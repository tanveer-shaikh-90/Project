package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tanveer-shaikh-90/seat-reservation/internal/db"
)

// Domain errors. Each maps to a clean 4xx outcome in the HTTP layer — never a 5xx.
var (
	ErrInvalidRequest      = errors.New("invalid request")
	ErrShowNotFound        = errors.New("show not found")
	ErrSeatNotFound        = errors.New("one or more seats do not exist for this show")
	ErrSeatTaken           = errors.New("one or more seats are already taken")
	ErrPerUserLimit        = errors.New("per-user seat limit exceeded")
	ErrIdempotencyConflict = errors.New("idempotency key reused with a different request")
	ErrNoSeats             = errors.New("no seats requested")
	ErrReservationNotFound = errors.New("reservation not found")
	ErrNotOwner            = errors.New("reservation belongs to another user")
	ErrAlreadyCancelled    = errors.New("reservation already cancelled")
)

// Service holds the dependencies for reservation operations.
type Service struct {
	pool *db.Pool
}

func New(pool *db.Pool) *Service {
	return &Service{pool: pool}
}

// Show is the public representation of a show with its seat breakdown.
type Show struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	PricePaise   int64          `json:"price_paise"`
	PerUserLimit int            `json:"per_user_limit"`
	Seats        []SeatState    `json:"seats,omitempty"`
	Counts       map[string]int `json:"counts,omitempty"`
	TotalSeats   int            `json:"total_seats,omitempty"`
}

type SeatState struct {
	Label  string `json:"label"`
	Status string `json:"status"`
}

// Reservation is the public representation of a reservation.
type Reservation struct {
	ReservationID string   `json:"reservation_id"`
	ShowID        string   `json:"show_id"`
	UserID        string   `json:"user_id"`
	Seats         []string `json:"seats"`
	AmountPaise   int64    `json:"amount_paise"`
	Status        string   `json:"status"`
	Replayed      bool     `json:"-"`
}

