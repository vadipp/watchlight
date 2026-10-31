package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Maintenance records are immutable except for an idempotent early completion.
// The server assigns starts_at: callers cannot relabel past failures as planned.
type Maintenance struct {
	MonitorID   string `json:"monitor_id"`
	OperationID string `json:"operation_id"`
	Reason      string `json:"reason"`
	Source      string `json:"source"`
	StartsAt    int64  `json:"starts_at"`
	ExpiresAt   int64  `json:"expires_at"`
	CompletedAt *int64 `json:"completed_at,omitempty"`
}

func (m Maintenance) end() int64 {
	if m.CompletedAt != nil {
		return min(m.ExpiresAt, *m.CompletedAt)
	}
	return m.ExpiresAt
}
func (s *Store) maintenanceHistory(ctx context.Context, id string, start, end int64) ([]Maintenance, error) {
	rows, err := s.QueryContext(ctx, `SELECT monitor_id,operation_id,reason,source,starts_at,expires_at,completed_at FROM maintenance WHERE monitor_id=? AND starts_at<? AND coalesce(completed_at,expires_at)>? ORDER BY starts_at,operation_id`, id, end, start)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Maintenance{}
	for rows.Next() {
		var m Maintenance
		if err := rows.Scan(&m.MonitorID, &m.OperationID, &m.Reason, &m.Source, &m.StartsAt, &m.ExpiresAt, &m.CompletedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
func (s *Store) activeMaintenance(ctx context.Context, now int64) (map[string]bool, error) {
	rows, err := s.QueryContext(ctx, `SELECT DISTINCT monitor_id FROM maintenance WHERE starts_at<=? AND coalesce(completed_at,expires_at)>?`, now, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
func scanMaintenance(row *sql.Row) (Maintenance, error) {
	var m Maintenance
	err := row.Scan(&m.MonitorID, &m.OperationID, &m.Reason, &m.Source, &m.StartsAt, &m.ExpiresAt, &m.CompletedAt)
	return m, err
}
func validMaintenanceText(s string, limit int) bool {
	return strings.TrimSpace(s) != "" && len(s) <= limit && !strings.ContainsFunc(s, unicode.IsControl)
}
func maintenanceRoutes(mux *http.ServeMux, c Config, s *Store, auth func(http.HandlerFunc) http.HandlerFunc) {
	exists := func(id string) bool {
		for _, m := range c.Monitors {
			if m.ID == id {
				return true
			}
		}
		return false
	}
	// A JSON-only API plus browser cross-site rejection protects Basic-auth writes.
	writeRequest := func(w http.ResponseWriter, r *http.Request) bool {
		media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if media != "application/json" || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			http.Error(w, "JSON same-origin request required", 415)
			return false
		}
		return true
	}
	mux.HandleFunc("GET /api/maintenance/{id}", scopedAuth(c, auth, "maintenance:read")(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !exists(id) {
			http.NotFound(w, r)
			return
		}
		end := time.Now().Unix() + 1
		start := end - 86400
		var err error
		if v := r.URL.Query().Get("start"); v != "" {
			start, err = strconv.ParseInt(v, 10, 64)
			if err != nil {
				http.Error(w, "Invalid start", 400)
				return
			}
		}
		if v := r.URL.Query().Get("end"); v != "" {
			end, err = strconv.ParseInt(v, 10, 64)
			if err != nil {
				http.Error(w, "Invalid end", 400)
				return
			}
		}
		if start < 0 || end <= start || end-start > 90*86400 {
			http.Error(w, "Window must be at most 90 days", 400)
			return
		}
		rows, err := s.maintenanceHistory(r.Context(), id, start, end)
		if err != nil {
			http.Error(w, "Storage unavailable", 503)
			return
		}
		writeJSON(w, rows)
	}))
	mux.HandleFunc("PUT /api/maintenance/{id}/{operation}", scopedAuth(c, auth, "maintenance:write")(func(w http.ResponseWriter, r *http.Request) {
		if !writeRequest(w, r) {
			return
		}
		id, op := r.PathValue("id"), r.PathValue("operation")
		if !exists(id) {
			http.NotFound(w, r)
			return
		}
		if !containerIdentity.MatchString(op) {
			http.Error(w, "Invalid operation ID", 400)
			return
		}
		var input struct {
			Reason   string `json:"reason"`
			Source   string `json:"source"`
			Duration int64  `json:"duration_seconds"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		dec.DisallowUnknownFields()
		if dec.Decode(&input) != nil || dec.Decode(new(any)) != io.EOF || !validMaintenanceText(input.Reason, 512) || !validMaintenanceText(input.Source, 128) || input.Duration < 1 || input.Duration > 86400 {
			http.Error(w, "Invalid maintenance request", 400)
			return
		}
		if grant, ok := r.Context().Value(apiTokenKey{}).(APIToken); ok && input.Duration > grant.MaxDurationSeconds {
			http.Error(w, "Duration exceeds token grant", 403)
			return
		}
		tx, err := s.BeginTx(r.Context(), nil)
		if err != nil {
			http.Error(w, "Storage unavailable", 503)
			return
		}
		defer tx.Rollback()
		now := time.Now().Unix()
		result, err := tx.ExecContext(r.Context(), `INSERT INTO maintenance(monitor_id,operation_id,reason,source,starts_at,expires_at) VALUES(?,?,?,?,?,?) ON CONFLICT(monitor_id,operation_id) DO NOTHING`, id, op, input.Reason, input.Source, now, now+input.Duration)
		if err != nil {
			http.Error(w, "Storage unavailable", 503)
			return
		}
		m, err := scanMaintenance(tx.QueryRowContext(r.Context(), `SELECT monitor_id,operation_id,reason,source,starts_at,expires_at,completed_at FROM maintenance WHERE monitor_id=? AND operation_id=?`, id, op))
		if err != nil {
			http.Error(w, "Storage unavailable", 503)
			return
		}
		if m.Reason != input.Reason || m.Source != input.Source || m.ExpiresAt-m.StartsAt != input.Duration {
			http.Error(w, "Operation ID already used with different parameters", 409)
			return
		}
		if tx.Commit() != nil {
			http.Error(w, "Storage unavailable", 503)
			return
		}
		n, _ := result.RowsAffected()
		w.Header().Set("Content-Type", "application/json")
		if n > 0 {
			w.WriteHeader(201)
		}
		writeJSON(w, m)
	}))
	mux.HandleFunc("POST /api/maintenance/{id}/{operation}/complete", scopedAuth(c, auth, "maintenance:write")(func(w http.ResponseWriter, r *http.Request) {
		if !writeRequest(w, r) {
			return
		}
		id, op := r.PathValue("id"), r.PathValue("operation")
		if !exists(id) {
			http.NotFound(w, r)
			return
		}
		// No completion fields: bounded decoding rejects accidental or unsupported input.
		var input map[string]json.RawMessage
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
		dec.DisallowUnknownFields()
		if dec.Decode(&input) != nil || input == nil || len(input) != 0 || dec.Decode(new(any)) != io.EOF {
			http.Error(w, "Expected empty JSON object", 400)
			return
		}
		tx, err := s.BeginTx(r.Context(), nil)
		if err != nil {
			http.Error(w, "Storage unavailable", 503)
			return
		}
		defer tx.Rollback()
		_, err = tx.ExecContext(r.Context(), `UPDATE maintenance SET completed_at=min(expires_at,?) WHERE monitor_id=? AND operation_id=? AND completed_at IS NULL`, time.Now().Unix(), id, op)
		if err != nil {
			http.Error(w, "Storage unavailable", 503)
			return
		}
		m, err := scanMaintenance(tx.QueryRowContext(r.Context(), `SELECT monitor_id,operation_id,reason,source,starts_at,expires_at,completed_at FROM maintenance WHERE monitor_id=? AND operation_id=?`, id, op))
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil || tx.Commit() != nil {
			http.Error(w, "Storage unavailable", 503)
			return
		}
		writeJSON(w, m)
	}))
}

type maintenanceInterval struct{ start, end int64 }

func maintenanceIntervals(records []Maintenance) []maintenanceInterval {
	out := []maintenanceInterval{}
	for _, m := range records {
		end := m.end()
		if end <= m.StartsAt {
			continue
		}
		if len(out) > 0 && m.StartsAt <= out[len(out)-1].end {
			out[len(out)-1].end = max(out[len(out)-1].end, end)
		} else {
			out = append(out, maintenanceInterval{m.StartsAt, end})
		}
	}
	return out
}
func maintenanceOverlap(intervals []maintenanceInterval, start, end float64) float64 {
	var sum float64
	for _, m := range intervals {
		if float64(m.start) >= end {
			break
		}
		sum += max(0, min(end, float64(m.end))-max(start, float64(m.start)))
	}
	return sum
}
