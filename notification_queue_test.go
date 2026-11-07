package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func queueTransition(t *testing.T, s *Store, id string, ok bool, at int64) {
	t.Helper()
	m := testMonitor()
	m.ID = id
	m.Name = "Service " + id
	m.Failures = 1
	m.Successes = 1
	if err := s.recordAt(m, ok, 100, "Connection failed", true, time.Unix(at, 0)); err != nil {
		t.Fatal(err)
	}
}
func queueCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.QueryRow("SELECT count(*) FROM outbox").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func TestBatchesSummarizeTransitionsAndPreserveHistory(t *testing.T) {
	s := testStore(t)
	queueTransition(t, s, "recovered", false, 1000)
	queueTransition(t, s, "recovered", true, 1060)
	queueTransition(t, s, "recovered", false, 1100)
	queueTransition(t, s, "recovered", true, 1130)
	queueTransition(t, s, "down", false, 1140)
	b, err := s.notificationBatch(1141, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Delayed alerts", "Service recovered — failed and recovered", "total observed downtime 1m30s", "4 transitions", "Service down", "down since " + alertTime(1140)} {
		if !strings.Contains(b.Body, want) {
			t.Fatalf("missing %q: %s", want, b.Body)
		}
	}
	if strings.Index(b.Body, "Service down") > strings.Index(b.Body, "Service recovered") {
		t.Fatal("ongoing outage not prioritized")
	}
	if len(b.Seqs) != 5 || queueCount(t, s) != 5 {
		t.Fatal("events lost before acknowledgement")
	}
	if err = s.acknowledgeBatch(b, deliveryState{}); err != nil {
		t.Fatal(err)
	}
	var n int
	s.QueryRow("SELECT count(*) FROM incidents").Scan(&n)
	if n != 5 || queueCount(t, s) != 0 {
		t.Fatal("history altered or queue not acknowledged")
	}
	s.QueryRow("SELECT count(*) FROM outbox_events").Scan(&n)
	if n != 0 {
		t.Fatal("orphan structured events")
	}
}

func TestNormalBatchWindowAndDeliveredDownRecovery(t *testing.T) {
	s := testStore(t)
	queueTransition(t, s, "a", false, 1000)
	if b, err := s.notificationBatch(1009, false); err != nil || len(b.Seqs) != 0 {
		t.Fatal("batch sent early", err)
	}
	var messages []string
	worker := notificationWorker{store: s, lastProbe: 1000, call: func(_ context.Context, method string, v url.Values) error {
		if method != "sendMessage" {
			t.Fatal("unexpected probe")
		}
		messages = append(messages, v.Get("text"))
		return nil
	}}
	if err := worker.tick(context.Background(), 1010); err != nil {
		t.Fatal(err)
	}
	queueTransition(t, s, "a", true, 1060)
	if err := worker.tick(context.Background(), 1070); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || !strings.Contains(messages[1], "✅ Up") || strings.Contains(messages[1], "failed and recovered") {
		t.Fatalf("delivered down recovery: %v", messages)
	}
	if !strings.Contains(messages[1], "observed outage 1m0s") {
		t.Fatal(messages[1])
	}
}

func TestSharedCooldownSurvivesRestartAndRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	queueTransition(t, s, "a", false, 1000)
	calls := 0
	worker := notificationWorker{store: s, call: func(context.Context, string, url.Values) error { calls++; return errors.New("network failure") }}
	if err = worker.tick(context.Background(), 1010); err != nil {
		t.Fatal(err)
	}
	queueTransition(t, s, "a", true, 1011)
	queueTransition(t, s, "b", false, 1011)
	if err = worker.tick(context.Background(), 1011); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || queueCount(t, s) != 3 {
		t.Fatal("new event bypassed cooldown or failed send removed events")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	worker = notificationWorker{store: s, call: func(context.Context, string, url.Values) error {
		calls++
		return &telegramError{RateLimited: true, RetryAfter: 120}
	}}
	if err = worker.tick(context.Background(), 1011); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("restart lost cooldown")
	}
	if err = worker.tick(context.Background(), 1012); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("retry missing")
	}
	worker.call = func(_ context.Context, method string, v url.Values) error {
		calls++
		if method != "sendMessage" || !strings.Contains(v.Get("text"), "Delayed alerts") || !strings.Contains(v.Get("text"), "failed and recovered") {
			t.Fatal("missing recovery digest", method, v.Get("text"))
		}
		return nil
	}
	if err = worker.tick(context.Background(), 1131); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("Telegram retry_after ignored")
	}
	if err = worker.tick(context.Background(), 1132); err != nil {
		t.Fatal(err)
	}
	if calls != 3 || queueCount(t, s) != 0 {
		t.Fatal("recovery digest not acknowledged")
	}
	var state deliveryState
	s.getMeta("telegram_delivery", &state)
	if state.Attempts != 0 || state.DelayedSince != 0 || state.Next != 1135 {
		t.Fatalf("bad delivery state: %+v", state)
	}
}

