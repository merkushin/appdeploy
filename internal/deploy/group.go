package deploy

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"appdeploy/internal/config"
	"appdeploy/internal/group"
	"appdeploy/internal/notify"
	"appdeploy/internal/store"
)

// RefPeer marks deploys that a coordinator asked this server to run; they use
// their own queue so they cannot deadlock behind the coordinator's deploy.
const RefPeer = "@peer"

// PeerPayload is what a coordinator sends to each server of a rollout.
type PeerPayload struct {
	Kind        string            `json:"kind"` // deploy | rollback | undo
	App         string            `json:"app"`
	Domains     []string          `json:"domains,omitempty"`
	Branch      string            `json:"branch,omitempty"`
	Release     int               `json:"release"`
	Commit      string            `json:"commit,omitempty"`
	Config      json.RawMessage   `json:"config,omitempty"`
	Env         map[string]string `json:"env,omitempty"` // the release's environment snapshot
	WatchWindow config.Duration   `json:"watch_window,omitempty"`
}

// Remote reaches the other servers of the group. The api package implements it
// over mutual TLS.
type Remote interface {
	// SendImage streams a gzip-compressed `docker save` of the release image.
	SendImage(ctx context.Context, srv group.Server, app string, release int, body io.Reader) error
	// StartDeploy asks the server to run the payload and returns its local deploy id.
	StartDeploy(ctx context.Context, srv group.Server, p PeerPayload) (string, error)
	// StreamDeploy follows that deploy's journal and returns its final state.
	StreamDeploy(ctx context.Context, srv group.Server, id string, line func(string)) (state, errMsg string, err error)
}

func (e *Engine) groupMode() bool {
	return e.opt.Group != nil && len(e.opt.Group.Peers()) > 0 && e.opt.Remote != nil
}

// queueKey separates deploys a coordinator asked for from the server's own.
func queueKey(d store.Deploy) string {
	if d.Source == "peer" || d.Source == "undo" || d.Ref == RefPeer {
		return "peer:" + d.App
	}
	return d.App
}

// planWaves splits n servers into rollout waves: with up to 3 servers one at a
// time, otherwise a canary first and then 25% of the servers at a time.
func planWaves(n int) [][]int {
	if n == 0 {
		return nil
	}
	chunk := 1
	if n > 3 {
		chunk = (n + 3) / 4
	}
	var waves [][]int
	i := 0
	for i < n {
		size := chunk
		if i == 0 {
			size = 1 // the canary
		}
		var w []int
		for j := 0; j < size && i < n; j++ {
			w = append(w, i)
			i++
		}
		waves = append(waves, w)
	}
	return waves
}

// ---- receiving side: every server runs what it is asked to ----

