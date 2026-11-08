package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
)

// Keep the original body for readable legacy queue entries and store structured
// events transactionally so unsent transitions can be summarized without parsing.
func enqueueNotification(tx *sql.Tx, m Monitor, status string, latency float64, detail string, now int64, suffix string) error {
	down := now
	if status == "up" {
		if err := tx.QueryRow("SELECT coalesce(max(time),0) FROM incidents WHERE id=? AND status='down'", m.ID).Scan(&down); err != nil {
			return err
		}
	}
	result, err := tx.Exec("INSERT INTO outbox(body,created) VALUES(?,?)", notificationBody(m, status, latency, detail)+suffix, now)
	if err != nil {
		return err
	}
	seq, err := result.LastInsertId()
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO outbox_events VALUES(?,?,?,?,?)", seq, m.ID, m.Name, status, down)
	return err
}

type telegramError struct {
	RateLimited bool
	RetryAfter  int64
}

func (e *telegramError) Error() string {
	if e.RateLimited {
		return "Telegram rate limit"
	}
	return "Telegram rejected request"
}

type deliveryState struct {
	Attempts     int   `json:"attempts"`
	Next         int64 `json:"next"`
	DelayedSince int64 `json:"delayed_since"`
}

// Back off the destination, not individual messages. New events must not bypass
// a Telegram cooldown or overtake an older undelivered incident.
func (d *deliveryState) failed(now int64, err error) {
	if d.DelayedSince == 0 {
		d.DelayedSince = now
	}
	d.Attempts = min(d.Attempts+1, 6)
	delay := min(int64(1<<d.Attempts), int64(60))
	var te *telegramError
	if errors.As(err, &te) && te.RateLimited {
		delay = max(delay, te.RetryAfter)
	}
	// Telegram returns seconds, not an absolute timestamp. Bound arithmetic only;
	// do not shorten a valid server-requested cooldown.
	delay = min(delay, math.MaxInt64-now)
	d.Next = now + delay
}

type queuedGroup struct {
	ID        string
	Name      string
	Status    string
	Body      string
	FirstDown int64
	Downtime  int64
	Last      int64
	DownSince int64
	Count     int
	Seqs      []int64
}
type notificationBatch struct {
	Body string
	Seqs []int64
}

type alertClock struct {
	Location *time.Location
	Label    string
}

func (c alertClock) format(t int64) string {
	location := c.Location
	if location == nil {
		location = time.UTC
	}
	stamp := time.Unix(t, 0).In(location)
	if c.Label != "" {
		return stamp.Format("02 Jan 15:04:05") + " " + c.Label
	}
	return stamp.Format("02 Jan 15:04:05 MST")
}
func cleanAlert(s string) string { return strings.Join(strings.Fields(s), " ") }
func observedDuration(start, end int64) string {
	return (time.Duration(max(0, end-start)) * time.Second).String()
}

func (g queuedGroup) line(clock alertClock) string {
	if g.ID == "" {
		return cleanAlert(g.Body) + " · " + clock.format(g.Last)
	}
	changes := ""
	if g.Count > 2 {
		changes = fmt.Sprintf("; %d transitions", g.Count)
	}
	if g.Status == "down" {
		return cleanAlert(g.Body) + " · down since " + clock.format(g.DownSince) + changes
	}
	if g.Count > 1 && g.DownSince > 0 {
		return fmt.Sprintf("✅ %s — failed and recovered; %s – %s; total observed downtime %s%s", cleanAlert(g.Name), clock.format(g.FirstDown), clock.format(g.Last), observedDuration(0, g.Downtime), changes) + visibilityNote(g.Body)
	}
	line := cleanAlert(g.Body) + " · recovered " + clock.format(g.Last)
	if g.DownSince > 0 {
		line += "; observed outage " + observedDuration(g.DownSince, g.Last)
	}
	return line
}
func visibilityNote(body string) string {
	if strings.HasSuffix(body, "; recovered and is visible again") {
		return "; visible again"
	}
	return ""
}

// Telegram measures text in UTF-16 code units. Leave room below its 4096 limit
// and never split a surrogate pair or drop an oversized monitor entirely.
func telegramText(s string, limit int) string {
	units := 0
	for i, r := range s {
		n := utf16.RuneLen(r)
		if units+n > limit-1 {
			return s[:i] + "…"
		}
		units += n
	}
	return s
}
func telegramLength(s string) int { return len(utf16.Encode([]rune(s))) }

