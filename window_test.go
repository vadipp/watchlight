package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestWindowCoverage(t *testing.T) {
	s := testStore(t)
	// A leading partial sample, one failure, and a long monitoring gap.
	for _, p := range []Point{{Time: 990, OK: true}, {Time: 1020, OK: false}, {Time: 1200, OK: true}} {
		if _, err := s.Exec("INSERT INTO results VALUES(?,?,?,?)", "web", p.Time, p.OK, 10); err != nil {
			t.Fatal(err)
		}
	}
	buckets, total, up, err := s.windowHistory("web", 1000, 1240, 30)
	if err != nil {
		t.Fatal(err)
	}
	var good, bad float64
	for _, b := range buckets {
		good += b.Up
		bad += b.Down
		if b.Up+b.Down > float64(b.End-b.Time) {
			t.Fatalf("overlapping coverage: %+v", b)
		}
	}
	if total != 2 || up != 1 || good != 50 || bad != 30 {
		t.Fatalf("total=%d up=%d coverage=%v/%v", total, up, good, bad)
	}
	if len(buckets) != 240 || buckets[0].Time != 1000 || buckets[239].End != 1240 {
		t.Fatal("window boundaries")
	}
}

func TestWindowSelection(t *testing.T) {
	s := testStore(t)
	m := testMonitor()
	m.Public = true
	h := handler(Config{Monitors: []Monitor{m}}, s)
	for _, tc := range []struct {
		query string
		hours int
	}{{"", 24}, {"4h", 4}, {"24h", 24}, {"7d", 168}} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest("GET", "/api/public?window="+tc.query, nil))
		var views []View
		if r.Code != 200 {
			t.Fatal(r.Code, r.Body.String())
		}
		if err := json.Unmarshal(r.Body.Bytes(), &views); err != nil {
			t.Fatal(err)
		}
		v := views[0]
		if v.WindowEnd-v.WindowStart != int64(tc.hours)*3600 || len(v.Buckets) != 240 || v.Uptime != nil {
			t.Fatalf("bad window: %+v", v)
		}
	}
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest("GET", "/api/public?window=365d", nil))
	if r.Code != 400 {
		t.Fatal("unbounded window accepted")
	}
}
