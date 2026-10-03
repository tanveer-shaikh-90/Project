package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/tanveer-shaikh-90/seat-reservation/internal/db"
	"github.com/tanveer-shaikh-90/seat-reservation/internal/metrics"
	"github.com/tanveer-shaikh-90/seat-reservation/internal/service"
)

// Server wires the HTTP routes to the service.
type Server struct {
	svc    *service.Service
	pool   *db.Pool
	logger *slog.Logger
}

func NewServer(svc *service.Service, pool *db.Pool, logger *slog.Logger) *Server {
	return &Server{svc: svc, pool: pool, logger: logger}
}

// Router builds the HTTP routing tree.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(RequestID)
	r.Use(Observe(s.logger))

	// Health & metrics are unauthenticated.
	r.Get("/health/live", s.handleLive)
	r.Get("/health/ready", s.handleReady)
	r.Handle("/metrics", promhttp.Handler())

	// Everything below requires a bearer token.
	r.Group(func(r chi.Router) {
		r.Use(Auth)
		r.Post("/shows", s.handleCreateShow)
		r.Get("/shows/{id}", s.handleGetShow)
		r.Post("/shows/{id}/reserve", s.handleReserve)
		r.Post("/reservations/{id}/cancel", s.handleCancel)
	})

	return r
}

// ---- Health ----

func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "alive"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if err := s.pool.Ready(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "not_ready",
			"reason": "database unreachable",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// ---- Shows ----

type createShowRequest struct {
	Name         string   `json:"name"`
	Seats        []string `json:"seats"`
	PricePaise   int64    `json:"price_paise"`
	PerUserLimit int      `json:"per_user_limit"`
}

func (s *Server) handleCreateShow(w http.ResponseWriter, r *http.Request) {
	if !isAdminFromCtx(r.Context()) {
		writeError(w, r, http.StatusForbidden, "forbidden", "admin token required to create a show")
		return
	}
	var req createShowRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.Name == "" || len(req.Seats) == 0 {
		writeError(w, r, http.StatusBadRequest, "bad_request", "name and seats are required")
		return
	}
	if req.PricePaise < 0 {
		writeError(w, r, http.StatusBadRequest, "bad_request", "price_paise must be >= 0")
		return
	}

	show, err := s.svc.CreateShow(r.Context(), req.Name, req.Seats, req.PricePaise, req.PerUserLimit)
	if err != nil {
		s.mapError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, show)
}

func (s *Server) handleGetShow(w http.ResponseWriter, r *http.Request) {
	showID := chi.URLParam(r, "id")
	show, err := s.svc.GetShow(r.Context(), showID)
	if err != nil {
		s.mapError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, show)
}

// ---- Reservations ----

type reserveRequest struct {
	Seats          []string `json:"seats"`
	IdempotencyKey string   `json:"idempotency_key"`
}

func (s *Server) handleReserve(w http.ResponseWriter, r *http.Request) {
	showID := chi.URLParam(r, "id")
	userID := userFromCtx(r.Context())

	var req reserveRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	// Idempotency key may arrive in the header or the body; header wins.
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		key = req.IdempotencyKey
	}
	if key == "" {
		writeError(w, r, http.StatusBadRequest, "bad_request", "idempotency_key is required (header or body)")
		return
	}
	if len(req.Seats) == 0 {
		writeError(w, r, http.StatusBadRequest, "bad_request", "seats are required")
		return
	}

	res, err := s.svc.Reserve(r.Context(), userID, showID, req.Seats, key)
	if err != nil {
		s.mapError(w, r, err)
		return
	}

	if res.Replayed {
		metrics.DeclinedTotal.WithLabelValues(metrics.ReasonIdempotentHit).Inc()
		writeJSON(w, http.StatusOK, res)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	reservationID := chi.URLParam(r, "id")
	userID := userFromCtx(r.Context())

	res, err := s.svc.Cancel(r.Context(), userID, reservationID)
	if err != nil {
		s.mapError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// mapError translates domain errors into clean HTTP status codes. Declines are
// 4xx domain outcomes and are counted by reason; only truly unexpected errors 5xx.
func (s *Server) mapError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, service.ErrShowNotFound):
		writeError(w, r, http.StatusNotFound, "show_not_found", err.Error())
	case errors.Is(err, service.ErrSeatNotFound):
		writeError(w, r, http.StatusNotFound, "seat_not_found", err.Error())
	case errors.Is(err, service.ErrReservationNotFound):
		writeError(w, r, http.StatusNotFound, "reservation_not_found", err.Error())
	case errors.Is(err, service.ErrSeatTaken):
		metrics.DeclinedTotal.WithLabelValues(metrics.ReasonSeatTaken).Inc()
		writeError(w, r, http.StatusConflict, metrics.ReasonSeatTaken, err.Error())
	case errors.Is(err, service.ErrPerUserLimit):
		metrics.DeclinedTotal.WithLabelValues(metrics.ReasonPerUserLimit).Inc()
		writeError(w, r, http.StatusConflict, metrics.ReasonPerUserLimit, err.Error())
	case errors.Is(err, service.ErrIdempotencyConflict):
		metrics.DeclinedTotal.WithLabelValues(metrics.ReasonIdempotentBody).Inc()
		writeError(w, r, http.StatusConflict, metrics.ReasonIdempotentBody, err.Error())
	case errors.Is(err, service.ErrNotOwner):
		writeError(w, r, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, service.ErrAlreadyCancelled):
		writeError(w, r, http.StatusConflict, "already_cancelled", err.Error())
	case errors.Is(err, service.ErrNoSeats):
		writeError(w, r, http.StatusBadRequest, "bad_request", err.Error())
	default:
		s.logger.Error("internal error",
			"request_id", requestIDFromCtx(r.Context()), "error", err.Error())
		writeError(w, r, http.StatusInternalServerError, "internal_error", "unexpected error")
	}
}

// ---- helpers ----

// decodeJSON intentionally ignores unknown fields: a spoofed "user_id" in the
// body is silently dropped because identity comes only from the auth token.
func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

type errorBody struct {
	Error     string `json:"error"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	writeJSON(w, status, errorBody{
		Error:     code,
		Message:   msg,
		RequestID: requestIDFromCtx(r.Context()),
	})
}
