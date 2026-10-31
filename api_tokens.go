package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"
)

// Grants are explicit monitor allowlists. No wildcard or owner permission exists.
type APIToken struct {
	Name               string   `yaml:"name"`
	TokenEnv           string   `yaml:"token_env"`
	Monitors           []string `yaml:"monitors"`
	Scopes             []string `yaml:"scopes"`
	OperationPrefix    string   `yaml:"operation_prefix"`
	MaxDurationSeconds int64    `yaml:"max_duration_seconds"`
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func validateAPITokens(c Config, monitors map[string]bool) error {
	names, envs := map[string]bool{}, map[string]bool{c.OwnerPasswordEnv: true}
	for _, m := range c.Monitors {
		if m.TokenEnv != "" {
			envs[m.TokenEnv] = true
		}
	}
	for _, t := range c.APITokens {
		if !monitorID.MatchString(t.Name) || names[t.Name] || t.TokenEnv == "" || envs[t.TokenEnv] || len(t.Monitors) == 0 || len(t.Scopes) == 0 {
			return errors.New("API tokens require unique names/secret environments and nonempty monitor/scope allowlists")
		}
		names[t.Name], envs[t.TokenEnv] = true, true
		seen := map[string]bool{}
		for _, id := range t.Monitors {
			if !monitors[id] || seen[id] {
				return errors.New("API token has unknown or duplicate monitor")
			}
			seen[id] = true
		}
		seen = map[string]bool{}
		for _, scope := range t.Scopes {
			if (scope != "maintenance:read" && scope != "maintenance:write" && scope != "monitor:read") || seen[scope] {
				return errors.New("API token has unknown or duplicate scope")
			}
			seen[scope] = true
		}
		if contains(t.Scopes, "maintenance:write") {
			if !containerIdentity.MatchString(t.OperationPrefix) || len(t.OperationPrefix) > 96 || t.MaxDurationSeconds < 1 || t.MaxDurationSeconds > 86400 {
				return errors.New("maintenance write tokens require an operation_prefix (1..96 characters) and max_duration_seconds (1..86400)")
			}
		} else if t.OperationPrefix != "" || t.MaxDurationSeconds != 0 {
			return errors.New("maintenance write restrictions require maintenance:write")
		}
	}
	return nil
}

func validateAPITokenSecrets(c Config) error {
	used := []string{os.Getenv(c.OwnerPasswordEnv)}
	for _, m := range c.Monitors {
		if m.TokenEnv != "" {
			used = append(used, os.Getenv(m.TokenEnv))
		}
	}
	for _, t := range c.APITokens {
		value := os.Getenv(t.TokenEnv)
		if len(value) < 32 || strings.ContainsFunc(value, unicode.IsSpace) {
			return errors.New("API token secrets must contain at least 32 characters and no whitespace")
		}
		for _, other := range used {
			if equal(value, other) {
				return errors.New("API token secrets must be distinct from other API, heartbeat and owner credentials")
			}
		}
		used = append(used, value)
	}
	return nil
}

type apiTokenKey struct{}

func scopedAuth(c Config, owner func(http.HandlerFunc) http.HandlerFunc, scope string) func(http.HandlerFunc) http.HandlerFunc {
	// Snapshot secrets once, like owner authentication. Rotation takes effect on restart.
	values := make([]string, len(c.APITokens))
	for i, t := range c.APITokens {
		values[i] = os.Getenv(t.TokenEnv)
	}
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				owner(next)(w, r)
				return
			}
			value := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			matched := -1
			for i := range values {
				if len(values[i]) >= 32 && equal(value, values[i]) {
					matched = i
				}
			}
			if matched < 0 {
				http.Error(w, "Authentication required", 401)
				return
			}
			grant := c.APITokens[matched]
			if !contains(grant.Scopes, scope) || !contains(grant.Monitors, r.PathValue("id")) ||
				(scope == "maintenance:write" && (!strings.HasPrefix(r.PathValue("operation"), grant.OperationPrefix) || r.PathValue("operation") == grant.OperationPrefix)) {
				http.Error(w, "Scope denied", 403)
				return
			}
			next(w, r.WithContext(context.WithValue(r.Context(), apiTokenKey{}, grant)))
		}
	}
}

// Recovery-only read: no targets, descriptions, errors or maintenance reasons.
func monitorStatusRoute(mux *http.ServeMux, c Config, s *Store, owner func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/monitors/{id}/status", scopedAuth(c, owner, "monitor:read")(func(w http.ResponseWriter, r *http.Request) {
		for _, m := range c.Monitors {
			if m.ID != r.PathValue("id") {
				continue
			}
			state, err := s.stateContext(r.Context(), m.ID)
			if err != nil {
				http.Error(w, "Storage unavailable", 503)
				return
			}
			now := time.Now().Unix()
			if state.Checked == 0 || time.Since(time.Unix(state.Checked, 0)) > duration(m.Interval)+duration(m.Timeout)+15*time.Second {
				state.Status = "unknown"
			}
			var heartbeat int64
			if m.Type == "heartbeat" {
				if err = s.QueryRowContext(r.Context(), "SELECT coalesce((SELECT time FROM heartbeats WHERE id=?),0)", m.ID).Scan(&heartbeat); err != nil {
					http.Error(w, "Storage unavailable", 503)
					return
				}
			}
			writeJSON(w, struct {
				ID                  string `json:"id"`
				Status              string `json:"status"`
				Checked             int64  `json:"checked"`
				HeartbeatReceivedAt int64  `json:"heartbeat_received_at"`
				ServerTime          int64  `json:"server_time"`
			}{m.ID, state.Status, state.Checked, heartbeat, now})
			return
		}
		http.NotFound(w, r)
	}))
}
