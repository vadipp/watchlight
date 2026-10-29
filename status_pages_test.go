package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNamedStatusPagesIsolateMembership(t *testing.T) {
	s := testStore(t)
	a := testMonitor()
	a.Public = true
	b := a
	b.ID = "other-public"
	b.Name = "Other public"
	hidden := a
	hidden.ID = "private-only"
	hidden.Name = "Private only"
	hidden.Public = false
	c := Config{Monitors: []Monitor{a, b, hidden}, DefaultStatusPage: "main", StatusPages: []StatusPage{{Slug: "main", Name: "Main <status>", Monitors: []string{a.ID}}, {Slug: "other", Name: "Other", Monitors: []string{b.ID}}}}
	h := handler(c, s)
	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest("GET", path, nil))
		return r
	}
	r := get("/")
	if r.Code != 302 || r.Header().Get("Location") != "/status/main" {
		t.Fatal("default page redirect missing")
	}
	r = get("/status/main")
	if r.Code != 200 || !strings.Contains(r.Body.String(), "Main &lt;status&gt;") {
		t.Fatal("page title missing or unescaped")
	}
	r = get("/api/status/main")
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"id":"web"`) || strings.Contains(r.Body.String(), b.ID) || strings.Contains(r.Body.String(), hidden.ID) {
		t.Fatal("page membership leak")
	}
	r = get("/api/status/other")
	if r.Code != 200 || !strings.Contains(r.Body.String(), b.ID) || strings.Contains(r.Body.String(), `"id":"web"`) {
		t.Fatal("other page membership leak")
	}
	for _, p := range []string{"/status/missing", "/api/status/missing"} {
		if get(p).Code != 404 {
			t.Fatal("unknown page must be 404")
		}
	}
	if get("/dashboard").Code != 401 {
		t.Fatal("owner dashboard unprotected")
	}
}
func TestStatusPageConfigRejectsPrivateAndUnknownMembers(t *testing.T) {
	for _, extra := range []string{
		"default_status_page: missing\n",
		"status_pages:\n  - slug: main\n    name: Main\n    monitors: [missing]\n",
		"status_pages:\n  - slug: main\n    name: Main\n    monitors: [local]\n",
	} {
		p := filepath.Join(t.TempDir(), "config.yaml")
		body := "owner_password_env: PASSWORD\nmonitors:\n  - id: local\n    type: http\n    target: https://example.com/\n    public: false\n" + extra
		if e := os.WriteFile(p, []byte(body), 0600); e != nil {
			t.Fatal(e)
		}
		if _, e := loadConfig(p); e == nil {
			t.Fatal("accepted unsafe page configuration")
		}
	}
}
func TestPublishingDoesNotRetriggerDownAlert(t *testing.T) {
	s := testStore(t)
	m := testMonitor()
	m.Failures = 1
	if e := s.record(m, false, 1, "offline", true); e != nil {
		t.Fatal(e)
	}
	m.Public = true
	if e := s.record(m, false, 1, "offline", true); e != nil {
		t.Fatal(e)
	}
	var n int
	if e := s.QueryRow("SELECT count(*) FROM outbox").Scan(&n); e != nil {
		t.Fatal(e)
	}
	if n != 1 || s.state(m.ID).Status != "down" {
		t.Fatal("publishing changed check state or repeated alert")
	}
}

func TestOwnerGroupsStayPrivateAndPreserveFingerprint(t *testing.T) {
	s := testStore(t)
	m := testMonitor()
	old := fingerprint(m)
	m.Group = "Operations"
	m.Public = true
	if fingerprint(m) != old {
		t.Fatal("group or visibility changed check identity")
	}
	c := Config{OwnerUser: "owner", OwnerPasswordEnv: "GROUP_TEST_PASSWORD", Monitors: []Monitor{m}}
	t.Setenv("GROUP_TEST_PASSWORD", "test-password-long-enough")
	h := handler(c, s)
	for _, private := range []bool{false, true} {
		path := "/api/public"
		if private {
			path = "/api/private"
		}
		req := httptest.NewRequest("GET", path, nil)
		if private {
			req.SetBasicAuth("owner", "test-password-long-enough")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatal(rec.Code)
		}
		var data []map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
		_, hasGroup := data[0]["group"]
		if hasGroup != private {
			t.Fatalf("private=%v response=%s", private, rec.Body.String())
		}
	}
}

func TestTechnicalDetailsOnlyInAuthenticatedOwnerSnapshot(t *testing.T) {
	s := testStore(t)
	t.Setenv("DETAIL_PASSWORD", "a-long-owner-password")
	t.Setenv("DETAIL_TOKEN", "never-return-this-token")
	m := testMonitor()
	m.Public = true
	m.Target = "https://internal.example.test/health"
	m.Description = "Internal collector explanation"
	m.BodyContains = "healthy marker"
	m.TokenEnv = "DETAIL_TOKEN"
	heartbeat := m
	heartbeat.ID = "worker"
	heartbeat.Type = "heartbeat"
	heartbeat.Target = ""
	heartbeat.MaxAge = "5m"
	c := Config{OwnerUser: "owner", OwnerPasswordEnv: "DETAIL_PASSWORD", Monitors: []Monitor{m, heartbeat}, StatusPages: []StatusPage{{Slug: "overview", Name: "Overview", Monitors: []string{m.ID, heartbeat.ID}}}}
	h := handler(c, s)
	for _, base := range []string{"/api/private", "/api/public", "/api/status/overview"} {
		for _, m := range []Monitor{m, heartbeat} {
			path := base + "/" + m.ID
			req := httptest.NewRequest("GET", path, nil)
			private := base == "/api/private"
			if private {
				req.SetBasicAuth("owner", "a-long-owner-password")
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Fatal(rec.Code)
			}
			var row map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &row); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"check", "type"} {
				_, present := row[key]
				if present != private {
					t.Fatalf("%s: unexpected %s exposure", path, key)
				}
			}
			if private {
				details := row["check"].(map[string]any)
				if details["description"] != m.Description {
					t.Fatal("missing collector explanation")
				}
				target := m.Target
				if m.Type == "heartbeat" {
					target = "/api/heartbeat/" + m.ID
				}
				if details["target"] != target {
					t.Fatal("missing target")
				}
			}
			for _, secret := range []string{"never-return-this-token", "DETAIL_TOKEN", "DETAIL_PASSWORD"} {
				if strings.Contains(rec.Body.String(), secret) {
					t.Fatal("credential exposed")
				}
			}
			if !private && (strings.Contains(rec.Body.String(), m.Description) || strings.Contains(rec.Body.String(), m.BodyContains)) {
				t.Fatal("technical values exposed publicly")
			}
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/private", nil))
	if rec.Code != 401 {
		t.Fatal("owner details lacked authentication")
	}
}