func TestSplitBatchesUnicodeAndConcurrentRecovery(t *testing.T) {
	s := testStore(t)
	for i := 0; i < 100; i++ {
		queueTransition(t, s, fmt.Sprintf("%03d-🚀-%s", i, strings.Repeat("界", 30)), false, 1000)
	}
	sent := 0
	first := true
	worker := notificationWorker{store: s, call: func(_ context.Context, method string, v url.Values) error {
		if method != "sendMessage" {
			t.Fatal("unexpected probe")
		}
		body := v.Get("text")
		if !utf8.ValidString(body) || telegramLength(body) > 4096 {
			t.Fatal("invalid or oversized message")
		}
		sent++
		if first {
			first = false
			queueTransition(t, s, "000-🚀-"+strings.Repeat("界", 30), true, 1010)
		}
		return nil
	}}
	if err := worker.tick(context.Background(), 1010); err != nil {
		t.Fatal(err)
	}
	if queueCount(t, s) == 0 {
		t.Fatal("all events acknowledged by partial batch")
	}
	var recovery int
	s.QueryRow("SELECT count(*) FROM outbox_events WHERE status='up'").Scan(&recovery)
	if recovery != 1 {
		t.Fatal("concurrent recovery removed with earlier failure")
	}
	before := sent
	if err := worker.tick(context.Background(), 1011); err != nil {
		t.Fatal(err)
	}
	if sent != before {
		t.Fatal("split messages not paced")
	}
	for now := int64(1012); queueCount(t, s) > 0 && now < 1100; now += 3 {
		if err := worker.tick(context.Background(), now); err != nil {
			t.Fatal(err)
		}
	}
	if sent < 2 || queueCount(t, s) != 0 {
		t.Fatal("split batch failed to drain")
	}
}

func TestLegacyQueueAndAcknowledgementAtomicity(t *testing.T) {
	s := testStore(t)
	if _, err := s.Exec("INSERT INTO outbox(body,created) VALUES('Historical alert',1000)"); err != nil {
		t.Fatal(err)
	}
	b, err := s.notificationBatch(1010, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.Body, "Historical alert") {
		t.Fatal("legacy queue lost")
	}
	if _, err = s.Exec("CREATE TRIGGER reject_ack BEFORE DELETE ON outbox BEGIN SELECT RAISE(ABORT,'test failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err = s.acknowledgeBatch(b, deliveryState{Next: 1200}); err == nil {
		t.Fatal("expected failure")
	}
	if queueCount(t, s) != 1 {
		t.Fatal("ack failure lost events")
	}
	var n int
	s.QueryRow("SELECT count(*) FROM metadata WHERE key='telegram_delivery'").Scan(&n)
	if n != 0 {
		t.Fatal("partial acknowledgement")
	}
}

type telegramTransport func(*http.Request) (*http.Response, error)

func (f telegramTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestTelegramRateLimitResponse(t *testing.T) {
	for _, body := range []string{`{"ok":false,"error_code":429,"parameters":{"retry_after":77}}`, `invalid json`} {
		client := &http.Client{Transport: telegramTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
		})}
		err := telegramCallUsing(context.Background(), client, "synthetic-secret-token", "sendMessage", url.Values{})
		var te *telegramError
		if !errors.As(err, &te) || !te.RateLimited {
			t.Fatal("rate limit not recognized", err)
		}
		if strings.HasPrefix(body, "{") && te.RetryAfter != 77 {
			t.Fatal("missing server cooldown")
		}
		if strings.Contains(err.Error(), "synthetic-secret-token") {
			t.Fatal("token leaked")
		}
	}
}

