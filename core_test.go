package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, e := openStore(filepath.Join(t.TempDir(), "test.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func testMonitor() Monitor {
	return Monitor{ID: "web", Name: "Website", Type: "http", Target: "https://example.com", Interval: "30s", Timeout: "10s", Failures: 3, Successes: 2}
}
func TestTransitionsAndOutboxSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, e := openStore(path)
	if e != nil {
		t.Fatal(e)
	}
	m := testMonitor()
	for i := 0; i < 2; i++ {
		if e = s.record(m, false, 10, "failure", true); e != nil {
			t.Fatal(e)
		}
	}
	if s.state(m.ID).Status != "unknown" {
		t.Fatal("threshold ignored")
	}
	if e = s.record(m, false, 10, "failure", true); e != nil {
		t.Fatal(e)
	}
	if s.state(m.ID).Status != "down" {
		t.Fatal("missing down transition")
	}
	s.Close()
	s, e = openStore(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	for i := 0; i < 2; i++ {
		if e = s.record(m, true, 5, "", true); e != nil {
			t.Fatal(e)
		}
	}
	if s.state(m.ID).Status != "up" {
		t.Fatal("missing recovery")
	}
	var count int
	var created int64
	if e = s.QueryRow("SELECT count(*),min(created) FROM outbox").Scan(&count, &created); e != nil {
		t.Fatal(e)
	}
	if count != 2 || created == 0 {
		t.Fatalf("want durable down+recovery, got %d", count)
	}
}
func TestPublicIsolationAndAuthentication(t *testing.T) {
	s := testStore(t)
	t.Setenv("TEST_PASS", "a-long-test-password")
	c := Config{OwnerUser: "owner", OwnerPasswordEnv: "TEST_PASS"}
	a := testMonitor()
	a.Public = true
	b := testMonitor()
	b.ID = "private-secret-id"
	b.Target = "https://private.internal/secret"
	c.Monitors = []Monitor{a, b}
	for _, m := range c.Monitors {
		if e := s.record(m, false, 1, "private diagnostic", false); e != nil {
			t.Fatal(e)
		}
	}
	h := handler(c, s)
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest("GET", "/api/public", nil))
	if r.Code != 200 || strings.Contains(r.Body.String(), "private") || strings.Contains(r.Body.String(), "example.com") {
		t.Fatalf("public data leak: %s", r.Body.String())
	}
	for _, path := range []string{"/dashboard", "/api/private", "/api/health"} {
		r = httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest("GET", path, nil))
		if r.Code != 401 {
			t.Fatalf("%s unprotected", path)
		}
	}
	req := httptest.NewRequest("GET", "/api/private", nil)
	req.SetBasicAuth("owner", "a-long-test-password")
	r = httptest.NewRecorder()
	h.ServeHTTP(r, req)
	if r.Code != 200 || !strings.Contains(r.Body.String(), b.ID) {
		t.Fatal("private view missing data")
	}
}
func TestHealthDetectsStalledChecks(t *testing.T) {
	s := testStore(t)
	t.Setenv("TEST_PASS", "a-long-test-password")
	m := testMonitor()
	c := Config{OwnerUser: "owner", OwnerPasswordEnv: "TEST_PASS", Monitors: []Monitor{m}}
	if e := s.record(m, true, 1, "", false); e != nil {
		t.Fatal(e)
	}
	h := handler(c, s)
	probe := func() int {
		r := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/health", nil)
		req.SetBasicAuth("owner", "a-long-test-password")
		h.ServeHTTP(r, req)
		return r.Code
	}
	if probe() != 200 {
		t.Fatal("fresh check unhealthy")
	}
	s.Exec("UPDATE states SET checked=?", time.Now().Add(-time.Hour).Unix())
	if probe() != 503 {
		t.Fatal("stalled scheduler not detected")
	}
}
func TestHeartbeatAuthenticationAndAge(t *testing.T) {
	s := testStore(t)
	t.Setenv("TOKEN", "test-heartbeat-token")
	m := testMonitor()
	m.Type = "heartbeat"
	m.TokenEnv = "TOKEN"
	m.MaxAge = "2m"
	h := handler(Config{Monitors: []Monitor{m}}, s)
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest("POST", "/api/heartbeat/web", nil))
	if r.Code != 401 {
		t.Fatal("unprotected heartbeat")
	}
	req := httptest.NewRequest("POST", "/api/heartbeat/web", nil)
	req.Header.Set("Authorization", "Bearer test-heartbeat-token")
	r = httptest.NewRecorder()
	h.ServeHTTP(r, req)
	if r.Code != 204 {
		t.Fatal(r.Code)
	}
	ok, _, _ := check(context.Background(), m, s)
	if !ok {
		t.Fatal("fresh heartbeat failed")
	}
	s.Exec("UPDATE heartbeats SET time=?", time.Now().Add(-3*time.Minute).Unix())
	ok, _, _ = check(context.Background(), m, s)
	if ok {
		t.Fatal("stale heartbeat accepted")
	}
}
func TestHTTPChecks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("healthy")) }))
	defer server.Close()
	m := testMonitor()
	m.Target = server.URL
	m.ExpectedStatus = 200
	m.BodyContains = "healthy"
	ok, _, _ := check(context.Background(), m, nil)
	if !ok {
		t.Fatal("valid check failed")
	}
	m.BodyContains = "missing"
	ok, _, _ = check(context.Background(), m, nil)
	if ok {
		t.Fatal("body check ignored")
	}
	m.BodyContains = ""
	m.ExpectedStatus = 204
	ok, _, _ = check(context.Background(), m, nil)
	if ok {
		t.Fatal("status check ignored")
	}
}
func TestRollupRetentionIsIdempotent(t *testing.T) {
	s := testStore(t)
	old := time.Now().Add(-10 * 24 * time.Hour).Truncate(time.Hour).Unix()
	s.Exec("INSERT INTO results VALUES('web',?,1,10),('web',?,0,20)", old, old+30)
	for i := 0; i < 2; i++ {
		if e := s.prune(7); e != nil {
			t.Fatal(e)
		}
	}
	var total, success int
	var latency float64
	if e := s.QueryRow("SELECT total,successes,latency_sum FROM hourly").Scan(&total, &success, &latency); e != nil {
		t.Fatal(e)
	}
	if total != 2 || success != 1 || latency != 30 {
		t.Fatal("bad aggregate")
	}
	var n int
	s.QueryRow("SELECT count(*) FROM results").Scan(&n)
	if n != 0 {
		t.Fatal("raw history not pruned")
	}
}
func TestConfigStrictValidation(t *testing.T) {
	for _, body := range []string{"unknown: true", "owner_password_env: PASS\nmonitors:\n  - id: test\n    type: tcp\n    target: missing-port", "owner_password_env: PASS\n---\nowner_password_env: OTHER"} {
		p := filepath.Join(t.TempDir(), "config.yaml")
		os.WriteFile(p, []byte(body), 0600)
		if _, e := loadConfig(p); e == nil {
			t.Fatalf("accepted invalid config: %s", body)
		}
	}
	for _, p := range []string{"config.example.yaml", "watchdog.example.yaml"} {
		if _, e := loadConfig(p); e != nil {
			t.Fatal(e)
		}
	}
}
func TestWatchdogSeesNotificationBacklog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Health{OK: true, Checked: time.Now().Unix(), Pending: 1, Oldest: time.Now().Add(-time.Hour).Unix()})
	}))
	defer server.Close()
	t.Setenv("PASS", "test-password")
	w := Watchdog{URL: server.URL, PasswordEnv: "PASS", MaxAge: "2m"}
	if inspectPrimary(context.Background(), w) == nil {
		t.Fatal("stalled notification delivery not detected")
	}
}
func TestChangedTargetResetsThresholds(t *testing.T) {
	s := testStore(t)
	m := testMonitor()
	for i := 0; i < 3; i++ {
		s.record(m, false, 1, "", false)
	}
	m.Target = "https://different.example.com"
	if e := s.record(m, true, 1, "", false); e != nil {
		t.Fatal(e)
	}
	if s.state(m.ID).Status != "unknown" {
		t.Fatal("new target inherited old state")
	}
}

