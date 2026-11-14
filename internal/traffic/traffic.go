// Package traffic turns the proxy's access log into per-host request counts,
// which the deploy watchdog compares before and after a release goes live.
package traffic

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

const bucketSize = time.Second

// Counter keeps request and 5xx counts per host in one-second buckets. Old
// buckets are dropped after Retention.
type Counter struct {
	Retention time.Duration // default 15 minutes

	mu    sync.Mutex
	hosts map[string]map[int64]*count // host -> unix second -> counts
}

type count struct{ total, errors int }

// NewCounter returns an empty counter.
func NewCounter() *Counter {
	return &Counter{Retention: 15 * time.Minute, hosts: map[string]map[int64]*count{}}
}

func normalizeHost(h string) string {
	h = strings.ToLower(h)
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return h
}

// Record adds one response.
func (c *Counter) Record(host string, status int, at time.Time) {
	host = normalizeHost(host)
	sec := at.Unix()
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.hosts[host]
	if m == nil {
		m = map[int64]*count{}
		c.hosts[host] = m
	}
	b := m[sec]
	if b == nil {
		b = &count{}
		m[sec] = b
	}
	b.total++
	if status >= 500 {
		b.errors++
	}
	// Drop old buckets of this host now and then.
	if len(m) > int(c.Retention/bucketSize)+60 {
		cut := at.Add(-c.Retention).Unix()
		for s := range m {
			if s < cut {
				delete(m, s)
			}
		}
	}
}

// Window returns the number of responses and 5xx responses of the hosts in [from, to).
func (c *Counter) Window(hosts []string, from, to time.Time) (total, errors int) {
	lo, hi := from.Unix(), to.Unix()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, h := range hosts {
		for sec, b := range c.hosts[normalizeHost(h)] {
			if sec >= lo && sec < hi {
				total += b.total
				errors += b.errors
			}
		}
	}
	return total, errors
}

// logLine is the part of Caddy's JSON access log entry we use.
type logLine struct {
	TS      float64 `json:"ts"`
	Status  int     `json:"status"`
	Request struct {
		Host string `json:"host"`
	} `json:"request"`
}

// Feed parses one JSON access log line into the counter. Lines that are not
// access log entries are ignored. Entries are counted at their own timestamp.
func (c *Counter) Feed(line []byte) {
	var l logLine
	if json.Unmarshal(line, &l) != nil || l.Status == 0 || l.Request.Host == "" {
		return
	}
	at := time.Now()
	if l.TS > 0 {
		at = time.Unix(int64(l.TS), 0)
	}
	c.Record(l.Request.Host, l.Status, at)
}

// Tail follows the access log at path and feeds new lines to the counter
// until ctx ends. It starts at the end of an existing file (history from before
// the agent started says nothing about the release being watched) and follows
// the file across truncation and rotation.
func (c *Counter) Tail(ctx context.Context, path string) {
	var (
		f      *os.File
		off    int64
		inode  uint64
		reader *bufio.Reader
		first  = true
	)
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	var partial []byte
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		st, err := os.Stat(path)
		if err != nil {
			continue // not created yet
		}
		ino := fileID(st)
		if f == nil || ino != inode || st.Size() < off {
			if f != nil {
				f.Close()
			}
			nf, err := os.Open(path)
			if err != nil {
				f = nil
				continue
			}
			f, inode, off, partial = nf, ino, 0, nil
			if first {
				off, _ = f.Seek(0, io.SeekEnd)
				first = false
			}
			reader = bufio.NewReader(f)
		}
		first = false
		for {
			chunk, err := reader.ReadBytes('\n')
			off += int64(len(chunk))
			if len(chunk) > 0 {
				partial = append(partial, chunk...)
			}
			if err != nil {
				break // no complete line yet; keep the partial one
			}
			c.Feed(partial)
			partial = nil
		}
	}
}
