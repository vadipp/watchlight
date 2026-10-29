package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"time"
)

//go:embed web/*
var assets embed.FS

type View struct {
	WindowStart     int64          `json:"window_start"`
	WindowEnd       int64          `json:"window_end"`
	Buckets         []WindowBucket `json:"buckets"`
	Check           *CheckInfo     `json:"check,omitempty"`
	LatencyMS       *float64       `json:"latency_ms"`
	Group           string         `json:"group,omitempty"`
	IntervalSeconds float64        `json:"interval_seconds,omitempty"`
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	Type            string         `json:"type,omitempty"`
	State           State          `json:"state"`
	History         []Point        `json:"history"`
	Incidents       []Incident     `json:"incidents"`
	Uptime          *float64       `json:"uptime"`
	Samples         int            `json:"samples"`
}

// Explicit allowlist: credentials and secret environment names never belong here.
type CheckInfo struct {
	DisableKeepAlive bool   `json:"disable_keep_alive,omitempty"`
	RecordLatency    *bool  `json:"record_latency,omitempty"`
	Type             string `json:"type"`
	Target           string `json:"target"`
	Description      string `json:"description,omitempty"`
	Interval         string `json:"interval"`
	Timeout          string `json:"timeout"`
	Failures         int    `json:"failures"`
	Successes        int    `json:"successes"`
	ExpectedStatus   int    `json:"expected_status,omitempty"`
	BodyContains     string `json:"body_contains,omitempty"`
	MaxAge           string `json:"max_age,omitempty"`
}

func equal(a, b string) bool {
	x := sha256.Sum256([]byte(a))
	y := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}