func TestGETHeartbeatAndRecovery(t *testing.T) {
	s := testStore(t)
	t.Setenv("HA_TOKEN", "home-assistant-test-token")
	m := testMonitor()
	m.Type = "heartbeat"
	m.TokenEnv = "HA_TOKEN"
	m.MaxAge = "2m"
	m.Successes = 1
	for i := 0; i < m.Failures; i++ {
		ok, lat, msg := check(context.Background(), m, s)
		if e := s.record(m, ok, lat, msg, true); e != nil {
			t.Fatal(e)
		}
	}
	if s.state(m.ID).Status != "down" {
		t.Fatal("missing heartbeat did not fail")
	}
	h := handler(Config{Monitors: []Monitor{m}}, s)
	req := httptest.NewRequest("GET", "/api/heartbeat/web", nil)
	req.Header.Set("Authorization", "Bearer home-assistant-test-token")
	r := httptest.NewRecorder()
	h.ServeHTTP(r, req)
	if r.Code != 204 {
		t.Fatalf("GET heartbeat: %d", r.Code)
	}
	ok, lat, msg := check(context.Background(), m, s)
	if e := s.record(m, ok, lat, msg, true); e != nil {
		t.Fatal(e)
	}
	if s.state(m.ID).Status != "up" {
		t.Fatal("heartbeat did not recover")
	}
	var n int
	s.QueryRow("SELECT count(*) FROM outbox").Scan(&n)
	if n != 2 {
		t.Fatalf("want down+recovery alerts, got %d", n)
	}
	req = httptest.NewRequest("GET", "/api/heartbeat/web?token=home-assistant-test-token", nil)
	r = httptest.NewRecorder()
	h.ServeHTTP(r, req)
	if r.Code != 401 {
		t.Fatal("query token unexpectedly accepted")
	}
}
func TestIncidentAndNotificationAtomicity(t *testing.T) {
	s := testStore(t)
	m := testMonitor()
	m.Failures = 1
	if _, e := s.Exec("CREATE TRIGGER reject_outbox BEFORE INSERT ON outbox BEGIN SELECT RAISE(ABORT,'test failure'); END"); e != nil {
		t.Fatal(e)
	}
	if e := s.record(m, false, 1, "", true); e == nil {
		t.Fatal("expected failed transaction")
	}
	for _, table := range []string{"states", "results", "incidents", "outbox"} {
		var n int
		if e := s.QueryRow("SELECT count(*) FROM " + table).Scan(&n); e != nil || n != 0 {
			t.Fatalf("partial write in %s", table)
		}
	}
}
func BenchmarkRecord(b *testing.B) {
	s, e := openStore(filepath.Join(b.TempDir(), "bench.db"))
	if e != nil {
		b.Fatal(e)
	}
	defer s.Close()
	m := testMonitor()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if e := s.record(m, true, 10, "", false); e != nil {
			b.Fatal(e)
		}
	}
}

func TestWatchdogCanRunWithoutEmail(t *testing.T) {
	p := filepath.Join(t.TempDir(), "watchdog.yaml")
	if e := os.WriteFile(p, []byte("owner_password_env: PASS\nwatchdog:\n  url: https://status.example.com/api/health\n  password_env: PASS\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := loadConfig(p); e != nil {
		t.Fatal(e)
	}
}
