package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScopedAPITokens(t *testing.T) {
	s := testStore(t)
	m := testMonitor()
	other := m
	other.ID = "other"
	heartbeat := Monitor{ID: "worker", Type: "heartbeat", TokenEnv: "HEARTBEAT_SECRET", Interval: "30s", Timeout: "5s", MaxAge: "2m", Failures: 1, Successes: 1}
	t.Setenv("OWNER_SECRET", "example-owner-password")
	t.Setenv("BACKUP_SECRET", strings.Repeat("b", 40))
	t.Setenv("READER_SECRET", strings.Repeat("r", 40))
	t.Setenv("HEARTBEAT_SECRET", strings.Repeat("h", 40))
	c := Config{OwnerUser: "owner", OwnerPasswordEnv: "OWNER_SECRET", Monitors: []Monitor{m, other, heartbeat}, APITokens: []APIToken{
		{Name: "backup", TokenEnv: "BACKUP_SECRET", Monitors: []string{"web"}, Scopes: []string{"maintenance:read", "maintenance:write", "monitor:read"}, OperationPrefix: "backup-", MaxDurationSeconds: 900},
		{Name: "reader", TokenEnv: "READER_SECRET", Monitors: []string{"web"}, Scopes: []string{"monitor:read"}},
	}}
	call := func(method, path, body, token string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if token == "owner" {
			r.SetBasicAuth("owner", "example-owner-password")
		} else if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		handler(c, s).ServeHTTP(w, r)
		return w
	}
	body := `{"reason":"Application backup","source":"backup","duration_seconds":900}`
	backup, reader := os.Getenv("BACKUP_SECRET"), os.Getenv("READER_SECRET")
	for _, tc := range []struct {
		method, path, body, token string
		code                      int
	}{
		{"PUT", "/api/maintenance/web/backup-1", body, backup, 201},
		{"PUT", "/api/maintenance/web/backup-1", body, backup, 200},
		{"GET", "/api/maintenance/web", "", backup, 200},
		{"GET", "/api/monitors/web/status", "", backup, 200},
		{"GET", "/api/monitors/web/status", "", reader, 200},
		{"GET", "/api/maintenance/web", "", reader, 403},
		{"PUT", "/api/maintenance/web/backup-2", body, reader, 403},
		{"PUT", "/api/maintenance/other/backup-1", body, backup, 403},
		{"GET", "/api/maintenance/other", "", backup, 403},
		{"GET", "/api/monitors/other/status", "", backup, 403},
		{"PUT", "/api/maintenance/web/development-1", body, backup, 403},
		{"PUT", "/api/maintenance/web/backup-", body, backup, 403},
		{"PUT", "/api/maintenance/web/backup-2", strings.Replace(body, "900", "901", 1), backup, 403},
		{"POST", "/api/maintenance/web/development-1/complete", "{}", backup, 403},
		{"POST", "/api/maintenance/web/backup-1/complete", "{}", backup, 200},
		{"POST", "/api/heartbeat/worker", "", backup, 401},
		{"GET", "/api/private", "", backup, 401},
		{"GET", "/api/private/web", "", backup, 401},
		{"GET", "/api/health", "", backup, 401},
		{"GET", "/dashboard", "", backup, 401},
		{"GET", "/api/monitors/web/status", "", os.Getenv("HEARTBEAT_SECRET"), 401},
		{"GET", "/api/maintenance/web", "", "bad-token", 401},
		{"GET", "/api/maintenance/web", "", "", 401},
		{"PUT", "/api/maintenance/other/development-1", body, "owner", 201},
	} {
		if w := call(tc.method, tc.path, tc.body, tc.token); w.Code != tc.code {
			t.Fatalf("%s %s: got %d want %d: %s", tc.method, tc.path, w.Code, tc.code, w.Body.String())
		}
	}
	// Revocation removes the grant, and response fields are an explicit allowlist.
	c.APITokens = nil
	if w := call("GET", "/api/monitors/web/status", "", backup); w.Code != 401 {
		t.Fatal("revoked token accepted")
	}
	w := call("GET", "/api/monitors/web/status", "", "owner")
	var status map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || len(status) != 5 || status["status"] != "unknown" {
		t.Fatalf("unsafe or invalid recovery response: %s", w.Body.String())
	}
}

