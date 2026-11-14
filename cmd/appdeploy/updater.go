package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"appdeploy/internal/api"
	"appdeploy/internal/group"
	"appdeploy/internal/store"
	"appdeploy/internal/update"
)

// testFailStart makes a build refuse to start the agent (and only the agent:
// `version` and `recover` still work). It is set by the end-to-end tests with
// -ldflags "-X main.testFailStart=1" to stand in for a release that is broken
// at run time.
var testFailStart = ""

// updater installs releases for the agent.
type updater struct {
	dataDir, repo, api string
}

// install installs a release (the latest, or tag) and reports what happened. On
// success it has left a marker that `appdeploy recover` uses to put the old
// binary back if the new version keeps failing to start.
func (u *updater) install(ctx context.Context, check, force bool, repo, tag string, logf func(string)) (update.Result, error) {
	pub, err := base64.StdEncoding.DecodeString(updatePublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return update.Result{}, errors.New("this binary was built without a release signing key, so it cannot verify updates")
	}
	if repo == "" {
		repo = u.repo
	}
	exe, _ := os.Executable()
	armed := false
	res, err := update.Run(ctx, update.Options{
		API: u.api, Repo: repo, PublicKey: ed25519.PublicKey(pub), Current: version, Tag: tag,
		OS: runtime.GOOS, Arch: runtime.GOARCH, Exe: exe, Force: force, CheckOnly: check,
		Token: os.Getenv("APPDEPLOY_UPDATE_TOKEN"),
		Log:   func(format string, a ...any) { logf(fmt.Sprintf(format, a...)) },
		BeforeSwap: func(from, to string) error {
			armed = true
			return update.WritePending(u.dataDir, update.Pending{From: from, To: to})
		},
	})
	if err != nil && armed {
		update.ClearPending(u.dataDir) // the swap did not happen: nothing to watch
	}
	return res, err
}

// versionTag turns "1.3.0" or "v1.3.0" into the release tag "v1.3.0".
func versionTag(v string) string { return "v" + strings.TrimPrefix(strings.TrimSpace(v), "v") }

// runLoop brings this server to the version the group wants, in its turn.
func (u *updater) runLoop(ctx context.Context, every time.Duration, g *group.Group, st *store.Store, pc *api.PeerClient, restart func()) {
	t := time.NewTicker(every)
	defer t.Stop()
	last := ""
	say := func(msg string) {
		if msg != last {
			last = msg
			log.Printf("update: %s", msg)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		state := g.State()
		if state.DesiredVersion == "" {
			continue
		}
		var names []string
		peers := map[string]update.PeerState{}
		for _, s := range state.Servers {
			names = append(names, s.Name)
			if s.Name == g.Self {
				continue
			}
			pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			v, err := pc.Version(pctx, s)
			cancel()
			peers[s.Name] = update.PeerState{Reachable: err == nil, Version: v.Version, Busy: v.Busy}
		}
		un, _ := st.UnfinishedDeploys()
		failedTo := ""
		if f, ok := update.ReadFailed(u.dataDir); ok {
			failedTo = f.To
		}
		d := update.Decide(state.DesiredVersion, version, g.Self, names, peers, len(un) > 0, failedTo)
		switch {
		case d.Done:
			last = ""
		case !d.Act:
			say(d.Reason)
		default:
			say("installing " + state.DesiredVersion)
			res, err := u.install(ctx, false, false, "", versionTag(state.DesiredVersion), func(l string) { log.Printf("update: %s", l) })
			if err != nil {
				say("failed: " + err.Error())
				continue
			}
			if res.Updated {
				log.Printf("update: %s -> %s installed; restarting", res.From, res.To)
				restart()
				return
			}
		}
	}
}

// confirmAfter clears the update marker once this agent has run healthily for a
// while; until then a failing new version can still be reverted.
func confirmAfter(ctx context.Context, d time.Duration, dataDir string, c *api.Client) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(d):
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := c.Ping(pctx); err != nil {
		return // not healthy: leave the marker so that the failing start counts
	}
	if update.Confirm(dataDir, version) {
		log.Printf("update to %s confirmed: the agent has been healthy for %s", version, d)
	}
}

// runRecover is run by the service manager before every start of the agent
// (ExecStartPre). It never blocks a start: problems are reported and ignored.
func runRecover(args []string) int {
	fs := flag.NewFlagSet("recover", flag.ContinueOnError)
	dataDir := fs.String("data-dir", envOr("APPDEPLOY_DATA_DIR", defaultDataDir), "state directory of the agent")
	if err := fs.Parse(args); err != nil {
		return 0
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "appdeploy recover:", err)
		return 0
	}
	reverted, msg, err := update.Recover(*dataDir, exe)
	switch {
	case err != nil:
		fmt.Fprintln(os.Stderr, "appdeploy recover:", err)
	case reverted:
		fmt.Println("appdeploy recover: REVERTED:", msg)
	case msg != "":
		fmt.Println("appdeploy recover:", msg)
	}
	return 0
}
