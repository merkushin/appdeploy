// Package store keeps the agent's durable state in SQLite: apps, releases,
// deploys, the deploy event journal and the containers the agent started.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")
var ErrExists = errors.New("already exists")

// Deploy states. Terminal states are succeeded, failed, rolled_back and
// superseded.
const (
	StateQueued       = "queued"
	StateValidating   = "validating"
	StateBuilding     = "building"
	StateMigrating    = "migrating"
	StateStarting     = "starting"
	StateVerifying    = "verifying"
	StateSwitching    = "switching"
	StateDistributing = "distributing"
	StateRolling      = "rolling"
	StateObserving    = "observing"
	StateSucceeded    = "succeeded"
	StateFailed       = "failed"
	StateRolledBack   = "rolled_back"
	StateSuperseded   = "superseded"
)

// IsTerminal reports whether a deploy in this state is finished.
func IsTerminal(state string) bool {
	switch state {
	case StateSucceeded, StateFailed, StateRolledBack, StateSuperseded:
		return true
	}
	return false
}

type App struct {
	Name           string
	Domains        []string
	Branch         string
	CurrentRelease int // 0 when nothing is deployed yet
	CreatedAt      time.Time
}

type Release struct {
	App       string
	Number    int
	Image     string
	Commit    string
	Config    json.RawMessage // resolved app config, as JSON
	CreatedAt time.Time
}

type Deploy struct {
	ID         string
	App        string
	Release    int
	State      string
	Error      string
	Ref        string
	Source     string // push | cli | rollback | resume
	Attempts   int
	StartedAt  time.Time
	FinishedAt time.Time
}

type Event struct {
	ID       int64
	DeployID string
	Time     time.Time
	Msg      string
}

type Container struct {
	DockerID string
	Name     string
	App      string
	Release  int
	Role     string
	HostPort int // 0 when nothing is published
	Memory   int64
}