func TestAPITokenConfigAndSecrets(t *testing.T) {
	base := `owner_password_env: OWNER_SECRET
monitors:
  - id: web
    type: http
    target: https://example.com/
api_tokens:
  - name: backup
    token_env: BACKUP_SECRET
    monitors: [web]
    scopes: [maintenance:read, maintenance:write, monitor:read]
    operation_prefix: backup-
    max_duration_seconds: 900
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	for _, tc := range []struct{ from, to string }{
		{"monitors: [web]", "monitors: ['*']"},
		{"monitors: [web]", "monitors: [web, web]"},
		{"scopes: [maintenance:read, maintenance:write, monitor:read]", "scopes: [owner]"},
		{"scopes: [maintenance:read, maintenance:write, monitor:read]", "scopes: []"},
		{"operation_prefix: backup-", "operation_prefix: ''"},
		{"max_duration_seconds: 900", "max_duration_seconds: 86401"},
		{"token_env: BACKUP_SECRET", "token_env: OWNER_SECRET"},
	} {
		os.WriteFile(path, []byte(strings.Replace(base, tc.from, tc.to, 1)), 0600)
		if _, err := loadConfig(path); err == nil {
			t.Fatalf("accepted %s", tc.to)
		}
	}
	os.WriteFile(path, []byte(base), 0600)
	c, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OWNER_SECRET", strings.Repeat("o", 40))
	for _, value := range []string{"", "short", strings.Repeat("o", 40), strings.Repeat("x", 40) + "\n"} {
		t.Setenv("BACKUP_SECRET", value)
		if validateAPITokenSecrets(c) == nil {
			t.Fatal("unsafe token accepted")
		}
	}
	t.Setenv("BACKUP_SECRET", strings.Repeat("b", 40))
	if err := validateAPITokenSecrets(c); err != nil {
		t.Fatal(err)
	}
	c.Monitors[0].TokenEnv = "HEARTBEAT_SECRET"
	t.Setenv("HEARTBEAT_SECRET", strings.Repeat("b", 40))
	if validateAPITokenSecrets(c) == nil {
		t.Fatal("heartbeat credential reused")
	}
}

func TestScopedRecoveryStatusFreshness(t *testing.T) {
	s := testStore(t)
	t.Setenv("STATUS_TOKEN", strings.Repeat("s", 40))
	m := Monitor{ID: "worker", Type: "heartbeat", Interval: "30s", Timeout: "5s", MaxAge: "2m", Failures: 1, Successes: 1}
	c := Config{Monitors: []Monitor{m}, APITokens: []APIToken{{Name: "reader", TokenEnv: "STATUS_TOKEN", Monitors: []string{"worker"}, Scopes: []string{"monitor:read"}}}}
	if err := s.heartbeat(m.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.record(m, true, 0, "private error must never escape", false); err != nil {
		t.Fatal(err)
	}
	read := func() map[string]any {
		r := httptest.NewRequest("GET", "/api/monitors/worker/status", nil)
		r.Header.Set("Authorization", "Bearer "+strings.Repeat("s", 40))
		w := httptest.NewRecorder()
		handler(c, s).ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var result map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if len(result) != 5 {
			t.Fatal("unexpected recovery fields", result)
		}
		return result
	}
	result := read()
	if result["status"] != "up" || result["heartbeat_received_at"].(float64) <= 0 || result["checked"].(float64) < result["heartbeat_received_at"].(float64) {
		t.Fatal(result)
	}
	if _, err := s.Exec("UPDATE states SET checked=? WHERE id=?", time.Now().Add(-time.Hour).Unix(), m.ID); err != nil {
		t.Fatal(err)
	}
	if result = read(); result["status"] != "unknown" {
		t.Fatal("stale status reported healthy", result)
	}
}
