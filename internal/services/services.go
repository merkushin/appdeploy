// Package services runs the shared Postgres and Redis of a server and gives
// each app its own database in them.
package services

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"appdeploy/internal/docker"
)

const (
	PostgresImage = "postgres:17-alpine"
	RedisImage    = "redis:7-alpine"
)

// Manager owns the service containers of one agent instance.
type Manager struct {
	Dk       *docker.Client
	Instance string
	DataDir  string

	mu sync.Mutex
}

func (m *Manager) network() string   { return m.Instance + "-services" }
func (m *Manager) pgName() string    { return m.Instance + "-svc-postgres" }
func (m *Manager) redisName() string { return m.Instance + "-svc-redis" }
func (m *Manager) labels(kind string) map[string]string {
	return map[string]string{"appdeploy.instance": m.Instance, "appdeploy.service": kind}
}
func (m *Manager) dir(parts ...string) string {
	return filepath.Join(append([]string{m.DataDir, "services"}, parts...)...)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var nonIdent = regexp.MustCompile(`[^a-z0-9]+`)

// Ident is the database and role name for an app.
func Ident(app string) string { return "app_" + nonIdent.ReplaceAllString(strings.ToLower(app), "_") }

// readKV reads a small KEY=value file; missing gives nil.
func readKV(path string) map[string]string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, l := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			out[k] = v
		}
	}
	return out
}

func writeKV(path string, kv map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var b bytes.Buffer
	for k, v := range kv {
		fmt.Fprintf(&b, "%s=%s\n", k, v)
	}
	return os.WriteFile(path, b.Bytes(), 0o600)
}

// ensureContainer starts the container if it is missing or stopped.
func (m *Manager) ensureContainer(ctx context.Context, name string, spec docker.RunSpec) error {
	if err := m.Dk.EnsureNetwork(ctx, m.network(), map[string]string{"appdeploy.instance": m.Instance}); err != nil {
		return err
	}
	exists, running := m.Dk.ContainerRunning(ctx, name)
	switch {
	case running:
		return nil
	case exists:
		return m.Dk.StartContainer(ctx, name)
	}
	_, err := m.Dk.Run(ctx, spec)
	return err
}

// ---- Postgres ----