type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS apps (
  name TEXT PRIMARY KEY,
  domains TEXT NOT NULL DEFAULT '[]',
  branch TEXT NOT NULL DEFAULT 'main',
  current_release INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS releases (
  app TEXT NOT NULL,
  number INTEGER NOT NULL,
  image TEXT NOT NULL,
  commit_sha TEXT NOT NULL DEFAULT '',
  config TEXT NOT NULL DEFAULT '{}',
  created_at INTEGER NOT NULL,
  PRIMARY KEY (app, number)
);
CREATE TABLE IF NOT EXISTS deploys (
  id TEXT PRIMARY KEY,
  app TEXT NOT NULL,
  release INTEGER NOT NULL,
  state TEXT NOT NULL,
  error TEXT NOT NULL DEFAULT '',
  ref TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL DEFAULT '',
  attempts INTEGER NOT NULL DEFAULT 0,
  started_at INTEGER NOT NULL,
  finished_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS deploys_app ON deploys(app, started_at);
CREATE TABLE IF NOT EXISTS payloads (
  deploy_id TEXT PRIMARY KEY,
  data TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  deploy_id TEXT NOT NULL,
  ts INTEGER NOT NULL,
  msg TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS events_deploy ON events(deploy_id, id);
CREATE TABLE IF NOT EXISTS containers (
  docker_id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  app TEXT NOT NULL,
  release INTEGER NOT NULL,
  role TEXT NOT NULL,
  host_port INTEGER NOT NULL DEFAULT 0,
  memory INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS containers_app ON containers(app, release);
`

// Open opens (and migrates) the database at path. Use ":memory:" in tests.
func Open(path string) (*Store, error) {
	dsn := path
	if path != ":memory:" {
		dsn = "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection keeps SQLite simple and serializes writers.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func ts(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromTS(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// ---- apps ----

func (s *Store) CreateApp(a App) error {
	domains, _ := json.Marshal(a.Domains)
	if a.Branch == "" {
		a.Branch = "main"
	}
	_, err := s.db.Exec(`INSERT INTO apps(name, domains, branch, created_at) VALUES(?,?,?,?)`,
		a.Name, string(domains), a.Branch, time.Now().UnixMilli())
	if isUnique(err) {
		return ErrExists
	}
	return err
}

func scanApp(row interface{ Scan(...any) error }) (App, error) {
	var a App
	var domains string
	var created int64
	if err := row.Scan(&a.Name, &domains, &a.Branch, &a.CurrentRelease, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return a, ErrNotFound
		}
		return a, err
	}
	_ = json.Unmarshal([]byte(domains), &a.Domains)
	a.CreatedAt = fromTS(created)
	return a, nil
}

const appCols = `name, domains, branch, current_release, created_at`

func (s *Store) GetApp(name string) (App, error) {
	return scanApp(s.db.QueryRow(`SELECT `+appCols+` FROM apps WHERE name=?`, name))
}

func (s *Store) ListApps() ([]App, error) {
	rows, err := s.db.Query(`SELECT ` + appCols + ` FROM apps ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) SetDomains(name string, domains []string) error {
	b, _ := json.Marshal(domains)
	res, err := s.db.Exec(`UPDATE apps SET domains=? WHERE name=?`, string(b), name)
	return affected(res, err)
}

func (s *Store) SetCurrentRelease(name string, n int) error {
	res, err := s.db.Exec(`UPDATE apps SET current_release=? WHERE name=?`, n, name)
	return affected(res, err)
}

// DeleteApp removes the app and all of its rows.
func (s *Store) DeleteApp(name string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM events WHERE deploy_id IN (SELECT id FROM deploys WHERE app=?)`,
		`DELETE FROM deploys WHERE app=?`,
		`DELETE FROM containers WHERE app=?`,
		`DELETE FROM releases WHERE app=?`,
	} {
		if _, err := tx.Exec(q, name); err != nil {
			return err
		}
	}
	res, err := tx.Exec(`DELETE FROM apps WHERE name=?`, name)
	if err := affected(res, err); err != nil {
		return err
	}
	return tx.Commit()
}

func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- releases ----

// NextReleaseNumber returns one more than the highest release or deploy
// number the app has used, so numbers never repeat.
func (s *Store) NextReleaseNumber(app string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT MAX(
	  COALESCE((SELECT MAX(number) FROM releases WHERE app=?),0),
	  COALESCE((SELECT MAX(release) FROM deploys WHERE app=?),0))`, app, app).Scan(&n)
	return n + 1, err
}

func (s *Store) PutRelease(r Release) error {
	cfg := string(r.Config)
	if cfg == "" {
		cfg = "{}"
	}
	_, err := s.db.Exec(`INSERT OR REPLACE INTO releases(app, number, image, commit_sha, config, created_at)
	  VALUES(?,?,?,?,?,?)`, r.App, r.Number, r.Image, r.Commit, cfg, time.Now().UnixMilli())
	return err
}

func scanRelease(row interface{ Scan(...any) error }) (Release, error) {
	var r Release
	var cfg string
	var created int64
	if err := row.Scan(&r.App, &r.Number, &r.Image, &r.Commit, &cfg, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return r, ErrNotFound
		}
		return r, err
	}
	r.Config = json.RawMessage(cfg)
	r.CreatedAt = fromTS(created)
	return r, nil
}

const releaseCols = `app, number, image, commit_sha, config, created_at`

func (s *Store) GetRelease(app string, n int) (Release, error) {
	return scanRelease(s.db.QueryRow(`SELECT `+releaseCols+` FROM releases WHERE app=? AND number=?`, app, n))
}

// ListReleases returns the app's releases, newest first.
func (s *Store) ListReleases(app string) ([]Release, error) {
	rows, err := s.db.Query(`SELECT `+releaseCols+` FROM releases WHERE app=? ORDER BY number DESC`, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Release
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) DeleteRelease(app string, n int) error {
	_, err := s.db.Exec(`DELETE FROM releases WHERE app=? AND number=?`, app, n)
	return err
}

// ---- deploys ----

func (s *Store) CreateDeploy(d Deploy) error {
	_, err := s.db.Exec(`INSERT INTO deploys(id, app, release, state, error, ref, source, attempts, started_at)
	  VALUES(?,?,?,?,?,?,?,?,?)`, d.ID, d.App, d.Release, d.State, d.Error, d.Ref, d.Source, d.Attempts, time.Now().UnixMilli())
	return err
}

func scanDeploy(row interface{ Scan(...any) error }) (Deploy, error) {
	var d Deploy
	var started, finished int64
	if err := row.Scan(&d.ID, &d.App, &d.Release, &d.State, &d.Error, &d.Ref, &d.Source, &d.Attempts, &started, &finished); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return d, ErrNotFound
		}
		return d, err
	}
	d.StartedAt = fromTS(started)
	d.FinishedAt = fromTS(finished)
	return d, nil
}

const deployCols = `id, app, release, state, error, ref, source, attempts, started_at, finished_at`

