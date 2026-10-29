package main

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestContainerReportsPersistRecoverAndStayPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.db")
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	t.Setenv("REPORT_TOKEN", "test-heartbeat-token")
	t.Setenv("REPORT_PASSWORD", "test-owner-password")
	m := Monitor{ID: "stack", Name: "Example stack", Type: "heartbeat", TokenEnv: "REPORT_TOKEN", Interval: "30s", Timeout: "5s", MaxAge: "5m", Failures: 3, Successes: 1, Public: true}
	c := Config{OwnerUser: "owner", OwnerPasswordEnv: "REPORT_PASSWORD", Monitors: []Monitor{m}, StatusPages: []StatusPage{{Slug: "example", Monitors: []string{m.ID}}}}
	send := func(body, token string) int {
		r := httptest.NewRequest("POST", "/api/heartbeat/stack", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler(c, s).ServeHTTP(w, r)
		return w.Code
	}
	report := `{"failures":[{"name":"example-worker-1","status":"unhealthy"}]}`
	if send(report, "wrong") != 401 {
		t.Fatal("report accepted without authentication")
	}
	if send(report, "test-heartbeat-token") != 204 {
		t.Fatal("report rejected")
	}
	s.Close()
	s, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		ok, lat, detail := check(context.Background(), m, s)
		if ok || !strings.Contains(detail, "example-worker-1: unhealthy") {
			t.Fatal("failure detail lost")
		}
		if err = s.record(m, ok, lat, detail, false); err != nil {
			t.Fatal(err)
		}
		if i < 2 && s.state(m.ID).Status == "down" {
			t.Fatal("threshold bypassed")
		}
	}
	if s.state(m.ID).Status != "down" {
		t.Fatal("failure not applied")
	}
	for _, route := range []string{"/api/public/stack", "/api/status/example/stack", "/api/private/stack"} {
		r := httptest.NewRequest("GET", route, nil)
		if route == "/api/private/stack" {
			r.SetBasicAuth("owner", "test-owner-password")
		}
		w := httptest.NewRecorder()
		handler(c, s).ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("route %s failed", route)
		}
		if strings.Contains(w.Body.String(), "example-worker-1") != (route == "/api/private/stack") {
			t.Fatalf("report visibility incorrect on %s", route)
		}
	}
	if _, err = s.Exec("UPDATE heartbeats SET time=? WHERE id=?", time.Now().Add(-10*time.Minute).Unix(), m.ID); err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := check(context.Background(), m, s); ok {
		t.Fatal("stale report healthy")
	}
	if send("", "test-heartbeat-token") != 204 {
		t.Fatal("legacy heartbeat rejected")
	}
	ok, lat, detail := check(context.Background(), m, s)
	if !ok || detail != "" {
		t.Fatal("recovery did not clear failure")
	}
	if err = s.record(m, ok, lat, detail, false); err != nil || s.state(m.ID).Status != "up" {
		t.Fatal("recovery failed")
	}
}

func TestHeartbeatReportsRejectUnsafePayloads(t *testing.T) {
	s := testStore(t)
	t.Setenv("REPORT_TOKEN", "test-token")
	c := Config{Monitors: []Monitor{{ID: "stack", Type: "heartbeat", TokenEnv: "REPORT_TOKEN"}}}
	for _, body := range []string{
		`{"failures":[{"name":"<script>","status":"exited"}]}`,
		`{"failures":[{"name":"worker","status":"secret raw output"}]}`,
		`{"failures":[],"output":"secret"}`, `{"failures":[]} {}`, strings.Repeat("x", 9000),
	} {
		r := httptest.NewRequest("POST", "/api/heartbeat/stack", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer test-token")
		w := httptest.NewRecorder()
		handler(c, s).ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("unsafe report returned %d", w.Code)
		}
	}
	var n int
	s.QueryRow("SELECT count(*) FROM heartbeats").Scan(&n)
	if n != 0 {
		t.Fatal("invalid reports changed arrival")
	}
}