func TestProbeFailureUsesSharedCooldownAndClearsWhenEmpty(t *testing.T) {
	s := testStore(t)
	calls := 0
	worker := notificationWorker{store: s, call: func(context.Context, string, url.Values) error { calls++; return errors.New("offline") }}
	if err := worker.tick(context.Background(), 1000); err != nil {
		t.Fatal(err)
	}
	if err := worker.tick(context.Background(), 1001); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("probe bypassed cooldown")
	}
	worker.call = func(context.Context, string, url.Values) error { calls++; return nil }
	if err := worker.tick(context.Background(), 1060); err != nil {
		t.Fatal(err)
	}
	var state deliveryState
	s.getMeta("telegram_delivery", &state)
	if calls != 2 || state.DelayedSince != 0 {
		t.Fatal("empty queue stayed delayed")
	}
}

func TestLegacySchemaUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE outbox(seq INTEGER PRIMARY KEY,body TEXT NOT NULL,attempts INTEGER NOT NULL DEFAULT 0,next INTEGER NOT NULL DEFAULT 0,created INTEGER NOT NULL); INSERT INTO outbox(body,created) VALUES('Existing alert',1000)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	queueTransition(t, s, "new", false, 1001)
	b, err := s.notificationBatch(1011, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Seqs) != 2 || !strings.Contains(b.Body, "Existing alert") || !strings.Contains(b.Body, "Service new") {
		t.Fatal(b)
	}
}

func TestStructuredEventInsertIsAtomic(t *testing.T) {
	s := testStore(t)
	if _, err := s.Exec("CREATE TRIGGER reject_event BEFORE INSERT ON outbox_events BEGIN SELECT RAISE(ABORT,'test failure'); END"); err != nil {
		t.Fatal(err)
	}
	m := testMonitor()
	m.Failures = 1
	if err := s.record(m, false, 100, "failed", true); err == nil {
		t.Fatal("expected event write failure")
	}
	for _, table := range []string{"outbox", "outbox_events", "states", "results", "incidents", "alert_states"} {
		var n int
		if err := s.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatal("partial transition", table, err)
		}
	}
}

func TestLongAlertIsTruncatedWithoutLosingEvent(t *testing.T) {
	s := testStore(t)
	m := testMonitor()
	m.Failures = 1
	m.Name = strings.Repeat("🚀", 5000)
	if err := s.recordAt(m, false, 100, "failed", true, time.Unix(1000, 0)); err != nil {
		t.Fatal(err)
	}
	b, err := s.notificationBatch(1010, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Seqs) != 1 || telegramLength(b.Body) > 4096 || !utf8.ValidString(b.Body) || !strings.Contains(b.Body, "…") {
		t.Fatal("oversized event lost or malformed")
	}
}

func TestNetworkBackoffIsBounded(t *testing.T) {
	var state deliveryState
	for i := 0; i < 20; i++ {
		state.failed(1000, errors.New("offline"))
	}
	if state.Next != 1060 {
		t.Fatal("network retry delay should cap at one minute", state)
	}
	state.failed(1000, &telegramError{RateLimited: true, RetryAfter: 900})
	if state.Next != 1900 {
		t.Fatal("server cooldown shortened", state)
	}
}

func TestFlappingStillDownUsesLatestOutageStart(t *testing.T) {
	s := testStore(t)
	queueTransition(t, s, "a", false, 1000)
	queueTransition(t, s, "a", true, 1060)
	queueTransition(t, s, "a", false, 1100)
	b, err := s.notificationBatch(1110, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.Body, "down since "+alertTime(1100)) || !strings.Contains(b.Body, "3 transitions") || strings.Contains(b.Body, "failed and recovered") {
		t.Fatal(b.Body)
	}
}

func TestCooldownStartsAfterRequestCompletes(t *testing.T) {
	s := testStore(t)
	queueTransition(t, s, "a", false, 1000)
	calls := 0
	worker := notificationWorker{store: s, finishedAt: func() time.Time { return time.Unix(1020, 900000000) }, call: func(context.Context, string, url.Values) error {
		calls++
		return &telegramError{RateLimited: true, RetryAfter: 120}
	}}
	if err := worker.tick(context.Background(), 1010); err != nil {
		t.Fatal(err)
	}
	var state deliveryState
	s.getMeta("telegram_delivery", &state)
	if state.Next != 1141 {
		t.Fatal("cooldown measured from request start", state)
	}
	if err := worker.tick(context.Background(), 1140); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("request bypassed server cooldown")
	}
}
