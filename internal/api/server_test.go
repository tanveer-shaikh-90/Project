package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tanveer-shaikh-90/seat-reservation/internal/db"
	"github.com/tanveer-shaikh-90/seat-reservation/internal/service"
)

const testAdmin = "test-admin-credential-not-for-production-12345"
const testSecret = "test-signing-secret-not-for-production-12345"

func testAuth(t *testing.T) *Authenticator {
	t.Helper()
	auth, err := NewAuthenticator(testSecret, testAdmin)
	if err != nil {
		t.Fatal(err)
	}
	return auth
}

func TestAuthentication(t *testing.T) {
	auth := testAuth(t)
	handler := auth.Auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"user_id": userFromCtx(r.Context())})
	}))
	issued := httptest.NewRecorder()
	auth.IssueToken(issued, httptest.NewRequest("POST", "/auth/token", nil))
	var tokenBody struct {
		Token  string
		UserID string `json:"user_id"`
	}
	if err := json.Unmarshal(issued.Body.Bytes(), &tokenBody); err != nil {
		t.Fatal(err)
	}
	claims := jwt.RegisteredClaims{Subject: "victim", Issuer: "seat-reservation", Audience: jwt.ClaimStrings{"seat-reservation"}, ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Hour))}
	expired, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(auth.secret)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		token  string
		status int
	}{
		{"", 401}, {"admin-attacker", 401}, {"victim", 401}, {expired, 401},
		{tokenBody.Token + "tampered", 401}, {tokenBody.Token, 200}, {testAdmin, 200},
	} {
		req := httptest.NewRequest("POST", "/", nil)
		req.Header.Set("Authorization", "Bearer "+test.token)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != test.status {
			t.Fatalf("auth status=%d want=%d", recorder.Code, test.status)
		}
		if test.token == tokenBody.Token && !strings.Contains(recorder.Body.String(), tokenBody.UserID) {
			t.Fatal("wrong subject")
		}
	}
}

func TestDecodeJSON(t *testing.T) {
	for _, body := range []string{`{"seats":["A1"]} {}`, `{"seats":`, `{"price_paise":1.5}`} {
		req := httptest.NewRequest("POST", "/", strings.NewReader(body))
		if err := decodeJSON(req, &createShowRequest{}); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}

type testResponse struct {
	status int
	body   map[string]any
	raw    string
}

func call(handler http.Handler, method, path, token string, body any) testResponse {
	encoded, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewReader(encoded))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	result := testResponse{status: recorder.Code, raw: recorder.Body.String()}
	_ = json.Unmarshal(recorder.Body.Bytes(), &result.body)
	return result
}

func requireStatus(t *testing.T, response testResponse, status int) {
	t.Helper()
	if response.status != status {
		t.Fatalf("status=%d want=%d body=%s", response.status, status, response.raw)
	}
}

func parallelCalls(count int, operation func(int) testResponse) []testResponse {
	results := make([]testResponse, count)
	var group sync.WaitGroup
	start := make(chan struct{})
	for index := range results {
		group.Add(1)
		go func(index int) { defer group.Done(); <-start; results[index] = operation(index) }(index)
	}
	close(start)
	group.Wait()
	return results
}

