package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWatchdogHeartbeatDelivery(t *testing.T) {
	t.Setenv("TEST_HEARTBEAT_TOKEN", "private-test-token")
	for _, status := range []int{204, 200, 401, 302, 500} {
		destinationHit := false
		destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationHit = true }))
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer private-test-token" || r.URL.RawQuery != "" {
				t.Error("incorrect heartbeat request")
			}
			w.Header().Set("Location", destination.URL)
			w.WriteHeader(status)
		}))
		err := sendWatchdogHeartbeat(context.Background(), Watchdog{HeartbeatURL: server.URL, HeartbeatTokenEnv: "TEST_HEARTBEAT_TOKEN"})
		if (err == nil) != (status == 204) {
			t.Errorf("status %d: %v", status, err)
		}
		if destinationHit {
			t.Error("followed redirect")
		}
		server.Close()
		destination.Close()
	}
}

func TestWatchdogHeartbeatAfterPersistedUnhealthyCycle(t *testing.T) {
	t.Setenv("TEST_HEARTBEAT_TOKEN", "private-test-token")
	store := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sent := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(503)
			return
		}
		states := map[string]WatchState{}
		store.getMeta("watchdog", &states)
		if states["Primary monitor"].Failures != 1 {
			t.Error("heartbeat sent before failed observation persisted")
		}
		sent = true
		w.WriteHeader(204)
		cancel()
	}))
	defer server.Close()
	err := runWatchdog(ctx, Config{Watchdog: &Watchdog{URL: server.URL + "/health", Interval: "30s", HeartbeatURL: server.URL + "/heartbeat", HeartbeatTokenEnv: "TEST_HEARTBEAT_TOKEN"}}, store)
	if err != nil || !sent {
		t.Fatalf("heartbeat missing after unhealthy observation: %v", err)
	}
}

func TestWatchdogHeartbeatConfiguration(t *testing.T) {
	for _, fields := range []string{
		"  heartbeat_url: http://example.com/heartbeat\n  heartbeat_token_env: TOKEN\n",
		"  heartbeat_url: https://example.com/heartbeat?token=secret\n  heartbeat_token_env: TOKEN\n",
		"  heartbeat_url: https://user:pass@example.com/heartbeat\n  heartbeat_token_env: TOKEN\n",
		"  heartbeat_url: https://example.com/heartbeat\n",
		"  heartbeat_token_env: TOKEN\n",
		"  heartbeat_url: https://example.com/heartbeat\n  heartbeat_token_env: TOKEN\n",
	} {
		file := filepath.Join(t.TempDir(), "config.yaml")
		os.WriteFile(file, []byte("owner_password_env: PASS\nwatchdog:\n  url: https://example.com/health\n  password_env: PASS\n"+fields), 0600)
		_, err := loadConfig(file)
		valid := strings.Contains(fields, "heartbeat_url: https://example.com/heartbeat\n  heartbeat_token_env: TOKEN")
		if (err == nil) != valid {
			t.Errorf("fields %q: %v", fields, err)
		}
	}
}
