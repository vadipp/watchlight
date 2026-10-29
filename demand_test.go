package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSummariesDoNotReadHistory(t *testing.T) {
	s := testStore(t)
	m := testMonitor()
	m.Public = true
	m.Group = "Operations"
	if err := s.record(m, true, 10, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Exec("DROP TABLE results; DROP TABLE incidents"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIST_PASSWORD", "example-password")
	h := handler(Config{OwnerUser: "owner", OwnerPasswordEnv: "LIST_PASSWORD", Monitors: []Monitor{m}}, s)
	for _, path := range []string{"/api/public", "/api/private", "/api/private?window=7d"} {
		req := httptest.NewRequest("GET", path, nil)
		req.SetBasicAuth("owner", "example-password")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatal("summary queried history", rec.Code)
		}
		var list []map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"buckets", "history", "incidents", "check", "uptime", "samples", "window_start", "window_end", "latency_ms", "type"} {
			if _, exists := list[0][key]; exists {
				t.Fatal("heavy summary field", key)
			}
		}
		if len(list[0]["state"].(map[string]any)) != 1 {
			t.Fatal("summary exposed unused state details")
		}
	}
}

func TestDetailMembershipAndPrivacy(t *testing.T) {
	s := testStore(t)
	a := testMonitor()
	a.Public = true
	b := a
	b.ID = "other"
	hidden := a
	hidden.ID = "hidden"
	hidden.Public = false
	t.Setenv("DETAIL_ACCESS_PASSWORD", "example-password")
	h := handler(Config{OwnerUser: "owner", OwnerPasswordEnv: "DETAIL_ACCESS_PASSWORD", Monitors: []Monitor{a, b, hidden}, StatusPages: []StatusPage{{Slug: "main", Monitors: []string{a.ID}}}}, s)
	for _, tc := range []struct {
		path string
		auth bool
		code int
	}{
		{"/api/public/web", false, 200}, {"/api/status/main/web", false, 200},
		{"/api/public/hidden", false, 404}, {"/api/public/missing", false, 404},
		{"/api/status/main/other", false, 404}, {"/api/status/missing/web", false, 404},
		{"/api/private/hidden", false, 401}, {"/api/private/hidden", true, 200},
		{"/api/private/missing", true, 404},
	} {
		req := httptest.NewRequest("GET", tc.path, nil)
		if tc.auth {
			req.SetBasicAuth("owner", "example-password")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.code {
			t.Fatalf("%s got %d", tc.path, rec.Code)
		}
		if rec.Code == 200 && strings.Contains(rec.Body.String(), `"history"`) {
			t.Fatal("raw history retained")
		}
	}
}

func TestSQLiteRunningQueryCancellation(t *testing.T) {
	s := testStore(t)
	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		started := time.Now()
		var total int64
		err := s.QueryRowContext(ctx, `WITH RECURSIVE numbers(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM numbers WHERE x<1000000000) SELECT sum(x) FROM numbers`).Scan(&total)
		cancel()
		if err == nil || time.Since(started) > time.Second {
			t.Fatal("running SQLite query did not cancel", err)
		}
		// Cancellation must leave the sole database connection usable for checks.
		if err := s.QueryRow("SELECT 1").Scan(&total); err != nil || total != 1 {
			t.Fatal("connection unusable after cancellation", err)
		}
	}
}

func TestHTTP2CancellationReleasesDetailWait(t *testing.T) {
	s := testStore(t)
	m := testMonitor()
	m.Public = true
	held, err := s.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	started, finished := make(chan struct{}), make(chan error, 1)
	app := handler(Config{Monitors: []Monitor{m}}, s)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Error("expected HTTP/2")
		}
		close(started)
		app.ServeHTTP(w, r)
		finished <- r.Context().Err()
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/public/web?window=7d", nil)
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		res, _ := server.Client().Do(req)
		if res != nil {
			res.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request not started")
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("HTTP cancellation not propagated", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler kept waiting for database after cancel")
	}
	<-clientDone
}
