package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type StatusPage struct {
	Slug     string   `yaml:"slug"`
	Name     string   `yaml:"name"`
	Monitors []string `yaml:"monitors"`
}
type Config struct {
	StatusPages       []StatusPage `yaml:"status_pages"`
	DefaultStatusPage string       `yaml:"default_status_page"`
	Listen            string       `yaml:"listen"`
	Database          string       `yaml:"database"`
	OwnerUser         string       `yaml:"owner_user"`
	OwnerPasswordEnv  string       `yaml:"owner_password_env"`
	RetentionDays     int          `yaml:"retention_days"`
	Monitors          []Monitor    `yaml:"monitors"`
	Telegram          Telegram     `yaml:"telegram"`
	Watchdog          *Watchdog    `yaml:"watchdog"`
}
type Monitor struct {
	ID             string `yaml:"id"`
	Name           string `yaml:"name"`
	Type           string `yaml:"type"`
	Target         string `yaml:"target"`
	Public         bool   `yaml:"public"`
	Interval       string `yaml:"interval"`
	Timeout        string `yaml:"timeout"`
	Failures       int    `yaml:"failures"`
	Successes      int    `yaml:"successes"`
	ExpectedStatus int    `yaml:"expected_status"`
	BodyContains   string `yaml:"body_contains"`
	TokenEnv       string `yaml:"token_env"`
	MaxAge         string `yaml:"max_age"`
}
type Telegram struct {
	RelayAddress string `yaml:"relay_address"`
	TokenEnv     string `yaml:"token_env"`
	ChatIDEnv    string `yaml:"chat_id_env"`
}
type Watchdog struct {
	URL              string `yaml:"url"`
	PasswordEnv      string `yaml:"password_env"`
	User             string `yaml:"user"`
	TelegramTokenEnv string `yaml:"telegram_token_env"`
	Interval         string `yaml:"interval"`
	MaxAge           string `yaml:"max_age"`
	SMTP             SMTP   `yaml:"smtp"`
}
type SMTP struct {
	Address     string `yaml:"address"`
	User        string `yaml:"user"`
	PasswordEnv string `yaml:"password_env"`
	From        string `yaml:"from"`
	To          string `yaml:"to"`
}

func duration(s string) time.Duration { d, _ := time.ParseDuration(s); return d }

var monitorID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

