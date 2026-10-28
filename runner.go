package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

func check(ctx context.Context, m Monitor, s *Store) (bool, float64, string) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, duration(m.Timeout))
	defer cancel()
	var e error
	switch m.Type {
	case "heartbeat":
		var last int64
		var detail string
		if err := s.QueryRow("SELECT h.time,coalesce(r.detail,'') FROM heartbeats h LEFT JOIN heartbeat_reports r ON r.id=h.id AND r.time=h.time WHERE h.id=?", m.ID).Scan(&last, &detail); err != nil {
			return false, 0, "heartbeat unavailable"
		}
		if last == 0 || time.Since(time.Unix(last, 0)) > duration(m.MaxAge) {
			if detail != "" {
				return false, 0, "heartbeat overdue; last report: " + detail
			}
			return false, 0, "heartbeat overdue"
		}
		if detail != "" {
			return false, 0, detail
		}
		return true, 0, ""
	case "tcp":
		var c net.Conn
		c, e = (&net.Dialer{}).DialContext(ctx, "tcp", m.Target)
		if e == nil {
			c.Close()
		}
	case "http":
		var req *http.Request
		req, e = http.NewRequestWithContext(ctx, "GET", m.Target, nil)
		if e == nil {
			var r *http.Response
			r, e = http.DefaultClient.Do(req)
			if e == nil {
				defer r.Body.Close()
				if r.StatusCode != m.ExpectedStatus {
					e = fmt.Errorf("unexpected HTTP status %d", r.StatusCode)
				} else if m.BodyContains != "" {
					var b []byte
					b, e = io.ReadAll(io.LimitReader(r.Body, 1024*1024))
					if e == nil && !strings.Contains(string(b), m.BodyContains) {
						e = fmt.Errorf("expected text missing in first 1 MiB")
					}
				}
				if m.BodyContains == "" {
					io.Copy(io.Discard, io.LimitReader(r.Body, 32768))
				}
			}
		}
	}
	latency := float64(time.Since(start).Microseconds()) / 1000
	if e != nil {
		if strings.HasPrefix(e.Error(), "unexpected HTTP status ") || e.Error() == "expected text missing in first 1 MiB" {
			return false, latency, e.Error()
		}
		if ctx.Err() != nil {
			return false, latency, "check timed out"
		}
		return false, latency, "connection or TLS negotiation failed"
	}
	return true, latency, ""
}
func runMonitors(ctx context.Context, c Config, s *Store) func() {
	var workers sync.WaitGroup
	for _, m := range c.Monitors {
		workers.Add(1)
		go func(m Monitor) {
			defer workers.Done()
			tick := time.NewTicker(duration(m.Interval))
			defer tick.Stop()
			for {
				ok, lat, msg := check(ctx, m, s)
				if ctx.Err() != nil {
					return
				}
				if e := s.record(m, ok, lat, msg, c.Telegram.TokenEnv != ""); e != nil {
					log.Printf("record %s failed: %v", m.ID, e)
				}
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
			}
		}(m)
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			if e := s.prune(c.RetentionDays); e != nil {
				log.Printf("retention failed: %v", e)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return workers.Wait
}