func (s *Store) notificationBatch(now int64, delayed bool, clock alertClock) (notificationBatch, error) {
	rows, err := s.Query(`SELECT o.seq,o.body,o.created,coalesce(e.monitor_id,''),coalesce(e.name,''),coalesce(e.status,''),coalesce(e.down_since,0)
 FROM outbox o LEFT JOIN outbox_events e ON e.seq=o.seq ORDER BY o.seq`)
	if err != nil {
		return notificationBatch{}, err
	}
	defer rows.Close()
	groups := map[string]*queuedGroup{}
	oldest := int64(0)
	newest := int64(0)
	for rows.Next() {
		var seq, created, down int64
		var body, id, name, status string
		if err = rows.Scan(&seq, &body, &created, &id, &name, &status, &down); err != nil {
			return notificationBatch{}, err
		}
		if oldest == 0 || created < oldest {
			oldest = created
		}
		newest = max(newest, created)
		key := "monitor:" + id
		if id == "" {
			key = fmt.Sprintf("legacy:%d", seq)
		}
		g := groups[key]
		if g == nil {
			g = &queuedGroup{ID: id}
			groups[key] = g
		}
		g.Name = name
		if g.Status != status || g.Count == 0 {
			g.Count++
			if status == "up" && down > 0 {
				g.Downtime += max(0, created-down)
			}
		}
		g.Status = status
		g.Body = body
		g.Last = created
		g.Seqs = append(g.Seqs, seq)
		g.DownSince = down
		if down > 0 && (g.FirstDown == 0 || down < g.FirstDown) {
			g.FirstDown = down
		}
	}
	if err = rows.Err(); err != nil {
		return notificationBatch{}, err
	}
	if len(groups) == 0 || (!delayed && now < oldest+10) {
		return notificationBatch{}, nil
	}
	sorted := make([]*queuedGroup, 0, len(groups))
	for _, g := range groups {
		sorted = append(sorted, g)
	}
	sort.Slice(sorted, func(i, j int) bool {
		if (sorted[i].Status == "down") != (sorted[j].Status == "down") {
			return sorted[i].Status == "down"
		}
		return sorted[i].Seqs[0] < sorted[j].Seqs[0]
	})
	header := "Alerts"
	if delayed {
		header = "Delayed alerts"
	}
	header += " · " + clock.format(oldest) + " – " + clock.format(newest)
	if len(sorted) == 1 {
		body := telegramText(sorted[0].line(clock), 3500)
		if delayed {
			body += " · delayed delivery"
		}
		return notificationBatch{Body: body, Seqs: sorted[0].Seqs}, nil
	}
	var batch notificationBatch
	batch.Body = header
	selected := 0
	for _, g := range sorted {
		line := telegramText(g.line(clock), 3000)
		if telegramLength(batch.Body)+telegramLength(line)+1 > 3500 {
			break
		}
		batch.Body += "\n" + line
		batch.Seqs = append(batch.Seqs, g.Seqs...)
		selected++
	}
	if selected < len(sorted) {
		batch.Body += fmt.Sprintf("\n%d more monitors/events queued.", len(sorted)-selected)
	}
	return batch, nil
}

func (s *Store) acknowledgeBatch(batch notificationBatch, state deliveryState) error {
	tx, err := s.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, seq := range batch.Seqs {
		if _, err = tx.Exec("DELETE FROM outbox WHERE seq=?", seq); err != nil {
			return err
		}
	}
	var remaining int
	if err = tx.QueryRow("SELECT count(*) FROM outbox").Scan(&remaining); err != nil {
		return err
	}
	if remaining == 0 {
		state.DelayedSince = 0
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO metadata VALUES('telegram_delivery',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", string(data)); err != nil {
		return err
	}
	return tx.Commit()
}

type notificationWorker struct {
	store      *Store
	call       func(context.Context, string, url.Values) error
	chat       string
	lastProbe  int64
	finishedAt func() time.Time
	clock      alertClock
}

func (w *notificationWorker) tick(ctx context.Context, now int64) error {
	var state deliveryState
	var data string
	err := w.store.QueryRow("SELECT value FROM metadata WHERE key='telegram_delivery'").Scan(&data)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil {
		if err = json.Unmarshal([]byte(data), &state); err != nil {
			return err
		}
	}
	if now < state.Next {
		return nil
	}
	batch, err := w.store.notificationBatch(now, state.DelayedSince != 0, w.clock)
	if err != nil {
		return err
	}
	method := "sendMessage"
	form := url.Values{"chat_id": {w.chat}, "text": {batch.Body}}
	if len(batch.Seqs) == 0 {
		if now-w.lastProbe < 60 {
			return nil
		}
		method = "getMe"
		form = url.Values{}
	}
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = w.call(callCtx, method, form)
	cancel()
	if w.finishedAt != nil {
		finished := w.finishedAt()
		now = finished.Unix()
		if finished.Nanosecond() != 0 {
			now++
		}
	}
	w.lastProbe = now
	// Round completion up to avoid shortening cooldowns through second precision.
	if e := w.store.setMeta("telegram", TelegramHealth{OK: err == nil, Checked: now}); e != nil {
		return e
	}
	if err != nil {
		state.failed(now, err)
		if e := w.store.setMeta("telegram_delivery", state); e != nil {
			return e
		}
		log.Print("Telegram delivery unavailable; shared retry queued")
		return nil
	}
	state.Attempts = 0
	state.Next = now + 3
	if method == "sendMessage" {
		return w.store.acknowledgeBatch(batch, state)
	}
	// An empty acknowledgement clears the delayed marker only if no events exist.
	return w.store.acknowledgeBatch(notificationBatch{}, state)
}

func runNotifications(ctx context.Context, c Config, s *Store) {
	if c.Telegram.TokenEnv == "" {
		return
	}
	client := newTelegramClient(c.Telegram.RelayAddress)
	defer client.CloseIdleConnections()
	token, _ := secret(c.Telegram.TokenEnv)
	chat, _ := secret(c.Telegram.ChatIDEnv)
	clock, err := c.Telegram.alertClock()
	if err != nil {
		log.Print("Invalid Telegram timezone")
		return
	}
	worker := notificationWorker{store: s, chat: chat, clock: clock, finishedAt: time.Now, call: func(ctx context.Context, method string, form url.Values) error {
		return telegramCallUsing(ctx, client, token, method, form)
	}}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := worker.tick(ctx, time.Now().Unix()); err != nil {
			log.Print("Telegram queue processing failed")
		}
	}
}
