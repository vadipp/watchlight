package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOptionalLatency(t *testing.T) {
	s := testStore(t)
	m := testMonitor()
	m.Public = true
	if err := s.record(m, false, 123, "failure", true); err != nil {
		t.Fatal(err)
	}
	fp := fingerprint(m)
	disabled := false
	m.RecordLatency = &disabled
	if fingerprint(m) != fp {
		t.Fatal("latency setting changes incident fingerprint")
	}
	t.Setenv("TEST_LATENCY_PASSWORD", "test-password")
	c := Config{OwnerPasswordEnv: "TEST_LATENCY_PASSWORD", Monitors: []Monitor{m}}
	// Old measurements must also be hidden immediately after disabling.
	for _, path := range []string{"/api/public/web", "/api/private/web"} {
		r := httptest.NewRequest("GET", path, nil)
		r.SetBasicAuth("", "test-password")
		w := httptest.NewRecorder()
		handler(c, s).ServeHTTP(w, r)
		var view View
		if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		if view.LatencyMS != nil || view.State.Latency != 0 {
			t.Fatalf("timing exposed: %s", w.Body.String())
		}
		for _, b := range view.Buckets {
			if b.Latency != 0 {
				t.Fatal("bucket latency exposed")
			}
		}
	}
	if err := s.record(m, false, 456, "failure", true); err != nil {
		t.Fatal(err)
	}
	if s.state(m.ID).Latency != 0 || s.state(m.ID).Fail != 2 {
		t.Fatal("measurement persisted or failure counter reset")
	}
	enabled := true
	m.RecordLatency = &enabled
	if err := s.record(m, false, 789, "failure", true); err != nil {
		t.Fatal(err)
	}
	if s.state(m.ID).Latency != 789 || s.state(m.ID).Status != "down" {
		t.Fatal("latency enable or threshold failed")
	}
}

func TestFailedChecksExcludedFromLatency(t *testing.T) {
	s := testStore(t)
	m := testMonitor()
	m.Public = true
	now := time.Now()
	// One mixed bucket and one bucket containing only failures.
	for _, p := range []struct {
		age     time.Duration
		ok      bool
		latency float64
	}{
		{10 * time.Minute, true, 100}, {10*time.Minute - time.Second, false, 10000},
		{10*time.Minute - 2*time.Second, true, 120}, {0, false, 10000},
	} {
		if err := s.recordAt(m, p.ok, p.latency, "", false, now.Add(-p.age)); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"/api/public/web", "/api/private/web"} {
		t.Setenv("LATENCY_TEST_PASSWORD", "test-password")
		r := httptest.NewRequest("GET", path, nil)
		r.SetBasicAuth("", "test-password")
		w := httptest.NewRecorder()
		handler(Config{OwnerPasswordEnv: "LATENCY_TEST_PASSWORD", Monitors: []Monitor{m}}, s).ServeHTTP(w, r)
		var v View
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		if !v.LatencyEnabled || v.LatencyMS != nil || v.State.Latency != 0 {
			t.Fatalf("failed current check exposed latency: %+v", v)
		}
		if v.Samples != 4 || v.Uptime == nil || *v.Uptime != 50 {
			t.Fatalf("failure history changed: %+v", v)
		}
		var mixed, failed bool
		for _, b := range v.Buckets {
			if b.Checks == 3 {
				mixed = true
				if b.Latency != 110 || b.Failures != 1 {
					t.Fatalf("mixed bucket: %+v", b)
				}
			}
			if b.Checks == 1 {
				failed = true
				if b.Latency != 0 || b.Failures != 1 {
					t.Fatalf("failed bucket: %+v", b)
				}
			}
		}
		if !mixed || !failed {
			t.Fatal("missing buckets")
		}
	}
	if err := s.record(m, true, 115, "", false); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	handler(Config{Monitors: []Monitor{m}}, s).ServeHTTP(w, httptest.NewRequest("GET", "/api/public/web", nil))
	var v View
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.LatencyMS == nil || *v.LatencyMS != 115 {
		t.Fatal("successful latency missing")
	}
}