func (s *Store) GetDeploy(id string) (Deploy, error) {
	return scanDeploy(s.db.QueryRow(`SELECT `+deployCols+` FROM deploys WHERE id=?`, id))
}

// SetDeployState records a state transition. The journal write happens before
// the step it names runs, so a crash always leaves the last attempted step.
func (s *Store) SetDeployState(id, state, errMsg string) error {
	var finished int64
	if IsTerminal(state) {
		finished = time.Now().UnixMilli()
	}
	res, err := s.db.Exec(`UPDATE deploys SET state=?, error=?, finished_at=? WHERE id=?`, state, errMsg, finished, id)
	return affected(res, err)
}

func (s *Store) SetDeployRelease(id string, release int) error {
	res, err := s.db.Exec(`UPDATE deploys SET release=? WHERE id=?`, release, id)
	return affected(res, err)
}

func (s *Store) BumpAttempts(id string) (int, error) {
	if _, err := s.db.Exec(`UPDATE deploys SET attempts=attempts+1 WHERE id=?`, id); err != nil {
		return 0, err
	}
	var n int
	err := s.db.QueryRow(`SELECT attempts FROM deploys WHERE id=?`, id).Scan(&n)
	return n, err
}

// UnfinishedDeploys lists deploys that are not in a terminal state, oldest first.
func (s *Store) UnfinishedDeploys() ([]Deploy, error) {
	rows, err := s.db.Query(`SELECT ` + deployCols + ` FROM deploys
	  WHERE state NOT IN ('succeeded','failed','rolled_back','superseded') ORDER BY started_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deploy
	for rows.Next() {
		d, err := scanDeploy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListDeploys returns the app's most recent deploys, newest first.
func (s *Store) ListDeploys(app string, limit int) ([]Deploy, error) {
	rows, err := s.db.Query(`SELECT `+deployCols+` FROM deploys WHERE app=? ORDER BY started_at DESC, id DESC LIMIT ?`, app, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deploy
	for rows.Next() {
		d, err := scanDeploy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---- events ----

func (s *Store) AddEvent(deployID, msg string) error {
	_, err := s.db.Exec(`INSERT INTO events(deploy_id, ts, msg) VALUES(?,?,?)`, deployID, time.Now().UnixMilli(), msg)
	return err
}

// EventsAfter returns events of a deploy with id > after, in order.
func (s *Store) EventsAfter(deployID string, after int64) ([]Event, error) {
	rows, err := s.db.Query(`SELECT id, deploy_id, ts, msg FROM events WHERE deploy_id=? AND id>? ORDER BY id`, deployID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var t int64
		if err := rows.Scan(&e.ID, &e.DeployID, &t, &e.Msg); err != nil {
			return nil, err
		}
		e.Time = fromTS(t)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- containers ----

func (s *Store) AddContainer(c Container) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO containers(docker_id, name, app, release, role, host_port, memory)
	  VALUES(?,?,?,?,?,?,?)`, c.DockerID, c.Name, c.App, c.Release, c.Role, c.HostPort, c.Memory)
	return err
}

func (s *Store) RemoveContainer(dockerID string) error {
	_, err := s.db.Exec(`DELETE FROM containers WHERE docker_id=?`, dockerID)
	return err
}

func (s *Store) listContainers(where string, args ...any) ([]Container, error) {
	rows, err := s.db.Query(`SELECT docker_id, name, app, release, role, host_port, memory FROM containers `+where+` ORDER BY app, release, role, name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Container
	for rows.Next() {
		var c Container
		if err := rows.Scan(&c.DockerID, &c.Name, &c.App, &c.Release, &c.Role, &c.HostPort, &c.Memory); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) AllContainers() ([]Container, error) { return s.listContainers("") }

func (s *Store) AppContainers(app string) ([]Container, error) {
	return s.listContainers("WHERE app=?", app)
}

func (s *Store) ReleaseContainers(app string, release int) ([]Container, error) {
	return s.listContainers("WHERE app=? AND release=?", app, release)
}

// ---- payloads (what a coordinator asked this server to deploy) ----

func (s *Store) PutPayload(deployID string, data []byte) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO payloads(deploy_id, data) VALUES(?,?)`, deployID, string(data))
	return err
}

func (s *Store) GetPayload(deployID string) ([]byte, error) {
	var d string
	err := s.db.QueryRow(`SELECT data FROM payloads WHERE deploy_id=?`, deployID).Scan(&d)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return []byte(d), err
}
