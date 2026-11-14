// Package notify delivers deploy events to webhooks.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Event names.
const (
	DeployStarted    = "deploy.started"
	DeploySucceeded  = "deploy.succeeded"
	DeployFailed     = "deploy.failed"
	DeployRolledBack = "deploy.rolled_back"
	MigrationSuspect = "migration.suspect"
	BackupFailed     = "backup.failed"
	AppCrashed       = "app.crashed"
	UpdateFailed     = "agent.update_failed"
	DiskLow          = "disk.low"
)

// Event is the JSON body sent to generic webhooks.
type Event struct {
	Event    string    `json:"event"`
	App      string    `json:"app"`
	Release  int       `json:"release"`
	DeployID string    `json:"deploy_id"`
	State    string    `json:"state,omitempty"`
	Commit   string    `json:"commit,omitempty"`
	Error    string    `json:"error,omitempty"`
	Time     time.Time `json:"time"`
}

// Text is a one-line human description, used by chat adapters.
func (e Event) Text() string {
	c := ""
	if len(e.Commit) >= 8 {
		c = " (" + e.Commit[:8] + ")"
	}
	base := fmt.Sprintf("[appdeploy] %s v%d%s: ", e.App, e.Release, c)
	switch e.Event {
	case DeployStarted:
		return base + "deploy started"
	case DeploySucceeded:
		return base + "deploy succeeded"
	case DeployFailed:
		return base + "deploy FAILED: " + e.Error
	case DeployRolledBack:
		return base + "ROLLED BACK automatically: " + e.Error
	case UpdateFailed:
		return "[appdeploy] an agent update was reverted: " + e.Error
	case AppCrashed:
		return fmt.Sprintf("[appdeploy] %s: %s", e.App, e.Error)
	case DiskLow:
		return "[appdeploy] disk space is low: " + e.Error
	case BackupFailed:
		return fmt.Sprintf("[appdeploy] backup of %s FAILED: %s", e.App, e.Error)
	case MigrationSuspect:
		return base + "the running release started failing after the migration: " + e.Error
	}
	return base + e.Event
}

// Notifier posts events. Slack incoming webhooks and the Telegram Bot API get
// {"text": ...}; every other URL gets the Event as JSON.
type Notifier struct {
	Client  *http.Client
	Retries int           // attempts after the first; default 2
	Backoff time.Duration // default 1s, doubled each attempt
}

func (n *Notifier) client() *http.Client {
	if n.Client != nil {
		return n.Client
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func body(rawURL string, ev Event) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host := strings.ToLower(u.Hostname())
	if host == "hooks.slack.com" || host == "api.telegram.org" {
		return json.Marshal(map[string]string{"text": ev.Text()})
	}
	return json.Marshal(ev)
}

// Send delivers the event, retrying transient failures.
func (n *Notifier) Send(ctx context.Context, rawURL string, ev Event) error {
	b, err := body(rawURL, ev)
	if err != nil {
		return err
	}
	retries, backoff := n.Retries, n.Backoff
	if retries == 0 {
		retries = 2
	}
	if backoff == 0 {
		backoff = time.Second
	}
	var last error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(backoff << (attempt - 1)):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(b))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "appdeploy")
		resp, err := n.client().Do(req)
		if err != nil {
			last = err
			continue
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		if resp.StatusCode/100 == 2 {
			return nil
		}
		last = fmt.Errorf("%s", resp.Status)
		if resp.StatusCode/100 == 4 && resp.StatusCode != 429 {
			break // the receiver rejected it; retrying will not help
		}
	}
	return fmt.Errorf("delivering %s to %s: %w", ev.Event, redact(rawURL), last)
}

// redact hides everything but the host: webhook URLs carry secrets.
func redact(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host + "/…"
	}
	return "webhook"
}
