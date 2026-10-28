package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"
)

type TelegramHealth struct {
	OK      bool  `json:"ok"`
	Checked int64 `json:"checked"`
}
type Health struct {
	Telegram *TelegramHealth `json:"telegram,omitempty"`
	OK       bool            `json:"ok"`
	Pending  int             `json:"pending_notifications"`
	Oldest   int64           `json:"oldest_pending"`
	Checked  int64           `json:"checked"`
}
type WatchState struct {
	Status   string `json:"status"`
	Failures int    `json:"failures"`
	LastSent string `json:"last_sent"`
}

func inspectPrimary(ctx context.Context, w Watchdog) error {
	req, e := http.NewRequestWithContext(ctx, "GET", w.URL, nil)
	if e != nil {
		return errors.New("invalid primary URL")
	}
	p, _ := secret(w.PasswordEnv)
	req.SetBasicAuth(w.User, p)
	r, e := http.DefaultClient.Do(req)
	if e != nil {
		return errors.New("primary unreachable")
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return errors.New("primary unhealthy")
	}
	var h Health
	if json.NewDecoder(io.LimitReader(r.Body, 65536)).Decode(&h) != nil || !h.OK || h.Checked == 0 || time.Since(time.Unix(h.Checked, 0)) > duration(w.MaxAge) {
		return errors.New("primary checks stale")
	}
	if h.Pending > 0 && h.Oldest > 0 && time.Since(time.Unix(h.Oldest, 0)) > duration(w.MaxAge) {
		return errors.New("primary notifications stalled")
	}
	if h.Telegram != nil && (!h.Telegram.OK || h.Telegram.Checked == 0 || time.Since(time.Unix(h.Telegram.Checked, 0)) > duration(w.MaxAge)) {
		return errors.New("primary Telegram API probe failing or stale")
	}
	return nil
}

// Report loop liveness, even when the services being observed are unhealthy.
// Redirects are rejected so a heartbeat cannot silently acknowledge a login page.
func sendWatchdogHeartbeat(ctx context.Context, w Watchdog) error {
	token, err := secret(w.HeartbeatTokenEnv)
	if err != nil {
		return errors.New("watchdog heartbeat token unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.HeartbeatURL, nil)
	if err != nil {
		return errors.New("invalid watchdog heartbeat URL")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := client.Do(req)
	if err != nil {
		return errors.New("watchdog heartbeat delivery failed")
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusNoContent {
		return errors.New("watchdog heartbeat rejected")
	}
	return nil
}

func runWatchdog(ctx context.Context, c Config, s *Store) error {
	if c.Watchdog == nil {
		return errors.New("watchdog configuration required")
	}
	w := *c.Watchdog
	states := map[string]WatchState{}
	s.getMeta("watchdog", &states)
	tick := time.NewTicker(duration(w.Interval))
	defer tick.Stop()
	for {
		checks := map[string]error{}
		probe, cancel := context.WithTimeout(ctx, 10*time.Second)
		checks["Primary monitor"] = inspectPrimary(probe, w)
		cancel()
		if w.TelegramTokenEnv != "" {
			token, _ := secret(w.TelegramTokenEnv)
			probe, cancel = context.WithTimeout(ctx, 10*time.Second)
			checks["Telegram API from watchdog"] = telegramCall(probe, token, "getMe", url.Values{})
			cancel()
		}
		for name, err := range checks {
			st := states[name]
			previous := st.Status
			if err != nil {
				st.Failures++
				if st.Failures >= 3 {
					st.Status = "down"
				}
			} else {
				st.Failures = 0
				st.Status = "up"
			}
			if w.SMTP.Address == "" {
				if st.Status != previous {
					log.Printf("%s: %s (fallback email not configured)", name, st.Status)
				}
			} else if st.Status != "" && st.LastSent != st.Status && (st.Status == "down" || st.LastSent == "down") {
				probe, cancel = context.WithTimeout(ctx, 20*time.Second)
				e := sendEmail(probe, w.SMTP, "Watchdog observer: "+name+" is "+st.Status)
				cancel()
				if e == nil {
					st.LastSent = st.Status
				} else {
					log.Print("watchdog fallback email failed; retry next interval")
				}
			}
			states[name] = st
		}
		if e := s.setMeta("watchdog", states); e != nil {
			return e
		}
		if w.HeartbeatURL != "" {
			probe, cancel = context.WithTimeout(ctx, 10*time.Second)
			err := sendWatchdogHeartbeat(probe, w)
			cancel()
			if err != nil {
				log.Print(err)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}
