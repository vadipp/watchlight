package main

import "testing"

func TestNotificationOutboxFormat(t *testing.T) {
	for _, tc := range []struct {
		name     string
		kind     string
		disabled bool
		wantUp   string
	}{
		{"latency", "http", false, "[Website] [✅ Up] 42 ms"},
		{"heartbeat", "heartbeat", false, "[Website] [✅ Up] Check passed"},
		{"disabled latency", "http", true, "[Website] [✅ Up] Check passed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			m := testMonitor()
			m.Type = tc.kind
			if tc.disabled {
				enabled := false
				m.RecordLatency = &enabled
			}
			for i := 0; i < m.Failures; i++ {
				if err := s.record(m, false, 0, "Connection refused\nby server", true); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < m.Successes; i++ {
				if err := s.record(m, true, 42.4, "", true); err != nil {
					t.Fatal(err)
				}
			}
			rows, err := s.Query("SELECT body FROM outbox ORDER BY seq")
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			for _, want := range []string{"[Website] [🔴 Down] Connection refused by server", tc.wantUp} {
				var got string
				if !rows.Next() {
					t.Fatal("missing notification")
				}
				if err := rows.Scan(&got); err != nil {
					t.Fatal(err)
				}
				if got != want {
					t.Fatalf("got %q, want %q", got, want)
				}
			}
			if rows.Next() {
				t.Fatal("unexpected notification")
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