func (m *Manager) pgExec(ctx context.Context, env []string, stdin string, args ...string) (string, error) {
	var out, errb bytes.Buffer
	var in *strings.Reader
	var err error
	if stdin != "" {
		in = strings.NewReader(stdin)
		err = m.Dk.ExecIO(ctx, m.pgName(), env, args, in, &out, &errb)
	} else {
		err = m.Dk.ExecIO(ctx, m.pgName(), env, args, nil, &out, &errb)
	}
	if err != nil {
		return out.String(), fmt.Errorf("%s: %s", err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// EnsurePostgres starts the shared Postgres and waits until it accepts TCP connections.
func (m *Manager) EnsurePostgres(ctx context.Context) (adminPassword string, err error) {
	adminFile := m.dir("postgres-admin.env")
	kv := readKV(adminFile)
	if kv == nil || kv["POSTGRES_PASSWORD"] == "" {
		kv = map[string]string{"POSTGRES_PASSWORD": randHex(16)}
		if err := writeKV(adminFile, kv); err != nil {
			return "", err
		}
	}
	envFile := m.dir("postgres-run.env")
	if err := writeKV(envFile, kv); err != nil {
		return "", err
	}
	defer os.Remove(envFile)
	if err := m.ensureContainer(ctx, m.pgName(), docker.RunSpec{
		Name: m.pgName(), Image: PostgresImage, Network: m.network(), EnvFile: envFile,
		Memory: 512 << 20, Volumes: []string{m.Instance + "-svc-postgres-data:/var/lib/postgresql/data"},
		Labels: m.labels("postgres"),
	}); err != nil {
		return "", fmt.Errorf("starting postgres: %w", err)
	}
	// The image first runs a temporary server that only listens on a unix
	// socket, so ask over TCP to know the real one is up.
	deadline := time.Now().Add(90 * time.Second)
	for {
		if _, err := m.pgExec(ctx, []string{"PGPASSWORD=" + kv["POSTGRES_PASSWORD"]}, "", "psql", "-h", "127.0.0.1", "-U", "postgres", "-tAc", "select 1"); err == nil {
			return kv["POSTGRES_PASSWORD"], nil
		}
		if time.Now().After(deadline) {
			return "", errors.New("postgres did not become ready within 90s")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// PostgresCreds are an app's connection details.
type PostgresCreds struct{ Role, Password, Database string }

func (m *Manager) pgCreds(app string) PostgresCreds {
	id := Ident(app)
	return PostgresCreds{Role: id, Database: id, Password: readKV(m.dir("postgres", app+".env"))["PASSWORD"]}
}

// ProvisionPostgres makes sure the app has a database and a role owning it,
// and returns DATABASE_URL. Calling it again returns the same credentials.
func (m *Manager) ProvisionPostgres(ctx context.Context, app, appNetwork string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	admin, err := m.EnsurePostgres(ctx)
	if err != nil {
		return nil, err
	}
	c := m.pgCreds(app)
	if c.Password == "" {
		c.Password = randHex(16)
		if err := writeKV(m.dir("postgres", app+".env"), map[string]string{"PASSWORD": c.Password}); err != nil {
			return nil, err
		}
	}
	sql := fmt.Sprintf(`DO $$ BEGIN
IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='%[1]s') THEN CREATE ROLE %[1]s LOGIN PASSWORD '%[2]s'; END IF;
END $$;
SELECT 'CREATE DATABASE %[3]s OWNER %[1]s' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname='%[3]s')\gexec
`, c.Role, c.Password, c.Database)
	if _, err := m.pgExec(ctx, []string{"PGPASSWORD=" + admin}, sql, "psql", "-h", "127.0.0.1", "-U", "postgres", "-v", "ON_ERROR_STOP=1"); err != nil {
		return nil, fmt.Errorf("creating database for %s: %w", app, err)
	}
	if err := m.Dk.ConnectNetwork(ctx, appNetwork, m.pgName()); err != nil {
		return nil, err
	}
	return map[string]string{
		"DATABASE_URL": fmt.Sprintf("postgres://%s:%s@%s:5432/%s?sslmode=disable", c.Role, c.Password, m.pgName(), c.Database),
	}, nil
}

// ---- Redis ----

// ProvisionRedis gives the app its own Redis database number and returns REDIS_URL.
// Database numbers separate keys but are not a security boundary: every app
// shares the server password.
func (m *Manager) ProvisionRedis(ctx context.Context, app, appNetwork string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	passFile := m.dir("redis-admin.env")
	kv := readKV(passFile)
	if kv == nil || kv["PASSWORD"] == "" {
		kv = map[string]string{"PASSWORD": randHex(16)}
		if err := writeKV(passFile, kv); err != nil {
			return nil, err
		}
	}
	if err := m.ensureContainer(ctx, m.redisName(), docker.RunSpec{
		Name: m.redisName(), Image: RedisImage, Network: m.network(), Memory: 192 << 20,
		Cmd:     []string{"redis-server", "--requirepass", kv["PASSWORD"], "--appendonly", "yes", "--maxmemory", "128mb", "--maxmemory-policy", "noeviction"},
		Volumes: []string{m.Instance + "-svc-redis-data:/data"}, Labels: m.labels("redis"),
	}); err != nil {
		return nil, fmt.Errorf("starting redis: %w", err)
	}
	dbFile := m.dir("redis", app+".env")
	idx := readKV(dbFile)["DB"]
	if idx == "" {
		entries, _ := os.ReadDir(m.dir("redis"))
		n := len(entries) + 1 // database 0 stays unused
		if n > 15 {
			return nil, errors.New("redis: all 15 databases are taken")
		}
		idx = strconv.Itoa(n)
		if err := writeKV(dbFile, map[string]string{"DB": idx}); err != nil {
			return nil, err
		}
	}
	if err := m.Dk.ConnectNetwork(ctx, appNetwork, m.redisName()); err != nil {
		return nil, err
	}
	return map[string]string{"REDIS_URL": fmt.Sprintf("redis://default:%s@%s:6379/%s", kv["PASSWORD"], m.redisName(), idx)}, nil
}