// CreateShow inserts a show and its seats. Duplicate seat labels in the request
// are rejected by the UNIQUE (show_id, label) constraint.
func (s *Service) CreateShow(ctx context.Context, name string, seats []string, pricePaise int64, perUserLimit int) (*Show, error) {
	if perUserLimit == 0 {
		perUserLimit = 4
	}
	if strings.TrimSpace(name) == "" || len(name) > 200 || perUserLimit < 1 || perUserLimit > 10000 || pricePaise < 0 {
		return nil, ErrInvalidRequest
	}
	var err error
	seats, err = validateSeats(seats, 10000)
	if err != nil || pricePaise > math.MaxInt64/int64(perUserLimit) {
		return nil, ErrInvalidRequest
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var showID string
	err = tx.QueryRow(ctx,
		`INSERT INTO shows (name, price_paise, per_user_limit) VALUES ($1, $2, $3) RETURNING id`,
		name, pricePaise, perUserLimit,
	).Scan(&showID)
	if err != nil {
		return nil, fmt.Errorf("insert show: %w", err)
	}

	// Bulk insert seats via UNNEST for a single round-trip.
	_, err = tx.Exec(ctx,
		`INSERT INTO seats (show_id, label) SELECT $1, unnest($2::text[])`,
		showID, seats,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("duplicate seat labels in request: %w", ErrSeatTaken)
		}
		return nil, fmt.Errorf("insert seats: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	show := &Show{
		ID:           showID,
		Name:         name,
		PricePaise:   pricePaise,
		PerUserLimit: perUserLimit,
		TotalSeats:   len(seats),
		Counts:       map[string]int{"available": len(seats), "held": 0, "confirmed": 0},
	}
	for _, label := range seats {
		show.Seats = append(show.Seats, SeatState{Label: label, Status: "available"})
	}
	return show, nil
}

// GetShow returns the full seat breakdown and reconciliation counts.
func (s *Service) GetShow(ctx context.Context, showID string) (*Show, error) {
	parsed, err := uuid.Parse(showID)
	if err != nil {
		return nil, ErrInvalidRequest
	}
	showID = parsed.String()
	var show Show
	err = s.pool.QueryRow(ctx,
		`SELECT id, name, price_paise, per_user_limit FROM shows WHERE id = $1`,
		showID,
	).Scan(&show.ID, &show.Name, &show.PricePaise, &show.PerUserLimit)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrShowNotFound
	}
	if err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx,
		`SELECT label, status FROM seats WHERE show_id = $1 ORDER BY label`, showID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := map[string]int{"available": 0, "held": 0, "confirmed": 0}
	for rows.Next() {
		var st SeatState
		if err := rows.Scan(&st.Label, &st.Status); err != nil {
			return nil, err
		}
		show.Seats = append(show.Seats, st)
		counts[st.Status]++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	show.Counts = counts
	show.TotalSeats = counts["available"] + counts["held"] + counts["confirmed"]
	return &show, nil
}

// Reserve atomically confirms the requested seats for the user, or declines cleanly.
//
// Correctness mechanism:
//   - Seats are locked with SELECT ... FOR UPDATE, ordered by label (deterministic
//     lock order => no deadlock for multi-seat requests).
//   - The decision ("is it available?") and the mutation happen inside one
//     transaction holding those row locks, so no read-then-write race exists.
//   - Idempotency is enforced by the UNIQUE (user_id, idempotency_key) constraint.
//   - Behaviour is all-or-nothing: if any requested seat is unavailable, the whole
//     request declines and nothing is reserved.
func (s *Service) Reserve(ctx context.Context, userID, showID string, seats []string, idempotencyKey string) (*Reservation, error) {
	parsed, err := uuid.Parse(showID)
	if err != nil || userID == "" || len(userID) > 200 || strings.TrimSpace(idempotencyKey) == "" || len(idempotencyKey) > 200 {
		return nil, ErrInvalidRequest
	}
	showID = parsed.String()
	seats, err = validateSeats(seats, 10000)
	if err != nil {
		return nil, err
	}
	requestHash := hashRequest(showID, seats)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	if err := lockUser(ctx, tx, userID); err != nil {
		return nil, err
	}

	// 1. Idempotency fast-path: has this (user, key) already reserved?
	if existing, found, err := s.lookupByKey(ctx, tx, userID, idempotencyKey, requestHash); err != nil {
		return nil, err
	} else if found {
		return existing, nil
	}

	// 2. Load show (price + limit).
	var pricePaise int64
	var perUserLimit int
	err = tx.QueryRow(ctx,
		`SELECT price_paise, per_user_limit FROM shows WHERE id = $1`, showID,
	).Scan(&pricePaise, &perUserLimit)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrShowNotFound
	}
	if err != nil {
		return nil, err
	}

	// 3. Lock the requested seats in deterministic order.
	rows, err := tx.Query(ctx,
		`SELECT label, status FROM seats
		 WHERE show_id = $1 AND label = ANY($2)
		 ORDER BY label
		 FOR UPDATE`,
		showID, seats,
	)
	if err != nil {
		return nil, err
	}
	locked := map[string]string{}
	for rows.Next() {
		var label, status string
		if err := rows.Scan(&label, &status); err != nil {
			rows.Close()
			return nil, err
		}
		locked[label] = status
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Every requested seat must exist.
	if len(locked) != len(seats) {
		return nil, ErrSeatNotFound
	}
	// All-or-nothing: every requested seat must be available.
	for _, label := range seats {
		if locked[label] != "available" {
			return nil, ErrSeatTaken
		}
	}

	// 4. Per-user limit, counting seats the user already holds for this show.
	var alreadyHeld int
	err = tx.QueryRow(ctx,
		`SELECT count(*) FROM seats
		 WHERE show_id = $1 AND held_by = $2 AND status = 'confirmed'`,
		showID, userID,
	).Scan(&alreadyHeld)
	if err != nil {
		return nil, err
	}
	if alreadyHeld+len(seats) > perUserLimit {
		return nil, ErrPerUserLimit
	}

	reservationID := uuid.NewString()
	if pricePaise > math.MaxInt64/int64(len(seats)) {
		return nil, ErrInvalidRequest
	}
	amount := pricePaise * int64(len(seats))
	_, err = tx.Exec(ctx,
		`INSERT INTO reservations
		   (id, show_id, user_id, seats, amount_paise, status, idempotency_key, request_hash)
		 VALUES ($1, $2, $3, $4, $5, 'confirmed', $6, $7)`,
		reservationID, showID, userID, seats, amount, idempotencyKey, requestHash,
	)
	if err != nil {
		return nil, err
	}

	// 6. Flip the locked seats to confirmed.
	_, err = tx.Exec(ctx,
		`UPDATE seats
		   SET status = 'confirmed', held_by = $1, reservation_id = $2, updated_at = now()
		 WHERE show_id = $3 AND label = ANY($4)`,
		userID, reservationID, showID, seats,
	)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	return &Reservation{
		ReservationID: reservationID,
		ShowID:        showID,
		UserID:        userID,
		Seats:         seats,
		AmountPaise:   amount,
		Status:        "confirmed",
	}, nil
}

// Cancel releases the seats of a reservation back to available. Only the owner
// may cancel, and a cancel never touches seats confirmed to someone else.
func (s *Service) Cancel(ctx context.Context, userID, reservationID string) (*Reservation, error) {
	parsed, err := uuid.Parse(reservationID)
	if err != nil {
		return nil, ErrInvalidRequest
	}
	reservationID = parsed.String()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err := lockUser(ctx, tx, userID); err != nil {
		return nil, err
	}

	var r Reservation
	var status string
	err = tx.QueryRow(ctx,
		`SELECT id, show_id, user_id, seats, amount_paise, status
		   FROM reservations WHERE id = $1 FOR UPDATE`,
		reservationID,
	).Scan(&r.ReservationID, &r.ShowID, &r.UserID, &r.Seats, &r.AmountPaise, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrReservationNotFound
	}
	if err != nil {
		return nil, err
	}
	if r.UserID != userID {
		return nil, ErrNotOwner
	}
	if status == "cancelled" {
		r.Status = status
		return &r, nil
	}

	rows, err := tx.Query(ctx, `SELECT label FROM seats WHERE show_id = $1 AND reservation_id = $2 ORDER BY label FOR UPDATE`, r.ShowID, reservationID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Release only the seats still held by this reservation.
	_, err = tx.Exec(ctx,
		`UPDATE seats
		   SET status = 'available', held_by = NULL, reservation_id = NULL, updated_at = now()
		 WHERE show_id = $1 AND reservation_id = $2 AND status = 'confirmed'`,
		r.ShowID, reservationID,
	)
	if err != nil {
		return nil, err
	}

	_, err = tx.Exec(ctx,
		`UPDATE reservations SET status = 'cancelled' WHERE id = $1`, reservationID)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	r.Status = "cancelled"
	return &r, nil
}

// lookupByKey returns an existing reservation for (user, key) if present.
// Same key + same body => replay (returned). Same key + different body => conflict.
func (s *Service) lookupByKey(ctx context.Context, tx pgx.Tx, userID, key, requestHash string) (*Reservation, bool, error) {
	var r Reservation
	var storedHash, status string
	err := tx.QueryRow(ctx,
		`SELECT id, show_id, user_id, seats, amount_paise, status, request_hash
		   FROM reservations WHERE user_id = $1 AND idempotency_key = $2`,
		userID, key,
	).Scan(&r.ReservationID, &r.ShowID, &r.UserID, &r.Seats, &r.AmountPaise, &status, &storedHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if storedHash != requestHash {
		return nil, false, ErrIdempotencyConflict
	}
	r.Status = status
	r.Replayed = true
	return &r, true, nil
}

func validateSeats(in []string, maximum int) ([]string, error) {
	if len(in) == 0 || len(in) > maximum {
		return nil, ErrInvalidRequest
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if strings.TrimSpace(v) != v || v == "" || len(v) > 100 || strings.ContainsAny(v, "\x00\r\n") {
			return nil, ErrInvalidRequest
		}
		if _, ok := seen[v]; ok {
			return nil, ErrInvalidRequest
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out, nil
}

func hashRequest(showID string, sortedSeats []string) string {
	encoded, _ := json.Marshal(struct {
		ShowID string
		Seats  []string
	}{showID, sortedSeats})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func lockUser(ctx context.Context, tx pgx.Tx, userID string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO booking_users (user_id) VALUES ($1) ON CONFLICT DO NOTHING`, userID); err != nil {
		return err
	}
	var lockedUser string
	return tx.QueryRow(ctx, `SELECT user_id FROM booking_users WHERE user_id = $1 FOR UPDATE`, userID).Scan(&lockedUser)
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}
