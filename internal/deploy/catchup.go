package deploy

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"appdeploy/internal/config"
	"appdeploy/internal/group"
	"appdeploy/internal/store"
)

// RecordFinal notes that a rollout (or rollback) of app to release completed
// and tells the other servers, so that one which missed it can catch up.
func (e *Engine) RecordFinal(app string, release int) {
	f := store.Final{App: app, Release: release, TS: time.Now().UnixMilli()}
	if _, err := e.st.PutFinal(f); err != nil {
		log.Printf("recording the final release of %s: %v", app, err)
	}
	if e.opt.Group == nil || e.opt.Remote == nil {
		return
	}
	for _, p := range e.opt.Group.Peers() {
		p := p
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			ctx, cancel := context.WithTimeout(e.ctx, 15*time.Second)
			defer cancel()
			_ = e.opt.Remote.Announce(ctx, p, f) // a server that is down learns it when it catches up
		}()
	}
}

// HandleFinal stores a record announced by another server.
func (e *Engine) HandleFinal(f store.Final) error {
	_, err := e.st.PutFinal(f)
	return err
}

// Finals lists the records this server knows.
func (e *Engine) Finals() ([]store.Final, error) { return e.st.ListFinals() }

// ReleasePayload describes a release this server has, in the form a coordinator
// would have sent it, so that another server can run the same release.
func (e *Engine) ReleasePayload(app string, release int) (PeerPayload, error) {
	a, err := e.st.GetApp(app)
	if err != nil {
		return PeerPayload{}, err
	}
	rel, err := e.st.GetRelease(app, release)
	if err != nil {
		return PeerPayload{}, err
	}
	env, err := ReadEnvFile(e.snapshotFile(app, release))
	if err != nil {
		return PeerPayload{}, err
	}
	return PeerPayload{Kind: "deploy", App: app, Domains: a.Domains, Branch: a.Branch, Release: release,
		Commit: rel.Commit, Config: rel.Config, Env: env}, nil
}

// ImportImage loads a gzip-compressed `docker save` as the image of a release.
func (e *Engine) ImportImage(ctx context.Context, app string, release int, gz io.Reader) error {
	zr, err := gzip.NewReader(gz)
	if err != nil {
		return err
	}
	loaded, err := e.dk.Load(ctx, zr)
	if err != nil {
		return err
	}
	if want := e.imageTag(app, release); loaded != want {
		return e.dk.Tag(ctx, loaded, want)
	}
	return nil
}

// catchUpState remembers the finals this server already settled, so that it
// does not ask its peers about them again every round.
type catchUpState struct {
	mu      sync.Mutex
	settled map[string]int64 // app -> ts of the final it has dealt with
}

// CatchUp brings this server to the release a completed rollout left the group
// on, for apps whose roles run here: a server that was down or unreachable
// during a rollout, or that joined later, converges by itself. It reports how
// many apps it started a deploy for.
func (e *Engine) CatchUp(ctx context.Context) (int, error) {
	if !e.groupMode() {
		return 0, nil
	}
	g := e.opt.Group
	peers := g.Peers()
	reached := 0
	for _, p := range peers {
		fs, err := e.opt.Remote.Finals(ctx, p)
		if err != nil {
			continue
		}
		reached++
		for _, f := range fs {
			if _, err := e.st.PutFinal(f); err != nil {
				return 0, err
			}
		}
	}
	if reached == 0 {
		return 0, errors.New("no other server could be reached")
	}
	finals, err := e.st.ListFinals()
	if err != nil {
		return 0, err
	}
	busy := map[string]bool{}
	if un, err := e.st.UnfinishedDeploys(); err == nil {
		for _, d := range un {
			busy[d.App] = true
		}
	}
	e.catchUp.mu.Lock()
	if e.catchUp.settled == nil {
		e.catchUp.settled = map[string]int64{}
	}
	e.catchUp.mu.Unlock()

	started := 0
	for _, f := range finals {
		e.catchUp.mu.Lock()
		done := e.catchUp.settled[f.App] == f.TS
		e.catchUp.mu.Unlock()
		if done || busy[f.App] {
			continue
		}
		settle := func() {
			e.catchUp.mu.Lock()
			e.catchUp.settled[f.App] = f.TS
			e.catchUp.mu.Unlock()
		}
		if a, err := e.st.GetApp(f.App); err == nil {
			if a.CurrentRelease == f.Release {
				settle()
				continue
			}
			// Something newer happened here since that rollout: not ours to undo.
			if last, _ := e.st.LastSucceeded(f.App); !last.IsZero() && last.UnixMilli() > f.TS {
				settle()
				continue
			}
		}
		pl, from, err := e.releaseFromPeers(ctx, peers, f)
		if err != nil {
			log.Printf("catch-up: %s v%d: %v", f.App, f.Release, err)
			continue
		}
		var cfg *config.App
		if cfg, err = config.FromJSON(pl.Config); err != nil {
			continue
		}
		runsHere := false
		for _, role := range cfg.Roles {
			runsHere = runsHere || g.Me().HasLabel(role.On...)
		}
		if !runsHere {
			settle()
			continue
		}
		if rel, err := e.st.GetRelease(f.App, f.Release); err == nil && e.dk.ImageExists(ctx, rel.Image) {
			pl.Kind = "rollback" // we have run it before: just go back to it
		} else {
			body, err := e.opt.Remote.FetchImage(ctx, from, f.App, f.Release)
			if err != nil {
				log.Printf("catch-up: fetching the image of %s v%d from %s: %v", f.App, f.Release, from.Name, err)
				continue
			}
			err = e.ImportImage(ctx, f.App, f.Release, body)
			body.Close()
			if err != nil {
				log.Printf("catch-up: loading the image of %s v%d: %v", f.App, f.Release, err)
				continue
			}
			pl.WatchWindow = config.Duration(20 * time.Second)
		}
		d, err := e.SubmitPeer(pl)
		if err != nil {
			log.Printf("catch-up: %s v%d: %v", f.App, f.Release, err)
			continue
		}
		_ = e.st.AddEvent(d.ID, fmt.Sprintf("catching up: the group serves v%d (from %s)", f.Release, from.Name))
		log.Printf("catch-up: %s -> v%d (from %s)", f.App, f.Release, from.Name)
		settle()
		started++
	}
	return started, nil
}

// releaseFromPeers asks the servers in turn for the release described by f.
func (e *Engine) releaseFromPeers(ctx context.Context, peers []group.Server, f store.Final) (PeerPayload, group.Server, error) {
	var last error
	for _, p := range peers {
		pl, err := e.opt.Remote.FetchRelease(ctx, p, f.App, f.Release)
		if err == nil {
			return pl, p, nil
		}
		last = err
	}
	if last == nil {
		last = errors.New("no peers")
	}
	return PeerPayload{}, group.Server{}, fmt.Errorf("no server could describe the release: %w", last)
}

// RunCatchUp runs CatchUp shortly after start and then at every interval until ctx ends.
func (e *Engine) RunCatchUp(ctx context.Context, every time.Duration) {
	first := time.NewTimer(2 * time.Second)
	defer first.Stop()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-first.C:
		case <-t.C:
		}
		if _, err := e.CatchUp(ctx); err != nil && ctx.Err() == nil {
			log.Printf("catch-up: %v", err)
		}
	}
}
