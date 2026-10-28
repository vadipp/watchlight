package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
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
	for _, path := range []string{"/api/public", "/api/private"} {
		r := httptest.NewRequest("GET", path, nil)
		r.SetBasicAuth("", "test-password")
		w := httptest.NewRecorder()
		handler(c, s).ServeHTTP(w, r)
		var views []View
		if err := json.Unmarshal(w.Body.Bytes(), &views); err != nil {
			t.Fatal(err)
		}
		if len(views) != 1 || views[0].LatencyMS != nil || views[0].State.Latency != 0 || views[0].History[0].Latency != 0 {
			t.Fatalf("timing exposed: %s", w.Body.String())
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
