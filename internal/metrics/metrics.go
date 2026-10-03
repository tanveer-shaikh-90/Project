package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Prometheus metrics for the burst. These reconcile with the API state:
// ConfirmedTotal counts successful reservations, DeclinedTotal breaks declines
// down by reason, and SeatsAvailable tracks live availability per show.
var (
	ConfirmedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "reservations_confirmed_total",
		Help: "Total number of reservations confirmed.",
	})

	DeclinedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "reservations_declined_total",
		Help: "Total number of reservations declined, by reason.",
	}, []string{"reason"})

	SeatsAvailable = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "seats_available",
		Help: "Number of seats currently available, by show.",
	}, []string{"show_id"})

	RequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request latency by route and status.",
		Buckets: prometheus.DefBuckets,
	}, []string{"route", "method", "status"})

	RequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total HTTP requests by route, method and status.",
	}, []string{"route", "method", "status"})
)

// Decline reasons (also used as API error codes).
const (
	ReasonSeatTaken      = "seat_taken"
	ReasonPerUserLimit   = "per_user_limit"
	ReasonIdempotentBody = "idempotency_conflict"
	ReasonIdempotentHit  = "idempotent_replay"
)