func TestPostgresContracts(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE") }()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	pool, err := db.New(ctx, parsed.String(), 25)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pool.Migrate(ctx); err != nil {
		t.Fatal("repeat migration:", err)
	}
	auth := testAuth(t)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	handler := NewServer(service.New(pool), pool, logger, auth).Router()
	newUser := func() (string, string) {
		response := call(handler, "POST", "/auth/token", "", nil)
		requireStatus(t, response, 201)
		return response.body["token"].(string), response.body["user_id"].(string)
	}
	create := func(labels []string) string {
		response := call(handler, "POST", "/shows", testAdmin, map[string]any{"name": "test", "seats": labels, "price_paise": 25000})
		requireStatus(t, response, 201)
		if len(response.body["seats"].([]any)) != len(labels) {
			t.Fatal("create missing seats")
		}
		return response.body["id"].(string)
	}
	reserve := func(show, token, key string, seats ...string) testResponse {
		return call(handler, "POST", "/shows/"+show+"/reserve", token, map[string]any{"seats": seats, "idempotency_key": key, "user_id": "spoofed-victim"})
	}

	t.Run("hot seat", func(t *testing.T) {
		show := create([]string{"A12"})
		tokens := make([]string, 100)
		for index := range tokens {
			tokens[index], _ = newUser()
		}
		results := parallelCalls(len(tokens), func(index int) testResponse { return reserve(show, tokens[index], uuid.NewString(), "A12") })
		created := 0
		for _, result := range results {
			if result.status == 201 {
				created++
			} else {
				requireStatus(t, result, 409)
				if result.body["error"] != "seat_taken" {
					t.Fatal(result.raw)
				}
			}
		}
		if created != 1 {
			t.Fatalf("winners=%d", created)
		}
	})

	t.Run("same key and limit", func(t *testing.T) {
		labels := make([]string, 20)
		for index := range labels {
			labels[index] = fmt.Sprintf("S%d", index)
		}
		show := create(labels)
		token, userID := newUser()
		key := uuid.NewString()
		results := parallelCalls(40, func(index int) testResponse { return reserve(show, token, key, "S0") })
		created := 0
		reservationID := results[0].body["reservation_id"]
		for _, result := range results {
			if result.status == 201 {
				created++
			} else {
				requireStatus(t, result, 200)
			}
			if result.body["reservation_id"] != reservationID || result.body["user_id"] != userID {
				t.Fatal("replay or identity mismatch")
			}
		}
		if created != 1 {
			t.Fatalf("creates=%d", created)
		}
		requireStatus(t, reserve(show, token, key, "S1"), 409)
		requireStatus(t, reserve(create([]string{"S0"}), token, key, "S0"), 409)
		results = parallelCalls(10, func(index int) testResponse { return reserve(show, token, uuid.NewString(), labels[index+1]) })
		created = 0
		for _, result := range results {
			if result.status == 201 {
				created++
			} else {
				requireStatus(t, result, 409)
				if result.body["error"] != "per_user_limit" {
					t.Fatal(result.raw)
				}
			}
		}
		if created != 3 {
			t.Fatalf("additional seats=%d want=3", created)
		}
	})

	t.Run("partial cancel rebook", func(t *testing.T) {
		show := create([]string{"A", "B", "C"})
		owner, _ := newUser()
		other, _ := newUser()
		key := uuid.NewString()
		booked := reserve(show, owner, key, "B", "A")
		requireStatus(t, booked, 201)
		path := "/reservations/" + booked.body["reservation_id"].(string) + "/cancel"
		requireStatus(t, reserve(show, other, uuid.NewString(), "A", "C"), 409)
		state := call(handler, "GET", "/shows/"+show, "", nil)
		if state.body["counts"].(map[string]any)["available"] != float64(1) {
			t.Fatal("partial mutation")
		}
		requireStatus(t, call(handler, "POST", path, other, nil), 403)
		requireStatus(t, call(handler, "POST", path, owner, nil), 200)
		requireStatus(t, reserve(show, other, uuid.NewString(), "A", "B"), 201)
		requireStatus(t, call(handler, "POST", path, owner, nil), 200)
		replayed := reserve(show, owner, key, "A", "B")
		requireStatus(t, replayed, 200)
		if replayed.body["status"] != "cancelled" {
			t.Fatal("cancelled key resurrected")
		}
		state = call(handler, "GET", "/shows/"+show, "", nil)
		if state.body["counts"].(map[string]any)["confirmed"] != float64(2) {
			t.Fatal("stale cancel released new booking")
		}
		restarted := NewServer(service.New(pool), pool, logger, auth).Router()
		snapshot := call(restarted, "GET", "/metrics", "", nil)
		requireStatus(t, snapshot, 200)
		if !strings.Contains(snapshot.raw, fmt.Sprintf("seats_available{show_id=%q} 1", show)) {
			t.Fatal("metrics drift:", snapshot.raw)
		}
	})

	t.Run("opposite order and cancel race", func(t *testing.T) {
		show := create([]string{"A", "B"})
		first, _ := newUser()
		second, _ := newUser()
		results := parallelCalls(2, func(index int) testResponse {
			if index == 0 {
				return reserve(show, first, "first", "A", "B")
			}
			return reserve(show, second, "second", "B", "A")
		})
		winner := 0
		if results[1].status == 201 {
			winner = 1
		}
		requireStatus(t, results[winner], 201)
		requireStatus(t, results[1-winner], 409)
		owner := []string{first, second}[winner]
		challenger := []string{first, second}[1-winner]
		path := "/reservations/" + results[winner].body["reservation_id"].(string) + "/cancel"
		results = parallelCalls(30, func(index int) testResponse {
			if index%2 == 0 {
				return call(handler, "POST", path, owner, nil)
			}
			return reserve(show, challenger, "new-key", "B", "A")
		})
		for _, result := range results {
			if result.status != 200 && result.status != 201 && result.status != 409 {
				t.Fatal(result.raw)
			}
		}
		final := reserve(show, challenger, "new-key", "A", "B")
		if final.status != 200 && final.status != 201 {
			t.Fatal(final.raw)
		}
	})

	t.Run("validation and dependency health", func(t *testing.T) {
		token, _ := newUser()
		requireStatus(t, call(handler, "GET", "/shows/not-a-uuid", "", nil), 400)
		requireStatus(t, call(handler, "POST", "/shows", token, map[string]any{}), 403)
		requireStatus(t, call(handler, "POST", "/shows", testAdmin, map[string]any{"name": "bad", "seats": []string{"A", "A"}}), 400)
		requireStatus(t, call(handler, "GET", "/health/ready", "", nil), 200)
		pool.Close()
		requireStatus(t, call(handler, "GET", "/health/live", "", nil), 200)
		requireStatus(t, call(handler, "GET", "/health/ready", "", nil), 503)
		if call(handler, "GET", "/metrics", "", nil).status == 200 {
			t.Fatal("metrics hid database failure")
		}
	})
}
