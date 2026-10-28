package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPConnectionReuseOption(t *testing.T) {
	var addresses []string
	var closes []bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		addresses = append(addresses, r.RemoteAddr)
		closes = append(closes, r.Close)
		w.Write([]byte("healthy"))
	}))
	defer server.Close()
	m := testMonitor()
	m.Target = server.URL
	m.ExpectedStatus = 200
	fp := fingerprint(m)
	for _, disabled := range []bool{false, false, true, true, false} {
		m.DisableKeepAlive = disabled
		if fingerprint(m) != fp {
			t.Fatal("connection option resets incident thresholds")
		}
		ok, _, detail := check(context.Background(), m, nil)
		if !ok {
			t.Fatal(detail)
		}
	}
	if addresses[0] != addresses[1] || addresses[0] != addresses[4] {
		t.Fatal("default connection pool changed")
	}
	if addresses[2] == addresses[0] || addresses[3] == addresses[0] || addresses[2] == addresses[3] {
		t.Fatal("opted-out check reused a connection")
	}
	if !closes[2] || !closes[3] {
		t.Fatal("opted-out connections not closed")
	}
}