func loadConfig(path string) (Config, error) {
	var c Config
	b, e := os.ReadFile(path)
	if e != nil {
		return c, e
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if e = dec.Decode(&c); e != nil {
		return c, e
	}
	var extra any
	if e = dec.Decode(&extra); e != io.EOF {
		return c, errors.New("configuration must contain exactly one YAML document")
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8080"
	}
	if c.Database == "" {
		c.Database = "data/watchlight.db"
	}
	if c.OwnerUser == "" {
		c.OwnerUser = "owner"
	}
	if c.RetentionDays == 0 {
		c.RetentionDays = 7
	}
	if c.RetentionDays < 1 || c.RetentionDays > 365 {
		return c, errors.New("retention_days must be 1..365")
	}
	if c.OwnerPasswordEnv == "" {
		return c, errors.New("owner_password_env is required")
	}
	seen := map[string]bool{}
	for i := range c.Monitors {
		m := &c.Monitors[i]
		if !monitorID.MatchString(m.ID) || seen[m.ID] {
			return c, errors.New("monitor IDs must be unique, 1..64 letters/digits/underscores/hyphens, starting with a letter or digit")
		}
		seen[m.ID] = true
		if m.Name == "" {
			m.Name = m.ID
		}
		if m.Interval == "" {
			m.Interval = "30s"
		}
		if m.Timeout == "" {
			m.Timeout = "10s"
		}
		if m.Failures == 0 {
			m.Failures = 3
		}
		if m.Successes == 0 {
			m.Successes = 1
		}
		if m.ExpectedStatus == 0 {
			m.ExpectedStatus = 200
		}
		if duration(m.Interval) < time.Second || duration(m.Timeout) <= 0 || duration(m.Timeout) > duration(m.Interval) || m.Failures < 1 || m.Successes < 1 {
			return c, fmt.Errorf("%s: invalid timing or thresholds", m.ID)
		}
		switch m.Type {
		case "http":
			if m.ExpectedStatus < 100 || m.ExpectedStatus > 599 {
				return c, fmt.Errorf("%s: invalid expected_status", m.ID)
			}
			u, err := url.Parse(m.Target)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
				return c, fmt.Errorf("%s: invalid HTTP target", m.ID)
			}
		case "tcp":
			if _, _, err := net.SplitHostPort(m.Target); err != nil {
				return c, fmt.Errorf("%s: invalid TCP target", m.ID)
			}
		case "heartbeat":
			if m.MaxAge == "" {
				m.MaxAge = "2m"
			}
			if duration(m.MaxAge) < duration(m.Interval) {
				return c, fmt.Errorf("%s: max_age must be at least interval", m.ID)
			}
			if m.TokenEnv == "" {
				return c, fmt.Errorf("%s: token_env required", m.ID)
			}
		default:
			return c, fmt.Errorf("%s: unsupported type", m.ID)
		}
	}
	pages := map[string]bool{}
	for _, p := range c.StatusPages {
		if !monitorID.MatchString(p.Slug) || pages[p.Slug] || strings.TrimSpace(p.Name) == "" {
			return c, errors.New("status pages require unique valid slugs and nonempty names")
		}
		pages[p.Slug] = true
		members := map[string]bool{}
		for _, id := range p.Monitors {
			if !seen[id] || members[id] {
				return c, fmt.Errorf("status page %s: unknown or duplicate monitor %s", p.Slug, id)
			}
			members[id] = true
			for _, m := range c.Monitors {
				if m.ID == id && !m.Public {
					return c, fmt.Errorf("status page %s: monitor %s must be public", p.Slug, id)
				}
			}
		}
	}
	if c.DefaultStatusPage != "" && !pages[c.DefaultStatusPage] {
		return c, errors.New("default_status_page must reference a configured page")
	}
	if c.Telegram.RelayAddress != "" {
		if _, _, e := net.SplitHostPort(c.Telegram.RelayAddress); e != nil {
			return c, errors.New("invalid Telegram relay_address")
		}
	}
	if (c.Telegram.TokenEnv == "") != (c.Telegram.ChatIDEnv == "") {
		return c, errors.New("Telegram requires token_env and chat_id_env")
	}
	if w := c.Watchdog; w != nil {
		if w.Interval == "" {
			w.Interval = "30s"
		}
		if w.MaxAge == "" {
			w.MaxAge = "2m"
		}
		u, e := url.Parse(w.URL)
		if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || duration(w.Interval) < time.Second || duration(w.MaxAge) <= 0 {
			return c, errors.New("invalid watchdog URL/timing")
		}
		if w.PasswordEnv == "" {
			return c, errors.New("watchdog needs primary authentication")
		}
		smtpSet := w.SMTP.Address != "" || w.SMTP.From != "" || w.SMTP.To != "" || w.SMTP.User != "" || w.SMTP.PasswordEnv != ""
		if smtpSet && (w.SMTP.Address == "" || w.SMTP.From == "" || w.SMTP.To == "") {
			return c, errors.New("incomplete SMTP fallback configuration")
		}
		if w.SMTP.User != "" && w.SMTP.PasswordEnv == "" {
			return c, errors.New("SMTP password_env is required when user is set")
		}
		if _, _, e := net.SplitHostPort(w.SMTP.Address); w.SMTP.Address != "" && e != nil {
			return c, e
		}
	}
	if w := c.Watchdog; w != nil {
		for _, v := range []string{w.SMTP.From, w.SMTP.To, w.SMTP.User} {
			if strings.ContainsAny(v, "\r\n") {
				return c, errors.New("invalid SMTP header")
			}
		}
	}
	return c, nil
}
func secret(name string) (string, error) {
	v := os.Getenv(name)
	if strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("required environment variable %s is empty", name)
	}
	return v, nil
}
func fingerprint(m Monitor) string {
	// Visibility does not affect check semantics or incident thresholds.
	m.Public = false
	b, _ := json.Marshal(m)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
