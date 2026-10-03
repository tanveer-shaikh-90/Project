package metrics

import (
	"context"
	"time"

	"github.com/tanveer-shaikh-90/seat-reservation/internal/db"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	DeclinedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "reservations_declined_total",
		Help: "Total number of reservations declined, by reason.",
	}, []string{"reason"})

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

type Snapshot struct {
	pool *db.Pool
	available, state, total, confirmed *prometheus.Desc
}

func NewSnapshot(pool *db.Pool) *Snapshot {
	return &Snapshot{
		pool: pool,
		available: prometheus.NewDesc("seats_available", "Available seats from a database snapshot.", []string{"show_id"}, nil),
		state: prometheus.NewDesc("seats_state", "Seats by current state.", []string{"show_id", "status"}, nil),
		total: prometheus.NewDesc("seats_total", "Physical seats in a show.", []string{"show_id"}, nil),
		confirmed: prometheus.NewDesc("reservations_confirmed_total", "Committed reservations, including subsequently cancelled ones.", nil, nil),
	}
}

func (s *Snapshot) Describe(ch chan<- *prometheus.Desc) {
	ch <- s.available
	ch <- s.state
	ch <- s.total
	ch <- s.confirmed
}

func (s *Snapshot) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := s.pool.Query(ctx, `SELECT show_id::text,
		count(*) FILTER (WHERE status='available'), count(*) FILTER (WHERE status='held'),
		count(*) FILTER (WHERE status='confirmed'), count(*) FROM seats GROUP BY show_id
		UNION ALL SELECT '', count(*), 0, 0, 0 FROM reservations`)
	if err != nil {
		ch <- prometheus.NewInvalidMetric(s.available, err)
		return
	}
	defer rows.Close()
	var snapshot []prometheus.Metric
	for rows.Next() {
		var showID string
		var available, held, confirmed, total float64
		if err := rows.Scan(&showID, &available, &held, &confirmed, &total); err != nil {
			ch <- prometheus.NewInvalidMetric(s.available, err)
			return
		}
		if showID == "" {
			snapshot = append(snapshot, prometheus.MustNewConstMetric(s.confirmed, prometheus.CounterValue, available))
			continue
		}
		snapshot = append(snapshot,
			prometheus.MustNewConstMetric(s.available, prometheus.GaugeValue, available, showID),
			prometheus.MustNewConstMetric(s.total, prometheus.GaugeValue, total, showID),
			prometheus.MustNewConstMetric(s.state, prometheus.GaugeValue, available, showID, "available"),
			prometheus.MustNewConstMetric(s.state, prometheus.GaugeValue, held, showID, "held"),
			prometheus.MustNewConstMetric(s.state, prometheus.GaugeValue, confirmed, showID, "confirmed"))
	}
	if err := rows.Err(); err != nil {
		ch <- prometheus.NewInvalidMetric(s.available, err)
		return
	}
	for _, metric := range snapshot {
		ch <- metric
	}
}
