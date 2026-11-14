//go:build e2e

package e2e

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHub answers the part of the GitHub API the updater uses.
type fakeGitHub struct {
	srv    *httptest.Server
	mu     sync.Mutex
	tag    string
	assets map[string][]byte
}

func newFakeGitHub() *fakeGitHub {
	g := &fakeGitHub{assets: map[string][]byte{}}
	mux := http.NewServeMux()
	latest := func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.tag == "" || (strings.Contains(r.URL.Path, "/tags/") && !strings.HasSuffix(r.URL.Path, "/tags/"+g.tag)) {
			http.NotFound(w, r)
			return
		}
		var assets []map[string]string
		for name := range g.assets {
			assets = append(assets, map[string]string{"name": name, "browser_download_url": g.srv.URL + "/dl/" + name})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": g.tag, "assets": assets})
	}
	mux.HandleFunc("/repos/acme/tool/releases/latest", latest)
	mux.HandleFunc("/repos/acme/tool/releases/tags/", latest)
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		b, ok := g.assets[strings.TrimPrefix(r.URL.Path, "/dl/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	})
	g.srv = httptest.NewServer(mux)
	return g
}

// unpublish removes every release.
func (g *fakeGitHub) unpublish() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tag, g.assets = "", map[string][]byte{}
}

func (g *fakeGitHub) URL() string { return g.srv.URL }
func (g *fakeGitHub) Close()      { g.srv.Close() }

// publish makes tag the latest release, with bin as the binary for this machine,
// signed with key.
func (g *fakeGitHub) publish(tag string, bin []byte, key ed25519.PrivateKey) {
	name := "appdeploy-" + goos() + "-" + goarch()
	h := sha256.Sum256(bin)
	sums := []byte(hex.EncodeToString(h[:]) + "  " + name + "\n")
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tag = tag
	g.assets = map[string][]byte{
		name:             bin,
		"SHA256SUMS":     sums,
		"SHA256SUMS.sig": []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(key, sums))),
	}
}

// The agent installs a newer signed release, restarts into it and keeps the apps
// running; unsigned or foreign releases are refused and change nothing.
func TestSelfUpdate(t *testing.T) {
	e := env
	e.newApp(t, "upd", "upd.test")
	r := e.newRepo(t, "hello")
	if out, err := r.push("upd"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	e.expectBody(t, "upd.test", "/", "hello v1")

	version := func() string { out, _ := sh(e.bin, "version"); return out }
	if got := version(); got != "appdeploy 0.0.1" {
		t.Fatalf("the test binary reports %q", got)
	}

	// Nothing published (earlier tests may have published releases).
	e.gh.unpublish()
	if out, err := e.ad("self-update", "--check"); err == nil || !strings.Contains(out, "published a release") {
		t.Errorf("no release: %v %s", err, out)
	}

	newBin := filepath.Join(t.TempDir(), "appdeploy-9.9.9")
	if out, err := e.buildBinary(newBin, "9.9.9"); err != nil {
		t.Fatalf("building the new version: %v\n%s", err, out)
	}
	bin, _ := os.ReadFile(newBin)

	e.gh.publish("v9.9.9", bin, e.updKey)
	out := e.mustAd(t, "self-update", "--check")
	contains(t, out, "an update is available")
	if got := version(); got != "appdeploy 0.0.1" {
		t.Fatalf("--check changed the binary: %q", got)
	}

	// A release signed by somebody else is refused; the agent keeps running.
	_, stranger, _ := ed25519.GenerateKey(rand.Reader)
	e.gh.publish("v9.9.9", bin, stranger)
	if out, err := e.ad("self-update"); err == nil || !strings.Contains(out, "signature of SHA256SUMS is not valid") {
		t.Fatalf("a foreign signature: %v %s", err, out)
	}
	if got := version(); got != "appdeploy 0.0.1" {
		t.Fatalf("a refused update changed the binary: %q", got)
	}
	e.mustAd(t, "ping")

	// The real thing.
	e.gh.publish("v9.9.9", bin, e.updKey)
	out = e.mustAd(t, "self-update")
	contains(t, out, "signature and checksum are valid")
	contains(t, out, "updated 0.0.1 -> v9.9.9")
	select {
	case <-e.agentDone:
		e.agent = nil
	case <-time.After(20 * time.Second):
		t.Fatalf("the agent did not restart itself")
	}
	if got := version(); got != "appdeploy 9.9.9" {
		t.Errorf("the binary reports %q", got)
	}
	if prev, _ := sh(e.bin+".previous", "version"); prev != "appdeploy 0.0.1" {
		t.Errorf("the old binary was not kept: %q", prev)
	}
	// The app was served throughout: the agent is not in the traffic path.
	e.expectBody(t, "upd.test", "/", "hello v1")

	// What a service manager does: start the agent again, now from the new binary.
	if err := e.startAgent(); err != nil {
		t.Fatal(err)
	}
	out = e.mustAd(t, "self-update")
	contains(t, out, "already up to date")
	e.expectBody(t, "upd.test", "/", "hello v1")
	if s := e.status(t, "upd"); s.App.CurrentRelease != 1 {
		t.Errorf("state after the update: %+v", s.App)
	}
}

// A release that installs fine but cannot start is undone by the service manager's
// pre-start step: after a few failed starts the old binary is back and the agent runs.
func TestFailedUpdateIsReverted(t *testing.T) {
	e := env
	version := func() string { out, _ := sh(e.bin, "version"); return out }
	before := version() // the previous test left a newer binary behind

	badBin := filepath.Join(t.TempDir(), "appdeploy-bad")
	if out, err := e.buildBinary(badBin, "9.9.10", "-X main.testFailStart=1"); err != nil {
		t.Fatalf("building the broken release: %v\n%s", err, out)
	}
	bin, _ := os.ReadFile(badBin)
	e.gh.publish("v9.9.10", bin, e.updKey)

	// The update itself succeeds: the binary runs `version`, which is all the updater can check.
	out := e.mustAd(t, "self-update")
	contains(t, out, "updated")
	select {
	case <-e.agentDone:
		e.agent = nil
	case <-time.After(20 * time.Second):
		t.Fatalf("the agent did not exit after installing")
	}
	if got := version(); got != "appdeploy 9.9.10" {
		t.Fatalf("the broken release is not in place: %q", got)
	}

	// Be the service manager: recover, then start, until the agent stays up.
	var log []string
	for attempt := 1; attempt <= 8; attempt++ {
		rec, _ := sh(e.bin, "recover", "--data-dir", e.dataDir)
		log = append(log, rec)
		if err := e.startAgent(); err == nil {
			break
		}
		if attempt == 8 {
			ents, _ := os.ReadDir(e.dataDir)
			var names []string
			for _, en := range ents {
				names = append(names, en.Name())
			}
			t.Fatalf("the agent never came back:\n%s\ndata dir: %v\nagent log tail:\n%s", strings.Join(log, "\n"), names, tail(e.agentLog(), 1500))
		}
	}
	if got := version(); got != before {
		t.Errorf("the binary is %q, want the previous %q", got, before)
	}
	if got, _ := sh(e.bin+".failed", "version"); got != "appdeploy 9.9.10" {
		t.Errorf("the failed binary was not kept for inspection: %q", got)
	}
	contains(t, strings.Join(log, "\n"), "REVERTED")
	if _, err := os.Stat(filepath.Join(e.dataDir, "update-failed.json")); err != nil {
		t.Errorf("no note about the failed update: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.dataDir, "update-pending.json")); err == nil {
		t.Errorf("the marker was left behind")
	}
	e.mustAd(t, "ping")
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
