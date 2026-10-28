package main

import (
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
		if notify && (v.Status == "down" || old == "down") {
			body := "Watchlight: " + m.Name + " is " + v.Status
			if _, e = tx.Exec("INSERT INTO outbox(body,created) VALUES(?,?)", body, now); e != nil {
				return e
			}
		}
	}
	return tx.Commit()
}
func (s *Store) history(id string) ([]Point, error) {
	rows, e := s.Query("SELECT time,ok,latency FROM results WHERE id=? ORDER BY time DESC LIMIT 240", id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	v := []Point{}
	for rows.Next() {
		var p Point
		if e = rows.Scan(&p.Time, &p.OK, &p.Latency); e != nil {
			return nil, e
		}
		v = append(v, p)
	}
	return v, rows.Err()
}
func (s *Store) incidents(id string) ([]Incident, error) {
	r, e := s.Query("SELECT id,status,time FROM incidents WHERE id=? ORDER BY time DESC LIMIT 30", id)
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
