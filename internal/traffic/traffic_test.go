package traffic

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCounterWindow(t *testing.T) {
	c := NewCounter()
	now := time.Unix(1_000_000, 0)
	c.Record("A.test:8080", 200, now.Add(-5*time.Second))
	c.Record("a.test", 503, now.Add(-4*time.Second))
	c.Record("a.test", 200, now.Add(-1*time.Second))
	c.Record("b.test", 500, now.Add(-1*time.Second))
	c.Record("a.test", 500, now.Add(-100*time.Second)) // outside the window

	total, errs := c.Window([]string{"a.test"}, now.Add(-10*time.Second), now)
	if total != 3 || errs != 1 {
		t.Errorf("a.test window = %d/%d, want 3 requests 1 error", total, errs)
	}
	total, errs = c.Window([]string{"a.test", "b.test"}, now.Add(-10*time.Second), now)
	if total != 4 || errs != 2 {
		t.Errorf("both = %d/%d", total, errs)
	}
	if total, _ := c.Window([]string{"nope.test"}, now.Add(-time.Hour), now); total != 0 {
		t.Errorf("unknown host counted %d", total)
	}
}

func TestFeedParsesCaddyLines(t *testing.T) {
	c := NewCounter()
	now := time.Now()
	c.Feed([]byte(fmt.Sprintf(`{"level":"info","ts":%d.5,"logger":"http.log.access.x","msg":"handled request","request":{"host":"a.test","uri":"/"},"status":502}`, now.Unix())))
	c.Feed([]byte(`{"level":"info","msg":"not an access entry"}`))
	c.Feed([]byte(`garbage`))
	total, errs := c.Window([]string{"a.test"}, now.Add(-time.Minute), now.Add(time.Minute))
	if total != 1 || errs != 1 {
		t.Errorf("got %d/%d", total, errs)
	}
}

func TestTailFollowsAppendsAndRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	line := func(status int) string {
		return fmt.Sprintf(`{"ts":%d,"status":%d,"request":{"host":"a.test"}}`+"\n", time.Now().Unix(), status)
	}
	// History before the agent started must be ignored.
	if err := os.WriteFile(path, []byte(line(500)+line(500)), 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewCounter()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Tail(ctx, path)
	time.Sleep(300 * time.Millisecond)

	count := func() (int, int) {
		return c.Window([]string{"a.test"}, time.Now().Add(-time.Minute), time.Now().Add(time.Minute))
	}
	waitCount := func(wantTotal, wantErr int) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if tot, e := count(); tot == wantTotal && e == wantErr {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		tot, e := count()
		t.Fatalf("counts = %d/%d, want %d/%d", tot, e, wantTotal, wantErr)
	}
	if tot, _ := count(); tot != 0 {
		t.Fatalf("history was counted: %d", tot)
	}

	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	// A line written in two pieces is counted once, when complete.
	l := line(200)
	f.WriteString(l[:20])
	time.Sleep(250 * time.Millisecond)
	f.WriteString(l[20:])
	f.WriteString(line(503))
	f.Close()
	waitCount(2, 1)

	// Rotation: the old file is renamed and a new one appears.
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(line(500)), 0o644); err != nil {
		t.Fatal(err)
	}
	waitCount(3, 2)
}
