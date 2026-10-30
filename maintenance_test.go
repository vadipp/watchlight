package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func maintenanceFixture(t *testing.T, s *Store, id, op string, start, end int64) {
	t.Helper()
	_, err := s.Exec("INSERT INTO maintenance(monitor_id,operation_id,reason,source,starts_at,expires_at) VALUES(?,?,?,?,?,?)", id, op, "Example deployment", "test", start, end)
	if err != nil {
		t.Fatal(err)
	}
}
func TestMaintenanceAPI(t *testing.T) {
	s := testStore(t)
	m := testMonitor()
	m.Public = true
	t.Setenv("MAINT_PASSWORD", "example-password")
	h := handler(Config{OwnerUser: "owner", OwnerPasswordEnv: "MAINT_PASSWORD", Monitors: []Monitor{m}}, s)
	call := func(method, path, body string, auth bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if auth {
			r.SetBasicAuth("owner", "example-password")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	path := "/api/maintenance/web/release-1"
	body := `{"reason":"Private deployment reason","source":"operator","duration_seconds":300}`
	for _, method := range []string{"GET", "PUT", "POST"} {
		p := path
		if method == "GET" {
			p = "/api/maintenance/web"
		}
		if method == "POST" {
			p += "/complete"
		}
		if w := call(method, p, body, false); w.Code != 401 {
			t.Fatalf("unprotected %s: %d", method, w.Code)
		}
	}
	w := call("PUT", path, body, true)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var first Maintenance
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	w = call("PUT", path, body, true)
	var second Maintenance
	json.Unmarshal(w.Body.Bytes(), &second)
	if w.Code != 200 || first.StartsAt != second.StartsAt || first.ExpiresAt != second.ExpiresAt {
		t.Fatal("retry extended window", w.Body.String())
	}
	if w = call("PUT", path, strings.Replace(body, "300", "301", 1), true); w.Code != 409 {
		t.Fatal("mutation accepted", w.Code)
	}
	for _, bad := range []string{`{}`, `{"reason":"x","source":"s","duration_seconds":86401}`, `{"reason":"x","source":"s","duration_seconds":10,"starts_at":1}`, body + body} {
		if w = call("PUT", path+"-bad", bad, true); w.Code != 400 {
			t.Fatal("invalid input accepted", w.Code)
		}
	}
	r := httptest.NewRequest("PUT", path+"-csrf", strings.NewReader(body))
	r.SetBasicAuth("owner", "example-password")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 415 {
		t.Fatal("cross-site write accepted")
	}
	for _, route := range []string{"/api/public/web", "/api/public"} {
		w = call("GET", route, "", false)
		if w.Code != 200 || strings.Contains(w.Body.String(), "Private deployment") || strings.Contains(w.Body.String(), "operator") {
			t.Fatal("public metadata leak", w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"maintenance`) {
			t.Fatal("public maintenance missing")
		}
	}
	w = call("GET", "/api/maintenance/web", "", true)
	if !strings.Contains(w.Body.String(), "Private deployment reason") {
		t.Fatal("audit missing")
	}
	w = call("POST", path+"/complete", "{}", true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var done Maintenance
	json.Unmarshal(w.Body.Bytes(), &done)
	if done.CompletedAt == nil {
		t.Fatal("completion missing")
	}
	w = call("POST", path+"/complete", "{}", true)
	var retry Maintenance
	json.Unmarshal(w.Body.Bytes(), &retry)
	if w.Code != 200 || *done.CompletedAt != *retry.CompletedAt {
		t.Fatal("completion is not idempotent")
	}
	w = call("PUT", path, body, true)
	json.Unmarshal(w.Body.Bytes(), &retry)
	if retry.CompletedAt == nil {
		t.Fatal("retry reopened operation")
	}
	if w = call("GET", "/api/maintenance/web?start=1&end=999999999", "", true); w.Code != 400 {
		t.Fatal("unbounded query")
	}
	if w = call("PUT", "/api/maintenance/missing/release", body, true); w.Code != 404 {
		t.Fatal("unknown monitor accepted")
	}
}
func TestMaintenanceAlertsSurviveRestartAndExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monitor.db")
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	m := testMonitor()
	m.Successes = 1
	m.Failures = 1
	record := func(ok bool) {
		t.Helper()
		if err := s.record(m, ok, 1, "example", true); err != nil {
			t.Fatal(err)
		}
	}
	count := func(want int) {
		t.Helper()
		var n int
		if err := s.QueryRow("SELECT count(*) FROM outbox").Scan(&n); err != nil || n != want {
			t.Fatalf("alerts=%d want=%d err=%v", n, want, err)
		}
	}
	record(true)
	now := time.Now().Unix()
	maintenanceFixture(t, s, m.ID, "release", now-1, now+300)
	record(false)
	count(0)
	if s.state(m.ID).Status != "down" {
		t.Fatal("actual state hidden")
	}
	s.Close()
	s, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	record(false)
	count(0)
	// Expire without a state transition: the next failed check must still alert.
	if _, err = s.Exec("UPDATE maintenance SET expires_at=?", now); err != nil {
		t.Fatal(err)
	}
	record(false)
	count(1)
	record(false)
	count(1)
	record(true)
	count(2)
	maintenanceFixture(t, s, m.ID, "release-2", now-1, now+300)
	record(false)
	record(true)
	count(2)
	if _, err = s.Exec("UPDATE maintenance SET expires_at=? WHERE operation_id='release-2'", now); err != nil {
		t.Fatal(err)
	}
	record(true)
	count(2)
	// A pre-existing incident is not erased by declaring maintenance.
	record(false)
	count(3)
	maintenanceFixture(t, s, m.ID, "release-3", now-1, now+300)
	record(true)
	count(3)
	s.Exec("UPDATE maintenance SET expires_at=? WHERE operation_id='release-3'", now)
	record(true)
	count(4)
}
func TestMaintenanceCoverageUnionAndPersistence(t *testing.T) {
	s := testStore(t)
	maintenanceFixture(t, s, "web", "one", 990, 1040)
	maintenanceFixture(t, s, "web", "two", 1020, 1080)
	maintenanceFixture(t, s, "web", "gap", 1120, 1160)
	for _, p := range []Point{{Time: 990, OK: true}, {Time: 1020, OK: false}, {Time: 1200, OK: true}} {
		if _, err := s.Exec("INSERT INTO results VALUES(?,?,?,?)", "web", p.Time, p.OK, 10); err != nil {
			t.Fatal(err)
		}
	}
	buckets, total, up, err := s.windowHistory(context.Background(), "web", 1000, 1240, 30)
	if err != nil {
		t.Fatal(err)
	}
	var good, bad, blue float64
	for _, b := range buckets {
		good += b.Up
		bad += b.Down
		blue += b.Maintenance
		if b.Up+b.Down+b.Maintenance > float64(b.End-b.Time) {
			t.Fatal("overlap", b)
		}
	}
	if good != 50 || bad != 0 || blue != 100 || total != 2 || up != 1 {
		t.Fatal(fmt.Sprint(good, bad, blue, total, up))
	}
	// Completed windows stop covering at their effective end; expiry stays auditable.
	if _, err = s.Exec("UPDATE maintenance SET completed_at=1030 WHERE operation_id='one'"); err != nil {
		t.Fatal(err)
	}
	records, err := s.maintenanceHistory(context.Background(), "web", 1000, 1240)
	if err != nil || len(records) != 3 || records[0].ExpiresAt != 1040 || records[0].end() != 1030 {
		t.Fatal(records, err)
	}
}

func TestMaintenanceAlertBaselineRespectsConfigurationReset(t *testing.T) {
	s := testStore(t)
	m := testMonitor()
	m.Failures = 1
	m.Successes = 2
	if err := s.record(m, false, 0, "failure", true); err != nil {
		t.Fatal(err)
	}
	m.Target = "https://example.net"
	if err := s.record(m, true, 0, "", true); err != nil {
		t.Fatal(err)
	}
	var n int
	s.QueryRow("SELECT count(*) FROM outbox").Scan(&n)
	if n != 1 || s.state(m.ID).Status != "unknown" {
		t.Fatal("configuration reset produced a spurious recovery", n)
	}
}

func TestMaintenanceKeepsSuccessfulCoverageGreen(t *testing.T) {
	for _, tc := range []struct {
		name             string
		samples          bool
		ok               bool
		green, red, blue float64
	}{
		{"successful", true, true, 240, 0, 0},
		{"failed", true, false, 0, 120, 120},
		{"unknown", false, false, 0, 0, 120},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			maintenanceFixture(t, s, "web", "release", 1040, 1160)
			if tc.samples {
				for at := int64(990); at < 1240; at += 30 {
					if _, err := s.Exec("INSERT INTO results VALUES(?,?,?,?)", "web", at, tc.ok, 0); err != nil {
						t.Fatal(err)
					}
				}
			}
			buckets, _, _, err := s.windowHistory(context.Background(), "web", 1000, 1240, 30)
			if err != nil {
				t.Fatal(err)
			}
			var green, red, blue float64
			for _, b := range buckets {
				green += b.Up
				red += b.Down
				blue += b.Maintenance
				if b.Maintenance < 0 || b.Up+b.Down+b.Maintenance > float64(b.End-b.Time) {
					t.Fatal("invalid coverage", b)
				}
			}
			if green != tc.green || red != tc.red || blue != tc.blue {
				t.Fatalf("green/red/blue=%v/%v/%v want %v/%v/%v", green, red, blue, tc.green, tc.red, tc.blue)
			}
		})
	}
}
