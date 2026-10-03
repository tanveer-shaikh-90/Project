package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
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
		if _, err := uuid.Parse(rid); err != nil {
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
			status := strconv.Itoa(rec.status)
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
	if route := chi.RouteContext(r.Context()); route != nil && route.RoutePattern() != "" {
		return route.RoutePattern()
	}
	return "unmatched"
}

type Authenticator struct {
	secret     []byte
	adminToken string
}

func NewAuthenticator(secret, adminToken string) (*Authenticator, error) {
	if len(secret) < 32 || len(adminToken) < 32 || secret == adminToken {
		return nil, errors.New("AUTH_SECRET and ADMIN_TOKEN must be distinct secrets of at least 32 bytes")
	}
	return &Authenticator{secret: []byte(secret), adminToken: adminToken}, nil
}

func (a *Authenticator) IssueToken(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	claims := jwt.RegisteredClaims{
		Issuer:    "seat-reservation",
		Audience:  jwt.ClaimStrings{"seat-reservation"},
		Subject:   uuid.NewString(),
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(24 * time.Hour)),
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(a.secret)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "token issuance failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": token, "user_id": claims.Subject, "expires_at": claims.ExpiresAt.Time})
}

func (a *Authenticator) Auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Fields(r.Header.Get("Authorization"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || len(parts[1]) > 4096 {
			writeError(w, r, http.StatusUnauthorized, "unauthorized", "valid bearer token required")
			return
		}
		isAdmin := subtle.ConstantTimeCompare([]byte(parts[1]), []byte(a.adminToken)) == 1
		userID := "admin"
		if !isAdmin {
			claims := &jwt.RegisteredClaims{}
			token, err := jwt.ParseWithClaims(parts[1], claims, func(token *jwt.Token) (any, error) {
				return a.secret, nil
			}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer("seat-reservation"), jwt.WithAudience("seat-reservation"), jwt.WithExpirationRequired())
			if err != nil || !token.Valid || claims.Subject == "" || len(claims.Subject) > 200 {
				writeError(w, r, http.StatusUnauthorized, "unauthorized", "invalid or expired token")
				return
			}
			userID = claims.Subject
		}

		ctx := context.WithValue(r.Context(), ctxUserID, userID)
		ctx = context.WithValue(ctx, ctxIsAdmin, isAdmin)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func BoundRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
		ctx, cancel := context.WithTimeout(r.Context(), 180*time.Second)
		defer cancel()
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
