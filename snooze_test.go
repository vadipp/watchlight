package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestSnoozeFreshRecoveryPersistenceAndAlerts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monitor.db")
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	m := testMonitor()
	m.Successes = 2
	m.Failures = 1
	record := func(ok bool) {
		t.Helper()
		if err := s.record(m, ok, 1, "", true); err != nil {
			t.Fatal(err)
		}
	}
	record(true)
	record(true)
	if err := s.hideUntilOK(m.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	hidden := func(want bool) {
		t.Helper()
		h, err := s.snoozesContext(context.Background())
		if err != nil || h[m.ID] != want {
			t.Fatal("hidden", h, err)
		}
	}
	hidden(true)
	record(true)
	hidden(true) // Old up state must not release the new snooze.
	record(false)
	hidden(true)
	var count int
	if err := s.QueryRow("SELECT count(*) FROM outbox").Scan(&count); err != nil || count != 0 {
		t.Fatal("muted alert", count, err)
	}
	record(true)
	hidden(true)
	record(true)
	hidden(false)
	if err := s.QueryRow("SELECT count(*) FROM outbox WHERE body LIKE '%visible again'").Scan(&count); err != nil || count != 1 {
		t.Fatal("recovery notice", count, err)
	}
	record(true)
	if err := s.QueryRow("SELECT count(*) FROM outbox").Scan(&count); err != nil || count != 1 {
		t.Fatal("duplicate notice", count, err)
	}
	record(false)
	if s.state(m.ID).Status != "down" {
		t.Fatal("normal failure disabled")
	}
	if err := s.QueryRow("SELECT count(*) FROM outbox").Scan(&count); err != nil || count != 2 {
		t.Fatal("normal alert", count, err)
	}
	if err := s.QueryRow("SELECT count(*) FROM results").Scan(&count); err != nil || count != 8 {
		t.Fatal("history lost", count, err)
	}
	if err := s.hideUntilOK(m.ID); err != nil {
		t.Fatal(err)
	}
	hidden(true)
}

func TestSnoozeVisibilityAndAccess(t *testing.T) {
	s := testStore(t)
	m := testMonitor()
	m.Public = true
	m.Successes = 1
	m.Group = "Services"
	if err := s.record(m, false, 0, "", false); err != nil {
		t.Fatal(err)
	}
	if err := s.hideUntilOK(m.ID); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SNOOZE_PASSWORD", "example-password")
	h := handler(Config{OwnerUser: "owner", OwnerPasswordEnv: "SNOOZE_PASSWORD", Monitors: []Monitor{m}, StatusPages: []StatusPage{{Slug: "main", Monitors: []string{m.ID}}}}, s)
	for _, tc := range []struct {
		path string
		auth bool
		code int
	}{
		{"/api/public", false, 200}, {"/api/status/main", false, 200},
		{"/api/public/" + m.ID, false, 404}, {"/api/status/main/" + m.ID, false, 404},
		{"/api/private", false, 401}, {"/api/private/" + m.ID, false, 401},
		{"/api/private", true, 200}, {"/api/private/" + m.ID, true, 200},
	} {
		req := httptest.NewRequest("GET", tc.path, nil)
		if tc.auth {
			req.SetBasicAuth("owner", "example-password")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.code {
			t.Fatalf("%s: %d", tc.path, rec.Code)
		}
		if tc.path == "/api/public" {
			var list []MonitorSummary
			if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) != 0 {
				t.Fatal("public snooze leaked", rec.Body.String())
			}
		}
		if tc.path == "/api/status/main" {
			var data struct{ Monitors []MonitorSummary }
			if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil || len(data.Monitors) != 0 {
				t.Fatal("page snooze leaked", rec.Body.String())
			}
		}
		if tc.path == "/api/private" && tc.auth {
			var list []MonitorSummary
			if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) != 1 || !list[0].Hidden {
				t.Fatal("owner snooze missing", rec.Body.String())
			}
		}
	}
	if err := s.record(m, true, 0, "", false); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/public/"+m.ID, nil))
	if rec.Code != 200 {
		t.Fatal("recovered monitor stayed hidden", rec.Code)
	}
}
