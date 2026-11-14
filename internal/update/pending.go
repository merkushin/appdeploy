package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MaxStarts is how many starts of a freshly installed version may fail to
// confirm themselves before the previous binary is put back.
const MaxStarts = 3

const (
	pendingFile = "update-pending.json"
	failedFile  = "update-failed.json"
)

// Pending marks an installed update that has not yet proved itself. The agent
// confirms it once it has been up and healthy for a while; if the service keeps
// restarting instead, `appdeploy recover` (run before every start) reverts it.
type Pending struct {
	From   string    `json:"from"`
	To     string    `json:"to"`
	Starts int       `json:"starts"`
	Time   time.Time `json:"time"`
}

// Failed records an update that was reverted, so that the same version is not
// tried again until someone asks for a different one.
type Failed struct {
	From   string    `json:"from"`
	To     string    `json:"to"`
	Reason string    `json:"reason"`
	Time   time.Time `json:"time"`
	// Notified is set once the failure has been reported, so a restart does not report it again.
	Notified bool `json:"notified,omitempty"`
}

func writeJSONFile(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSONFile(path string, v any) bool {
	b, err := os.ReadFile(path)
	return err == nil && json.Unmarshal(b, v) == nil
}

// WritePending leaves the marker before a new binary is put in place.
func WritePending(dir string, p Pending) error {
	p.Time = time.Now()
	return writeJSONFile(filepath.Join(dir, pendingFile), p)
}

func ReadPending(dir string) (Pending, bool) {
	var p Pending
	return p, readJSONFile(filepath.Join(dir, pendingFile), &p)
}

func ClearPending(dir string) { _ = os.Remove(filepath.Join(dir, pendingFile)) }

func ReadFailed(dir string) (Failed, bool) {
	var f Failed
	return f, readJSONFile(filepath.Join(dir, failedFile), &f)
}

func ClearFailed(dir string) { _ = os.Remove(filepath.Join(dir, failedFile)) }

// MarkFailedNotified remembers that the failure was reported.
func MarkFailedNotified(dir string) {
	if f, ok := ReadFailed(dir); ok {
		f.Notified = true
		_ = writeJSONFile(filepath.Join(dir, failedFile), f)
	}
}

func norm(v string) string { return strings.TrimPrefix(strings.TrimSpace(v), "v") }

// Recover runs before each start of the agent. It counts the start of an
// unconfirmed update and, once the new version has had MaxStarts starts without
// confirming itself, puts the previous binary back. It reports whether it did.
func Recover(dir, exe string) (reverted bool, msg string, err error) {
	p, ok := ReadPending(dir)
	if !ok {
		return false, "", nil
	}
	p.Starts++
	if p.Starts <= MaxStarts {
		return false, fmt.Sprintf("start %d of %d for %s, which is not confirmed yet", p.Starts, MaxStarts, p.To), writeJSONFile(filepath.Join(dir, pendingFile), p)
	}
	prev := exe + ".previous"
	if _, err := os.Stat(prev); err != nil {
		ClearPending(dir)
		return false, "", fmt.Errorf("%s did not stay up, but there is no %s to go back to", p.To, prev)
	}
	// Keep the failed binary for a post-mortem; the old one takes its place.
	_ = os.Remove(exe + ".failed")
	if err := os.Rename(exe, exe+".failed"); err != nil {
		return false, "", err
	}
	if err := os.Rename(prev, exe); err != nil {
		_ = os.Rename(exe+".failed", exe)
		return false, "", err
	}
	reason := fmt.Sprintf("%s did not stay up after %d starts; went back to %s", p.To, MaxStarts, p.From)
	_ = writeJSONFile(filepath.Join(dir, failedFile), Failed{From: p.From, To: p.To, Reason: reason, Time: time.Now()})
	ClearPending(dir)
	return true, reason, nil
}

// Confirm is called by a running agent once it has been healthy for a while. It
// clears the marker of the update that brought this version. A marker for a
// newer version belongs to an update that was just installed by this very agent
// and must stay; a marker for any other version is stale and is cleared.
func Confirm(dir, running string) (confirmed bool) {
	p, ok := ReadPending(dir)
	if !ok {
		return false
	}
	if norm(p.To) == norm(running) {
		ClearPending(dir)
		ClearFailed(dir)
		return true
	}
	pv, pok := ParseVersion(p.To)
	rv, rok := ParseVersion(running)
	if pok && rok && Newer(pv, rv) {
		return false // the next version is about to start; its own confirmation will clear this
	}
	ClearPending(dir)
	return false
}

var errNoPending = errors.New("no update is pending")

var _ = errNoPending
