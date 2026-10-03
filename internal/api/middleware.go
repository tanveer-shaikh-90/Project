package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/tanveer-shaikh-90/seat-reservation/internal/metrics"
)

type ctxKey string

const (
	ctxUserID    ctxKey = "user_id"
	ctxIsAdmin   ctxKey = "is_admin"
	ctxRequestID ctxKey = "request_id"
)

// RequestID attaches a correlation id to every request and echoes it back.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Request-ID")
		if rid == "" {
			rid = uuid.NewString()
		}
		ctx := context.WithValue(r.Context(), ctxRequestID, rid)
		w.Header().Set("X-Request-ID", rid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// statusRecorder captures the response status for metrics and logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Observe records structured access logs plus Prometheus latency/count metrics.
func Observe(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: 200}
			next.ServeHTTP(rec, r)

			route := routeLabel(r)
			status := http.StatusText(rec.status)
			metrics.RequestDuration.WithLabelValues(route, r.Method, status).
				Observe(time.Since(start).Seconds())
			metrics.RequestsTotal.WithLabelValues(route, r.Method, status).Inc()

			rid, _ := r.Context().Value(ctxRequestID).(string)
			logger.Info("request",
				"request_id", rid,
				"method", r.Method,
				"route", route,
				"path", r.URL.Path,
				"status", rec.status,
				"duration_ms", time.Since(start).Milliseconds(),
			)
		})
	}
}

// routeLabel normalises paths so metrics cardinality stays bounded.
func routeLabel(r *http.Request) string {
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/shows/") && strings.HasSuffix(p, "/reserve"):
		return "POST /shows/{id}/reserve"
	case strings.HasPrefix(p, "/reservations/") && strings.HasSuffix(p, "/cancel"):
		return "POST /reservations/{id}/cancel"
	case strings.HasPrefix(p, "/shows/") && r.Method == http.MethodGet:
		return "GET /shows/{id}"
	default:
		return r.Method + " " + p
	}
}

// Auth derives identity strictly from the bearer token. Any user_id in the body
// is ignored, so a request can only ever act as the token's user.
// Token convention: "Bearer admin-<name>" is an admin; "Bearer <anything>" is a user.
func Auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			writeError(w, r, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}
		token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		if token == "" {
			writeError(w, r, http.StatusUnauthorized, "unauthorized", "empty bearer token")
			return
		}

		isAdmin := strings.HasPrefix(token, "admin-")
		userID := token // the token IS the identity

		ctx := context.WithValue(r.Context(), ctxUserID, userID)
		ctx = context.WithValue(ctx, ctxIsAdmin, isAdmin)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func userFromCtx(ctx context.Context) string {
	v, _ := ctx.Value(ctxUserID).(string)
	return v
}

func isAdminFromCtx(ctx context.Context) bool {
	v, _ := ctx.Value(ctxIsAdmin).(bool)
	return v
}

func requestIDFromCtx(ctx context.Context) string {
	v, _ := ctx.Value(ctxRequestID).(string)
	return v
}