// SubmitPeer queues a deploy, rollback or undo that a coordinator requested.
// It is idempotent: asking again for a release the server already serves
// returns a deploy that has already succeeded.
func (e *Engine) SubmitPeer(p PeerPayload) (store.Deploy, error) {
	if p.Kind == "" {
		p.Kind = "deploy"
	}
	app, err := e.st.GetApp(p.App)
	if errors.Is(err, store.ErrNotFound) {
		if p.Kind != "deploy" {
			return e.doneDeploy(p, "app is not known here: nothing to do")
		}
		if err = e.st.CreateApp(store.App{Name: p.App, Domains: p.Domains, Branch: p.Branch}); err != nil && !errors.Is(err, store.ErrExists) {
			return store.Deploy{}, err
		}
		app, err = e.st.GetApp(p.App)
	}
	if err != nil {
		return store.Deploy{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	d := store.Deploy{ID: newID(), App: p.App, Release: p.Release, State: store.StateQueued, Ref: RefPeer}
	switch p.Kind {
	case "deploy":
		if app.CurrentRelease == p.Release {
			if _, err := e.st.GetRelease(p.App, p.Release); err == nil {
				return e.doneDeployLocked(p, fmt.Sprintf("already serving v%d", p.Release))
			}
		}
		if len(p.Domains) > 0 {
			_ = e.st.SetDomains(p.App, p.Domains)
		}
		d.Source, d.Ref = "peer", p.Commit
		if d.Ref == "" {
			d.Ref = RefPeer
		}
	case "rollback":
		if app.CurrentRelease == p.Release {
			return e.doneDeployLocked(p, fmt.Sprintf("already serving v%d", p.Release))
		}
		if _, err := e.st.GetRelease(p.App, p.Release); err != nil {
			return store.Deploy{}, fmt.Errorf("release v%d is not on this server", p.Release)
		}
		d.Source = "rollback"
	case "undo":
		// Undo release p.Release: go back to the previous release, or tear down if there is none.
		if app.CurrentRelease != p.Release {
			return e.doneDeployLocked(p, fmt.Sprintf("v%d is not live here: nothing to undo", p.Release))
		}
		prev := 0
		rels, _ := e.st.ListReleases(p.App)
		for _, rel := range rels {
			if rel.Number < p.Release && e.dk.ImageExists(e.ctx, rel.Image) {
				prev = rel.Number
				break
			}
		}
		if prev > 0 {
			d.Source, d.Release = "rollback", prev
		} else {
			d.Source = "undo"
		}
	default:
		return store.Deploy{}, fmt.Errorf("unknown deploy kind %q", p.Kind)
	}
	if err := e.st.CreateDeploy(d); err != nil {
		return d, err
	}
	b, _ := json.Marshal(p)
	if err := e.st.PutPayload(d.ID, b); err != nil {
		return d, err
	}
	if d.Source == "peer" {
		// The ref column holds the commit for display; the queue key must
		// still be the peer one, which Source "peer" already implies.
	}
	e.enqueueLocked(d, "queued ("+p.Kind+" requested by the coordinator)")
	return d, nil
}

func (e *Engine) doneDeploy(p PeerPayload, msg string) (store.Deploy, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.doneDeployLocked(p, msg)
}

// doneDeployLocked records a deploy that has nothing to do and is already succeeded.
func (e *Engine) doneDeployLocked(p PeerPayload, msg string) (store.Deploy, error) {
	d := store.Deploy{ID: newID(), App: p.App, Release: p.Release, State: store.StateQueued, Source: "peer", Ref: RefPeer}
	if err := e.st.CreateDeploy(d); err != nil {
		return d, err
	}
	_ = e.st.AddEvent(d.ID, msg)
	_ = e.st.SetDeployState(d.ID, store.StateSucceeded, "")
	d.State = store.StateSucceeded
	return d, nil
}

// loadPeer prepares a "peer" deploy from the stored payload.
func (r *run) loadPeer() error {
	e := r.e
	raw, err := e.st.GetPayload(r.d.ID)
	if err != nil {
		return fmt.Errorf("payload of the deploy is missing: %w", err)
	}
	var p PeerPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	cfg, err := config.FromJSON(p.Config)
	if err != nil {
		return err
	}
	if p.WatchWindow > 0 {
		cfg.Watch.Window = p.WatchWindow
	}
	r.cfg, r.sha = cfg, p.Commit
	r.image = e.imageTag(r.d.App, r.d.Release)
	if !e.dk.ImageExists(e.ctx, r.image) {
		return fmt.Errorf("image %s was not delivered to this server", r.image)
	}
	r.builtImage = true
	e.secretsMu.Lock()
	defer e.secretsMu.Unlock()
	return WriteEnvFile(e.snapshotFile(r.d.App, r.d.Release), p.Env)
}

// undo tears down the first release on a server that has nothing to go back to.
func (r *run) undo() error {
	e := r.e
	r.state(store.StateSwitching)
	cs, _ := e.st.ReleaseContainers(r.d.App, r.d.Release)
	if err := e.st.SetCurrentRelease(r.d.App, 0); err != nil {
		return err
	}
	if err := e.ApplyProxy(e.ctx); err != nil {
		return err
	}
	for _, c := range cs {
		_ = e.dk.Remove(e.ctx, c.DockerID)
		_ = e.st.RemoveContainer(c.DockerID)
	}
	_ = e.dk.RemoveImage(e.ctx, e.imageTag(r.d.App, r.d.Release))
	_ = e.st.DeleteRelease(r.d.App, r.d.Release)
	_ = removeFile(e.snapshotFile(r.d.App, r.d.Release))
	_ = e.st.SetDeployState(r.d.ID, store.StateSucceeded, "")
	r.logf("==> undone: v%d removed, %s is not served from this server", r.d.Release, r.d.App)
	return nil
}

// roleNames lists the roles this server runs for the deploy: all of them when
// standalone or coordinating, those whose labels match when running as a member.
func (r *run) roleNames() []string {
	names := sortedRoles(r.cfg)
	g := r.e.opt.Group
	if g == nil || (r.d.Source != "peer" && r.d.Ref != RefPeer) {
		return names
	}
	me := g.Me()
	var out []string
	for _, n := range names {
		if me.HasLabel(r.cfg.Roles[n].On...) {
			out = append(out, n)
		}
	}
	return out
}

// ---- coordinating side ----

// targets returns the servers that run at least one role of the config, sorted
// by name, with this server first if it is one of them.
func (e *Engine) targets(cfg *config.App) []group.Server {
	g := e.opt.Group
	var out []group.Server
	for _, s := range g.State().Servers {
		for _, role := range cfg.Roles {
			if s.HasLabel(role.On...) {
				out = append(out, s)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// rollout distributes the release and starts it server by server.
func (r *run) rollout() error {
	e := r.e
	g := e.opt.Group
	ctx := e.ctx
	targets := e.targets(r.cfg)
	if len(targets) == 0 {
		return errors.New("no server of the group carries the labels the roles ask for (roles.*.on)")
	}
	if r.cfg.Services.Postgres || r.cfg.Services.Redis {
		for _, t := range targets {
			if t.Name != g.Self {
				return errors.New("services (postgres/redis) are only available when every role runs on the coordinating server; use an external DATABASE_URL secret instead")
			}
		}
	}
	env, err := ReadEnvFile(e.snapshotFile(r.d.App, r.d.Release))
	if err != nil {
		return err
	}
	base := PeerPayload{Kind: "deploy", App: r.d.App, Domains: r.app.Domains, Branch: r.app.Branch,
		Release: r.d.Release, Commit: r.sha, Config: r.cfg.JSON(), Env: env}
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = t.Name
	}
	r.logf("rolling out v%d to %d server(s): %s", r.d.Release, len(targets), strings.Join(names, ", "))

	// Ship the image once to every other target, in parallel.
	r.state(store.StateDistributing)
	var wg sync.WaitGroup
	errs := make(chan error, len(targets))
	for _, t := range targets {
		if t.Name == g.Self {
			continue
		}
		t := t
		wg.Add(1)
		go func() {
			defer wg.Done()
			pr, pw := io.Pipe()
			go func() {
				zw := gzip.NewWriter(pw)
				err := e.dk.Save(ctx, r.image, zw)
				if cerr := zw.Close(); err == nil {
					err = cerr
				}
				pw.CloseWithError(err)
			}()
			start := time.Now()
			if err := e.opt.Remote.SendImage(ctx, t, r.d.App, r.d.Release, pr); err != nil {
				pr.CloseWithError(err)
				errs <- fmt.Errorf("sending the image to %s: %w", t.Name, err)
				return
			}
			r.logf("image delivered to %s in %s", t.Name, time.Since(start).Round(100*time.Millisecond))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return err
		}
	}

	r.state(store.StateRolling)
	window := r.cfg.Watch.Window.D()
	var succeeded []group.Server
	for wi, wave := range planWaves(len(targets)) {
		w := window
		limit := time.Minute // the canary is watched for at most a minute
		if wi > 0 {
			limit = 20 * time.Second
		}
		if w > limit {
			w = limit
		}
		var wavenames []string
		for _, i := range wave {
			wavenames = append(wavenames, targets[i].Name)
		}
		r.logf("wave %d: %s (watched for %s each)", wi+1, strings.Join(wavenames, ", "), w)
		type result struct {
			srv group.Server
			err error
		}
		res := make(chan result, len(wave))
		for _, i := range wave {
			t := targets[i]
			p := base
			p.WatchWindow = config.Duration(w)
			go func() { res <- result{t, r.runOn(t, p)} }()
		}
		var failure error
		for range wave {
			x := <-res
			if x.err == nil {
				succeeded = append(succeeded, x.srv)
			} else if failure == nil {
				failure = x.err
			}
		}
		if failure != nil {
			if e.ctx.Err() != nil {
				return e.ctx.Err()
			}
			r.undoRollout(succeeded)
			return fmt.Errorf("rollout stopped in wave %d: %w", wi+1, failure)
		}
	}

	// Keep this server's own books complete even if it runs no role.
	selfRuns := false
	for _, t := range targets {
		selfRuns = selfRuns || t.Name == g.Self
	}
	if !selfRuns {
		if err := e.st.PutRelease(store.Release{App: r.d.App, Number: r.d.Release, Image: r.image, Commit: r.sha, Config: r.cfg.JSON()}); err != nil {
			return err
		}
		_ = e.st.SetCurrentRelease(r.d.App, r.d.Release)
	}
	bg, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	r.prune(bg)
	r.cleanDisk(bg)
	_ = removeAll(r.buildDir)
	_ = e.st.SetDeployState(r.d.ID, store.StateSucceeded, "")
	r.d.State = store.StateSucceeded
	r.logf("==> succeeded: %s is now serving v%d on %s", r.d.App, r.d.Release, strings.Join(names, ", "))
	r.emit(notify.DeploySucceeded, "")
	return nil
}

// runOn runs a payload on one server (this one included) and waits for it.
func (r *run) runOn(srv group.Server, p PeerPayload) error {
	e := r.e
	tag := "[" + srv.Name + "] "
	var state, msg string
	if srv.Name == e.opt.Group.Self {
		d, err := e.SubmitPeer(p)
		if err != nil {
			return fmt.Errorf("%s: %w", srv.Name, err)
		}
		var after int64
		for {
			cur, err := e.st.GetDeploy(d.ID)
			if err != nil {
				return err
			}
			evs, _ := e.st.EventsAfter(d.ID, after)
			for _, ev := range evs {
				after = ev.ID
				r.logf("%s%s", tag, ev.Msg)
			}
			if len(evs) == 0 && store.IsTerminal(cur.State) {
				state, msg = cur.State, cur.Error
				break
			}
			if !sleep(e.ctx, 200*time.Millisecond) {
				return e.ctx.Err()
			}
		}
	} else {
		id, err := e.opt.Remote.StartDeploy(e.ctx, srv, p)
		if err != nil {
			return fmt.Errorf("%s: %w", srv.Name, err)
		}
		state, msg, err = e.opt.Remote.StreamDeploy(e.ctx, srv, id, func(l string) { r.logf("%s%s", tag, l) })
		if err != nil {
			return fmt.Errorf("%s: %w", srv.Name, err)
		}
	}
	if state != store.StateSucceeded {
		return fmt.Errorf("%s: deploy %s: %s", srv.Name, state, msg)
	}
	return nil
}

// undoRollout takes the release off the servers that already completed it.
func (r *run) undoRollout(done []group.Server) {
	if len(done) == 0 {
		return
	}
	r.logf("undoing v%d on %d server(s) that already switched", r.d.Release, len(done))
	var wg sync.WaitGroup
	for _, s := range done {
		s := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.runOn(s, PeerPayload{Kind: "undo", App: r.d.App, Release: r.d.Release}); err != nil {
				r.logf("WARNING: undoing on %s failed: %v (fix it with: appdeploy rollback %s)", s.Name, err, r.d.App)
			}
		}()
	}
	wg.Wait()
}

// groupRollback re-activates an earlier release on every server that runs the app.
func (r *run) groupRollback() error {
	e := r.e
	r.state(store.StateRolling)
	var failures []string
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, s := range e.targets(r.cfg) {
		s := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.runOn(s, PeerPayload{Kind: "rollback", App: r.d.App, Release: r.d.Release}); err != nil {
				mu.Lock()
				failures = append(failures, err.Error())
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(failures) > 0 {
		sort.Strings(failures)
		return errors.New(strings.Join(failures, "; "))
	}
	_ = e.st.SetCurrentRelease(r.d.App, r.d.Release)
	_ = e.st.SetDeployState(r.d.ID, store.StateSucceeded, "")
	r.d.State = store.StateSucceeded
	r.logf("==> succeeded: %s is serving v%d on every server", r.d.App, r.d.Release)
	return nil
}
