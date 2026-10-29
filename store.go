package main

import (
	"context"
	"database/sql"
	"encoding/json"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"time"
)

type Store struct{ *sql.DB }
type State struct {
	ID      string  `json:"id"`
	Status  string  `json:"status"`
	Checked int64   `json:"checked"`
	Latency float64 `json:"latency"`
	Fail    int     `json:"-"`
	Pass    int     `json:"-"`
	Error   string  `json:"error,omitempty"`
}
type Point struct {
	Time    int64   `json:"time"`
	OK      bool    `json:"ok"`
	Latency float64 `json:"latency"`
}
type Incident struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Time   int64  `json:"time"`
}

func openStore(path string) (*Store, error) {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return nil, e
	}
	db, e := sql.Open("sqlite", path)
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	s := &Store{db}
	_, e = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;
CREATE TABLE IF NOT EXISTS states(id TEXT PRIMARY KEY,status TEXT NOT NULL,checked INTEGER NOT NULL,latency REAL NOT NULL,fail INTEGER NOT NULL,pass INTEGER NOT NULL,error TEXT NOT NULL,fingerprint TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS results(id TEXT NOT NULL,time INTEGER NOT NULL,ok INTEGER NOT NULL,latency REAL NOT NULL);
CREATE INDEX IF NOT EXISTS results_time ON results(time);
CREATE INDEX IF NOT EXISTS results_id_time ON results(id,time);
CREATE TABLE IF NOT EXISTS incidents(id TEXT NOT NULL,status TEXT NOT NULL,time INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS outbox(seq INTEGER PRIMARY KEY,body TEXT NOT NULL,attempts INTEGER NOT NULL DEFAULT 0,next INTEGER NOT NULL DEFAULT 0,created INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS hourly(id TEXT NOT NULL,hour INTEGER NOT NULL,total INTEGER NOT NULL,successes INTEGER NOT NULL,latency_sum REAL NOT NULL,latency_max REAL NOT NULL,PRIMARY KEY(id,hour));
CREATE TABLE IF NOT EXISTS heartbeats(id TEXT PRIMARY KEY,time INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS heartbeat_reports(id TEXT PRIMARY KEY,time INTEGER NOT NULL,detail TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS snoozes(id TEXT PRIMARY KEY,armed INTEGER NOT NULL,passes INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS metadata(key TEXT PRIMARY KEY,value TEXT NOT NULL);`)
	if e != nil {
		db.Close()
		return nil, e
	}
	return s, nil
}

// Commit arrival and optional failure detail together. Empty legacy heartbeats
// clear older reports so a recovered sender cannot inherit a stale failure.
func (s *Store) heartbeat(id, detail string) error {
	tx, err := s.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	if _, err = tx.Exec("INSERT INTO heartbeats VALUES(?,?) ON CONFLICT(id) DO UPDATE SET time=excluded.time", id, now); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO heartbeat_reports VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET time=excluded.time,detail=excluded.detail", id, now, detail); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) state(id string) State {
	v := State{ID: id, Status: "unknown"}
	s.QueryRow("SELECT status,checked,latency,fail,pass,error FROM states WHERE id=?", id).Scan(&v.Status, &v.Checked, &v.Latency, &v.Fail, &v.Pass, &v.Error)
	return v
}
func (s *Store) record(m Monitor, ok bool, lat float64, detail string, notify bool) error {
	// Preserve the existing schema without persisting disabled measurements.
	if !m.recordsLatency() {
		lat = 0
	}
	tx, e := s.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	v := State{ID: m.ID, Status: "unknown"}
	var fp string
	e = tx.QueryRow("SELECT status,fail,pass,fingerprint FROM states WHERE id=?", m.ID).Scan(&v.Status, &v.Fail, &v.Pass, &fp)
	if e != nil && e != sql.ErrNoRows {
		return e
	}
	configFP := fingerprint(m)
	if fp != configFP {
		v.Status = "unknown"
		v.Fail = 0
		v.Pass = 0
	}
	var snoozePasses int
	snoozeErr := tx.QueryRow("SELECT passes FROM snoozes WHERE id=?", m.ID).Scan(&snoozePasses)
	if snoozeErr != nil && snoozeErr != sql.ErrNoRows {
		return snoozeErr
	}
	snoozed := snoozeErr == nil
	recovered := false
	if snoozed {
		if ok {
			snoozePasses++
		} else {
			snoozePasses = 0
		}
		recovered = ok && snoozePasses >= m.Successes
		if recovered {
			_, e = tx.Exec("DELETE FROM snoozes WHERE id=?", m.ID)
		} else {
			_, e = tx.Exec("UPDATE snoozes SET passes=? WHERE id=?", snoozePasses, m.ID)
		}
		if e != nil {
			return e
		}
	}
	old := v.Status
	if ok {
		v.Pass++
		v.Fail = 0
		if v.Pass >= m.Successes {
			v.Status = "up"
		}
	} else {
		v.Fail++
		v.Pass = 0
		if v.Fail >= m.Failures {
			v.Status = "down"
		}
	}
	now := time.Now().Unix()
	_, e = tx.Exec("INSERT INTO results VALUES(?,?,?,?)", m.ID, now, ok, lat)
	if e != nil {
		return e
	}
	_, e = tx.Exec(`INSERT INTO states VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET status=excluded.status,checked=excluded.checked,latency=excluded.latency,fail=excluded.fail,pass=excluded.pass,error=excluded.error,fingerprint=excluded.fingerprint`, m.ID, v.Status, now, lat, v.Fail, v.Pass, detail, configFP)
	if e != nil {
		return e
	}
	if v.Status != old {
		if _, e = tx.Exec("INSERT INTO incidents VALUES(?,?,?)", m.ID, v.Status, now); e != nil {
			return e
		}
		if notify && !snoozed && (v.Status == "down" || old == "down") {
			body := "Watchlight: " + m.Name + " is " + v.Status
			if _, e = tx.Exec("INSERT INTO outbox(body,created) VALUES(?,?)", body, now); e != nil {
				return e
			}
		}
	}
	if notify && recovered {
		if _, e = tx.Exec("INSERT INTO outbox(body,created) VALUES(?,?)", "Watchlight: "+m.Name+" recovered and is visible again", now); e != nil {
			return e
		}
	}
	return tx.Commit()
}

// A snooze is a one-time persisted action; old successes cannot release it.
func (s *Store) hideUntilOK(id string) error {
	_, err := s.Exec("INSERT INTO snoozes(id,armed,passes) VALUES(?,?,0) ON CONFLICT(id) DO UPDATE SET armed=excluded.armed,passes=0", id, time.Now().Unix())
	return err
}
func (s *Store) snoozesContext(ctx context.Context) (map[string]bool, error) {
	rows, err := s.QueryContext(ctx, "SELECT id FROM snoozes")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hidden := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		hidden[id] = true
	}
	return hidden, rows.Err()
}
func (s *Store) incidents(ctx context.Context, id string) ([]Incident, error) {
	r, e := s.QueryContext(ctx, "SELECT id,status,time FROM incidents WHERE id=? ORDER BY time DESC LIMIT 30", id)
	if e != nil {
		return nil, e
	}
	defer r.Close()
	v := []Incident{}
	for r.Next() {
		var x Incident
		if e = r.Scan(&x.ID, &x.Status, &x.Time); e != nil {
			return nil, e
		}
		v = append(v, x)
	}
	return v, r.Err()
}
func (s *Store) prune(days int) error {
	cut := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Truncate(time.Hour).Unix()
	for {
		var hour int64
		if e := s.QueryRow("SELECT (time/3600)*3600 FROM results WHERE time<? ORDER BY time LIMIT 1", cut).Scan(&hour); e == sql.ErrNoRows {
			break
		} else if e != nil {
			return e
		}
		tx, e := s.Begin()
		if e != nil {
			return e
		}
		_, e = tx.Exec("INSERT INTO hourly SELECT id,?,count(*),sum(ok),sum(latency),max(latency) FROM results WHERE time>=? AND time<? GROUP BY id ON CONFLICT(id,hour) DO UPDATE SET total=excluded.total,successes=excluded.successes,latency_sum=excluded.latency_sum,latency_max=excluded.latency_max", hour, hour, hour+3600)
		if e == nil {
			_, e = tx.Exec("DELETE FROM results WHERE time>=? AND time<?", hour, hour+3600)
		}
		if e != nil {
			tx.Rollback()
			return e
		}
		if e = tx.Commit(); e != nil {
			return e
		}
	}
	year := time.Now().Add(-365 * 24 * time.Hour).Unix()
	if _, e := s.Exec("DELETE FROM hourly WHERE hour<?", year); e != nil {
		return e
	}
	_, e := s.Exec("DELETE FROM incidents WHERE time<?", year)
	return e
}
func (s *Store) getMeta(key string, v any) {
	var b string
	if s.QueryRow("SELECT value FROM metadata WHERE key=?", key).Scan(&b) == nil {
		json.Unmarshal([]byte(b), v)
	}
}
func (s *Store) setMeta(key string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	_, e = s.Exec("INSERT INTO metadata VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, string(b))
	return e
}

// WindowBucket bounds the response size independently of check cadence.
type WindowBucket struct {
	Time     int64   `json:"time"`
	End      int64   `json:"end"`
	Checks   int     `json:"checks"`
	Failures int     `json:"failures"`
	Up       float64 `json:"up"`
	Down     float64 `json:"down"`
	Latency  float64 `json:"latency"`
}

func (s *Store) windowHistory(ctx context.Context, id string, start, end int64, cadence float64) ([]WindowBucket, int, int, error) {
	const count = 240
	width := float64(end-start) / count
	buckets := make([]WindowBucket, count)
	for i := range buckets {
		buckets[i].Time = start + int64(float64(i)*width)
		buckets[i].End = start + int64(float64(i+1)*width)
	}
	rows, err := s.QueryContext(ctx, "SELECT time,ok,latency FROM results WHERE id=? AND time>=? AND time<=? ORDER BY time", id, start-int64(cadence)-1, end)
	if err != nil {
		return nil, 0, 0, err
	}
	defer rows.Close()
	total, successes := 0, 0
	var previous *Point
	addCoverage := func(p Point, until float64) {
		left := max(float64(start), float64(p.Time))
		right := min(float64(end), float64(p.Time)+cadence, until)
		for left < right {
			index := min(count-1, int((left-float64(start))/width))
			stop := min(right, float64(buckets[index].End))
			if stop <= left {
				break
			}
			if p.OK {
				buckets[index].Up += stop - left
			} else {
				buckets[index].Down += stop - left
			}
			left = stop
		}
	}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, 0, 0, err
		}
		var point Point
		if err := rows.Scan(&point.Time, &point.OK, &point.Latency); err != nil {
			return nil, 0, 0, err
		}
		if previous != nil {
			addCoverage(*previous, float64(point.Time))
		}
		previous = &point
		if point.Time < start {
			continue
		}
		index := min(count-1, int(float64(point.Time-start)/width))
		buckets[index].Checks++
		buckets[index].Latency += point.Latency
		total++
		if point.OK {
			successes++
		} else {
			buckets[index].Failures++
		}
	}
	if previous != nil {
		addCoverage(*previous, float64(end))
	}
	for i := range buckets {
		if buckets[i].Checks > 0 {
			buckets[i].Latency /= float64(buckets[i].Checks)
		}
	}
	return buckets, total, successes, rows.Err()
}

func (s *Store) statesContext(ctx context.Context) (map[string]State, error) {
	rows, err := s.QueryContext(ctx, "SELECT id,status,checked,latency,fail,pass,error FROM states")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	states := map[string]State{}
	for rows.Next() {
		var v State
		if err := rows.Scan(&v.ID, &v.Status, &v.Checked, &v.Latency, &v.Fail, &v.Pass, &v.Error); err != nil {
			return nil, err
		}
		states[v.ID] = v
	}
	return states, rows.Err()
}
func (s *Store) stateContext(ctx context.Context, id string) (State, error) {
	v := State{ID: id, Status: "unknown"}
	err := s.QueryRowContext(ctx, "SELECT status,checked,latency,fail,pass,error FROM states WHERE id=?", id).Scan(&v.Status, &v.Checked, &v.Latency, &v.Fail, &v.Pass, &v.Error)
	if err == sql.ErrNoRows {
		err = nil
	}
	return v, err
}