func handler(c Config, s *Store) http.Handler {
	mux := http.NewServeMux()
	password, _ := secret(c.OwnerPasswordEnv)
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			u, p, ok := r.BasicAuth()
			if !ok || !equal(u, c.OwnerUser) || !equal(p, password) {
				w.Header().Set("WWW-Authenticate", `Basic realm="Watchlight", charset="UTF-8"`)
				http.Error(w, "Authentication required", 401)
				return
			}
			next(w, r)
		}
	}
	snapshot := func(private bool, page *StatusPage) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			hours := 24
			switch r.URL.Query().Get("window") {
			case "", "24h":
			case "4h":
				hours = 4
			case "7d":
				hours = 168
			default:
				http.Error(w, "Invalid window", 400)
				return
			}
			end := time.Now().Unix()
			start := end - int64(hours)*3600
			views := []View{}
			members := map[string]bool{}
			if page != nil {
				for _, id := range page.Monitors {
					members[id] = true
				}
			}
			for _, m := range c.Monitors {
				if !private && (!m.Public || (page != nil && !members[m.ID])) {
					continue
				}
				st := s.state(m.ID)
				if st.Checked == 0 || time.Since(time.Unix(st.Checked, 0)) > duration(m.Interval)+duration(m.Timeout)+15*time.Second {
					st.Status = "unknown"
				}
				if !private {
					st.Error = ""
				}
				buckets, total, up, e := s.windowHistory(m.ID, start, end, duration(m.Interval).Seconds())
				h, historyErr := s.history(m.ID)
				if historyErr != nil {
					http.Error(w, "Storage unavailable", 503)
					return
				}
				if e != nil {
					http.Error(w, "Storage unavailable", 503)
					return
				}
				inc, e := s.incidents(m.ID)
				if e != nil {
					http.Error(w, "Storage unavailable", 503)
					return
				}

				var uptime *float64
				if total > 0 {
					v := 100 * float64(up) / float64(total)
					uptime = &v
				}
				if !m.recordsLatency() {
					st.Latency = 0
					for i := range buckets {
						buckets[i].Latency = 0
					}
					for i := range h {
						h[i].Latency = 0
					}
				}
				view := View{WindowStart: start, WindowEnd: end, Buckets: buckets, ID: m.ID, Name: m.Name, State: st, History: h, Incidents: inc, Uptime: uptime, Samples: total}
				if m.recordsLatency() && st.Checked != 0 {
					latency := st.Latency
					view.LatencyMS = &latency
				}
				if private {
					view.Group = m.Group
					view.Type = m.Type
					view.IntervalSeconds = duration(m.Interval).Seconds()
					target := m.Target
					if m.Type == "heartbeat" {
						target = "/api/heartbeat/" + m.ID
					}
					view.Check = &CheckInfo{Type: m.Type, Target: target, Description: m.Description, Interval: m.Interval, Timeout: m.Timeout, Failures: m.Failures, Successes: m.Successes}
					if m.Type == "http" {
						enabled := m.recordsLatency()
						view.Check.RecordLatency = &enabled
						view.Check.DisableKeepAlive = m.DisableKeepAlive
						view.Check.ExpectedStatus = m.ExpectedStatus
						view.Check.BodyContains = m.BodyContains
					}
					if m.Type == "heartbeat" {
						view.Check.MaxAge = m.MaxAge
					}
				}
				views = append(views, view)
			}
			if page != nil {
				writeJSON(w, struct {
					Slug     string `json:"slug"`
					Name     string `json:"name"`
					Monitors []View `json:"monitors"`
				}{page.Slug, page.Name, views})
			} else {
				writeJSON(w, views)
			}
		}
	}
	mux.HandleFunc("GET /api/public", snapshot(false, nil))
	mux.HandleFunc("GET /api/private", auth(snapshot(true, nil)))
	mux.HandleFunc("GET /api/health", auth(func(w http.ResponseWriter, r *http.Request) {
		h := Health{OK: true, Checked: time.Now().Unix()}
		if c.Telegram.TokenEnv != "" {
			h.Telegram = &TelegramHealth{}
			s.getMeta("telegram", h.Telegram)
		}
		for _, m := range c.Monitors {
			st := s.state(m.ID)
			if st.Checked == 0 || time.Since(time.Unix(st.Checked, 0)) > duration(m.Interval)+duration(m.Timeout)+15*time.Second {
				h.OK = false
			}
		}
		if e := s.QueryRow("SELECT count(*),coalesce(min(created),0) FROM outbox").Scan(&h.Pending, &h.Oldest); e != nil {
			h.OK = false
		}
		w.Header().Set("Content-Type", "application/json")
		if !h.OK {
			w.WriteHeader(503)
		}
		writeJSON(w, h)
	}))
	heartbeatHandler := func(w http.ResponseWriter, r *http.Request) {
		for _, m := range c.Monitors {
			if m.ID != r.PathValue("id") || m.Type != "heartbeat" {
				continue
			}
			token, _ := secret(m.TokenEnv)
			if !equal(r.Header.Get("Authorization"), "Bearer "+token) {
				http.Error(w, "Unauthorized", 401)
				return
			}
			detail, err := heartbeatDetail(w, r)
			if err != nil {
				http.Error(w, "Invalid heartbeat report", 400)
				return
			}
			if e := s.heartbeat(m.ID, detail); e != nil {
				http.Error(w, "Storage unavailable", 503)
				return
			}
			w.WriteHeader(204)
			return
		}
		http.NotFound(w, r)
	}
	mux.HandleFunc("POST /api/heartbeat/{id}", heartbeatHandler)
	mux.HandleFunc("GET /api/heartbeat/{id}", heartbeatHandler)
	files, _ := fs.Sub(assets, "web")
	findPage := func(slug string) *StatusPage {
		for i := range c.StatusPages {
			if c.StatusPages[i].Slug == slug {
				return &c.StatusPages[i]
			}
		}
		return nil
	}
	mux.HandleFunc("GET /api/status/{slug}", func(w http.ResponseWriter, r *http.Request) {
		p := findPage(r.PathValue("slug"))
		if p == nil {
			http.NotFound(w, r)
			return
		}
		snapshot(false, p)(w, r)
	})
	mux.HandleFunc("GET /status/{slug}", func(w http.ResponseWriter, r *http.Request) {
		p := findPage(r.PathValue("slug"))
		if p == nil {
			http.NotFound(w, r)
			return
		}
		b, _ := fs.ReadFile(files, "index.html")
		body := strings.Replace(string(b), "<h1>Service status</h1>", "<h1>"+html.EscapeString(p.Name)+"</h1>", 1)
		body = strings.Replace(body, "<title>Watchlight · Service status</title>", "<title>Watchlight · "+html.EscapeString(p.Name)+"</title>", 1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(body))
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if c.DefaultStatusPage != "" {
			http.Redirect(w, r, "/status/"+c.DefaultStatusPage, http.StatusFound)
			return
		}
		http.FileServer(http.FS(files)).ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /dashboard", auth(func(w http.ResponseWriter, r *http.Request) {
		b, _ := fs.ReadFile(files, "index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	}))
	mux.Handle("GET /", http.FileServer(http.FS(files)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/dashboard" {
			w.Header().Set("Cache-Control", "no-store")
		}
		mux.ServeHTTP(w, r)
	})
}

// Only structured container identities and enumerated states are accepted.
// Raw command output, Docker errors and arbitrary diagnostic text are excluded.
func heartbeatDetail(w http.ResponseWriter, r *http.Request) (string, error) {
	var report struct {
		Failures []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"failures"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	dec.DisallowUnknownFields()
	err := dec.Decode(&report)
	if err == io.EOF {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if r.Method != http.MethodPost || len(report.Failures) > 64 {
		return "", fmt.Errorf("invalid report")
	}
	if dec.Decode(new(any)) != io.EOF {
		return "", fmt.Errorf("trailing data")
	}
	parts := []string{}
	for _, failure := range report.Failures {
		if !containerIdentity.MatchString(failure.Name) {
			return "", fmt.Errorf("invalid identity")
		}
		switch failure.Status {
		case "missing", "exited", "paused", "restarting", "dead", "created", "removing", "unhealthy", "starting", "not-running":
		default:
			return "", fmt.Errorf("invalid state")
		}
		parts = append(parts, failure.Name+": "+failure.Status)
	}
	return strings.Join(parts, "; "), nil
}

var containerIdentity = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
