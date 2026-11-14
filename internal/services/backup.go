package services

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const backupTimeFormat = "20060102T150405Z"

func (m *Manager) backupDir(app string) string { return filepath.Join(m.DataDir, "backups", app) }

// Backup dumps the app's database to a gzip file and returns its path.
func (m *Manager) Backup(ctx context.Context, app string) (string, error) {
	c := m.pgCreds(app)
	if c.Password == "" {
		return "", fmt.Errorf("app %q has no database (set services.postgres: true and deploy)", app)
	}
	if err := os.MkdirAll(m.backupDir(app), 0o700); err != nil {
		return "", err
	}
	final := filepath.Join(m.backupDir(app), time.Now().UTC().Format(backupTimeFormat)+".sql.gz")
	tmp := final + ".partial"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp)
	zw := gzip.NewWriter(f)
	var errb strings.Builder
	runErr := m.Dk.ExecIO(ctx, m.pgName(), []string{"PGPASSWORD=" + c.Password},
		[]string{"pg_dump", "-h", "127.0.0.1", "-U", c.Role, "--clean", "--if-exists", "--no-owner", c.Database}, nil, zw, &errb)
	if cerr := zw.Close(); runErr == nil {
		runErr = cerr
	}
	if serr := f.Sync(); runErr == nil {
		runErr = serr
	}
	f.Close()
	if runErr != nil {
		return "", fmt.Errorf("pg_dump: %v: %s", runErr, strings.TrimSpace(errb.String()))
	}
	if err := os.Rename(tmp, final); err != nil {
		return "", err
	}
	if _, err := m.PruneBackups(app, time.Now()); err != nil {
		log.Printf("pruning backups of %s: %v", app, err)
	}
	return final, nil
}

// Restore replays a backup file (a name in the app's backup directory, or a path) into the app's database.
func (m *Manager) Restore(ctx context.Context, app, file string) error {
	c := m.pgCreds(app)
	if c.Password == "" {
		return fmt.Errorf("app %q has no database", app)
	}
	path := file
	if !strings.ContainsRune(file, filepath.Separator) {
		path = filepath.Join(m.backupDir(app), file)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	var out, errb strings.Builder
	if err := m.Dk.ExecIO(ctx, m.pgName(), []string{"PGPASSWORD=" + c.Password},
		[]string{"psql", "-h", "127.0.0.1", "-U", c.Role, "-d", c.Database, "-v", "ON_ERROR_STOP=1", "-q"}, zr, &out, &errb); err != nil {
		return fmt.Errorf("psql: %v: %s", err, strings.TrimSpace(errb.String()))
	}
	return nil
}

// ListBackups returns the app's backup file names, newest first.
func (m *Manager) ListBackups(app string) ([]string, error) {
	ents, err := os.ReadDir(m.backupDir(app))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".sql.gz") {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names, nil
}

func parseBackupTime(name string) (time.Time, bool) {
	t, err := time.Parse(backupTimeFormat, strings.TrimSuffix(name, ".sql.gz"))
	return t, err == nil
}

// SelectBackups decides which backups to keep: the newest one of each of the
// last 7 days that have backups, plus the newest one of each of the last 4
// ISO weeks that have backups. Names that are not backups are never selected for deletion.
func SelectBackups(names []string) (keep, drop []string) {
	type item struct {
		name string
		t    time.Time
	}
	var items []item
	for _, n := range names {
		if t, ok := parseBackupTime(n); ok {
			items = append(items, item{n, t})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].t.After(items[j].t) })
	kept := map[string]bool{}
	days, weeks := map[string]bool{}, map[string]bool{}
	for _, it := range items {
		day := it.t.Format("2006-01-02")
		y, w := it.t.ISOWeek()
		week := fmt.Sprintf("%d-W%02d", y, w)
		if !days[day] && len(days) < 7 {
			days[day], kept[it.name] = true, true
		}
		if !weeks[week] && len(weeks) < 4 {
			weeks[week], kept[it.name] = true, true
		}
	}
	for _, it := range items {
		if kept[it.name] {
			keep = append(keep, it.name)
		} else {
			drop = append(drop, it.name)
		}
	}
	return keep, drop
}

// PruneBackups deletes backups outside the retention policy.
func (m *Manager) PruneBackups(app string, _ time.Time) (int, error) {
	names, err := m.ListBackups(app)
	if err != nil {
		return 0, err
	}
	_, drop := SelectBackups(names)
	for _, n := range drop {
		if err := os.Remove(filepath.Join(m.backupDir(app), n)); err != nil {
			return 0, err
		}
	}
	return len(drop), nil
}

// RunScheduled backs up every app returned by apps once per interval until ctx
// ends. A failure is passed to onFail; the other apps are still backed up.
func (m *Manager) RunScheduled(ctx context.Context, interval time.Duration, apps func() []string, onFail func(app string, err error)) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, app := range apps() {
			if ctx.Err() != nil {
				return
			}
			bctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
			if _, err := m.Backup(bctx, app); err != nil {
				log.Printf("backup of %s failed: %v", app, err)
				if onFail != nil {
					onFail(app, err)
				}
			}
			cancel()
		}
	}
}

var _ = io.Discard
