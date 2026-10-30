package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"strings"
	"time"
)

func notificationBody(m Monitor, status string, latency float64, detail string) string {
	label := "🔴 Down"
	if status == "up" {
		label = "✅ Up"
		if m.recordsLatency() {
			detail = fmt.Sprintf("%.0f ms", latency)
		} else {
			detail = "Check passed"
		}
	} else if strings.TrimSpace(detail) == "" {
		detail = "Check failed"
	}
	return fmt.Sprintf("[%s] [%s] %s", m.Name, label, strings.Join(strings.Fields(detail), " "))
}

func newTelegramClient(relay string) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if relay != "" {
		transport.Proxy = nil
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != "api.telegram.org:443" {
				return nil, errors.New("unexpected Telegram destination")
			}
			return (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, relay)
		}
	}
	return &http.Client{Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
}
func telegramCall(ctx context.Context, token, method string, form url.Values) error {
	return telegramCallUsing(ctx, http.DefaultClient, token, method, form)
}
func telegramCallUsing(ctx context.Context, client *http.Client, token, method string, form url.Values) error {
	req, e := http.NewRequestWithContext(ctx, "POST", "https://api.telegram.org/bot"+token+"/"+method, strings.NewReader(form.Encode()))
	if e != nil {
		return errors.New("invalid Telegram request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r, e := client.Do(req)
	if e != nil {
		return errors.New("Telegram connection failed")
	}
	defer r.Body.Close()
	var result struct {
		OK bool `json:"ok"`
	}
	if e = json.NewDecoder(io.LimitReader(r.Body, 65536)).Decode(&result); e != nil || r.StatusCode != 200 || !result.OK {
		return errors.New("Telegram rejected request")
	}
	return nil
}
func runNotifications(ctx context.Context, c Config, s *Store) {
	if c.Telegram.TokenEnv == "" {
		return
	}
	client := newTelegramClient(c.Telegram.RelayAddress)
	defer client.CloseIdleConnections()
	token, _ := secret(c.Telegram.TokenEnv)
	chat, _ := secret(c.Telegram.ChatIDEnv)
	lastProbe := time.Time{}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if time.Since(lastProbe) >= time.Minute {
			probe, cancel := context.WithTimeout(ctx, 10*time.Second)
			e := telegramCallUsing(probe, client, token, "getMe", url.Values{})
			cancel()
			if e := s.setMeta("telegram", TelegramHealth{OK: e == nil, Checked: time.Now().Unix()}); e != nil {
				log.Print("cannot persist Telegram health")
			}
			lastProbe = time.Now()
		}
		var seq int64
		var body string
		var attempts int
		if s.QueryRow("SELECT seq,body,attempts FROM outbox WHERE next<=? ORDER BY seq LIMIT 1", time.Now().Unix()).Scan(&seq, &body, &attempts) != nil {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		e := telegramCallUsing(callCtx, client, token, "sendMessage", url.Values{"chat_id": {chat}, "text": {body}})
		cancel()
		if e == nil {
			if _, e = s.Exec("DELETE FROM outbox WHERE seq=?", seq); e != nil {
				log.Printf("outbox acknowledgement failed: %v", e)
			}
		} else {
			attempts++
			delay := time.Duration(1<<min(attempts, 9)) * time.Second
			s.Exec("UPDATE outbox SET attempts=?,next=? WHERE seq=?", attempts, time.Now().Add(delay).Unix(), seq)
			log.Print("Telegram delivery failed; retry queued")
		}
	}
}
func sendEmail(ctx context.Context, c SMTP, body string) error {
	// Require STARTTLS and certificate verification before sending any credentials or content.
	d := &netDialer
	conn, e := d.DialContext(ctx, "tcp", c.Address)
	if e != nil {
		return errors.New("SMTP connection failed")
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	host, _, _ := splitHostPort(c.Address)
	client, e := smtp.NewClient(conn, host)
	if e != nil {
		return errors.New("SMTP greeting failed")
	}
	defer client.Close()
	if e = client.StartTLS(tlsConfig(host)); e != nil {
		return errors.New("SMTP STARTTLS failed")
	}
	if c.User != "" {
		password, e := secret(c.PasswordEnv)
		if e != nil {
			return e
		}
		if e = client.Auth(smtp.PlainAuth("", c.User, password, host)); e != nil {
			return errors.New("SMTP authentication failed")
		}
	}
	if e = client.Mail(c.From); e != nil {
		return errors.New("SMTP sender rejected")
	}
	if e = client.Rcpt(c.To); e != nil {
		return errors.New("SMTP recipient rejected")
	}
	w, e := client.Data()
	if e != nil {
		return e
	}
	_, e = fmt.Fprintf(w, "From: %s\r\nTo: %s\r\nSubject: Watchlight watchdog status\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s\r\n", c.From, c.To, body)
	if e != nil {
		return e
	}
	if e = w.Close(); e != nil {
		return errors.New("SMTP submission failed")
	}
	return client.Quit()
}
