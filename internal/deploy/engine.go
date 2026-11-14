// Package deploy runs deploys as journaled state machines.
//
// A deploy is driven by the agent, not by the client that asked for it: the
// client may disconnect at any point. Each state transition is written to the
// store before the step runs, so a restarted agent can resume or roll back.
package deploy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"sync"
	"time"

	"appdeploy/internal/config"
	"appdeploy/internal/docker"
	"appdeploy/internal/group"
	"appdeploy/internal/notify"
	"appdeploy/internal/proxy"
	"appdeploy/internal/services"
	"appdeploy/internal/store"
	"appdeploy/internal/traffic"
)

// Options configures an Engine. Zero values get the defaults from the spec.
type Options struct {
	DataDir  string // /var/lib/appdeploy
	Instance string // name prefix for containers, images and networks; default "ad"

	UpstreamHost string // host the proxy uses to reach containers; default 127.0.0.1
	ProbeHost    string // host the agent uses for health probes; default 127.0.0.1

	ReserveFrac  float64 // fraction of RAM kept free of container limits; default 0.15
	ReserveMin   int64   // minimum reserve in bytes; default 512 MiB
	MinFreeFrac  float64 // refuse to build below this free disk fraction; default 0.10
	MinFreeBytes int64   // refuse to build below this much free disk; default 5 GiB

	WorkerSettle  time.Duration // how long a non-web role must stay up to pass verifying; default 3s
	ProbeInterval time.Duration // pause between health probes; default 1s
	BuildTimeout  time.Duration // default 30m
	MaxAttempts   int           // resume attempts per deploy after agent restarts; default 3

	HookBin    string // path of the appdeploy binary used by git hooks
	SocketPath string // agent socket path embedded in git hooks
	RepoOwner  string // unix user that owns app repositories (the SSH git user); empty = leave as is

	Group  *group.Group // nil = a standalone server
	Remote Remote       // how to reach the other servers of the group

	Services *services.Manager // shared Postgres/Redis; nil = apps may not ask for services

	PruneBuildCache time.Duration // after each deploy, drop build cache unused for this long; 0 = never

	DefaultWatchWindow time.Duration // watch.window when appdeploy.yml sets none; default 3m

	NotifyURLs []string         // webhooks for every app, besides each app's notify: list
	Notifier   *notify.Notifier // default: 2 retries

	// Traffic supplies request counts per host for the watchdog. Without it the
	// watchdog still watches container restarts and health checks.
	Traffic *traffic.Counter

	// FailPoint, if set, is called with the name of a step boundary inside the
	// traffic switch. Tests use it to kill the agent at exact moments.
	FailPoint func(name string)
}

func (o *Options) defaults() {
	if o.Instance == "" {
		o.Instance = "ad"
	}
	if o.UpstreamHost == "" {
		o.UpstreamHost = "127.0.0.1"
	}
	if o.ProbeHost == "" {
		o.ProbeHost = "127.0.0.1"
	}
	if o.ReserveFrac == 0 {
		o.ReserveFrac = 0.15
	}
	if o.ReserveMin == 0 {
		o.ReserveMin = 512 << 20
	}
	if o.MinFreeFrac == 0 {
		o.MinFreeFrac = 0.10
	}
	if o.MinFreeBytes == 0 {
		o.MinFreeBytes = 5 << 30
	}
	if o.WorkerSettle == 0 {
		o.WorkerSettle = 3 * time.Second
	}
	if o.ProbeInterval == 0 {
		o.ProbeInterval = time.Second
	}
	if o.BuildTimeout == 0 {
		o.BuildTimeout = 30 * time.Minute
	}
	if o.MaxAttempts == 0 {
		o.MaxAttempts = 3
	}
}

// Engine owns deploy execution for all apps on this server.
type Engine struct {
	st  *store.Store
	dk  *docker.Client
	px  proxy.Proxy
	opt Options

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex // guards queues and release-number allocation
	queues map[string]*queue

	catchUp catchUpState

	buildSem  chan struct{} // at most one build at a time per server
	proxyMu   sync.Mutex
	secretsMu sync.Mutex
}

type queue struct {
	running bool
	pending *store.Deploy
}

// New creates an engine. Call Recover once before submitting deploys.
func New(st *store.Store, dk *docker.Client, px proxy.Proxy, opt Options) (*Engine, error) {
	opt.defaults()
	if opt.DataDir == "" {
		return nil, errors.New("DataDir is required")
	}
	if opt.Notifier == nil {
		opt.Notifier = &notify.Notifier{}
	}
	for _, d := range []string{"apps", "secrets", "tmp"} {
		if err := os.MkdirAll(opt.DataDir+"/"+d, 0o700); err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Engine{
		st: st, dk: dk, px: px, opt: opt,
		ctx: ctx, cancel: cancel,
		queues:   map[string]*queue{},
		buildSem: make(chan struct{}, 1),
	}, nil
}

// Close stops accepting work and waits for running steps to notice. Deploys
// that were interrupted stay in the journal and resume on the next start.
func (e *Engine) Close() {
	e.cancel()
	e.wg.Wait()
}

// emit delivers an event to the global and the app's webhooks without blocking the deploy.
func (r *run) emit(event, errMsg string) {
	urls := append([]string{}, r.e.opt.NotifyURLs...)
	if r.cfg != nil {
		urls = append(urls, r.cfg.Notify...)
	}
	if len(urls) == 0 {
		return
	}
	ev := notify.Event{Event: event, App: r.d.App, Release: r.d.Release, DeployID: r.d.ID,
		State: r.d.State, Commit: r.sha, Error: errMsg, Time: time.Now()}
	r.e.wg.Add(1)
	go func() {
		defer r.e.wg.Done()
		ctx, cancel := context.WithTimeout(r.e.ctx, 45*time.Second)
		defer cancel()
		for _, u := range urls {
			if err := r.e.opt.Notifier.Send(ctx, u, ev); err != nil && r.e.ctx.Err() == nil {
				log.Printf("notify: %v", err)
			}
		}
	}()
}

func newID() string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%011x%s", time.Now().UnixMilli(), hex.EncodeToString(b[:]))
}

// Names used for Docker objects.
func (e *Engine) imageTag(app string, release int) string {
	return fmt.Sprintf("%s/%s:v%d", e.opt.Instance, app, release)
}
func (e *Engine) networkName(app string) string { return e.opt.Instance + "-" + app }
func (e *Engine) containerName(app string, release int, role string, i int) string {
	return fmt.Sprintf("%s-%s-v%d-%s-%d", e.opt.Instance, app, release, role, i)
}
func (e *Engine) labels(app string, release int, role string) map[string]string {
	return map[string]string{
		"appdeploy.instance": e.opt.Instance,
		"appdeploy.app":      app,
		"appdeploy.release":  fmt.Sprint(release),
		"appdeploy.role":     role,
	}
}

// ImageTag is the name of a release's image on this server.
func (e *Engine) ImageTag(app string, release int) string { return e.imageTag(app, release) }

// InstanceLabel is the label every Docker object of this engine carries.
func (e *Engine) InstanceLabel() string { return "appdeploy.instance=" + e.opt.Instance }

// ---- apps ----

// CreateApp registers an app and creates its git repository.
func (e *Engine) CreateApp(name string, domains []string, branch string) (store.App, error) {
	if !config.ValidAppName(name) {
		return store.App{}, fmt.Errorf("invalid app name %q: use lowercase letters, digits and dashes, starting with a letter", name)
	}
	if branch == "" {
		branch = "main"
	}
	a := store.App{Name: name, Domains: domains, Branch: branch}
	if err := e.st.CreateApp(a); err != nil {
		if errors.Is(err, store.ErrExists) {
			return a, fmt.Errorf("app %q already exists", name)
		}
		return a, err
	}
	if err := e.initRepo(name, branch); err != nil {
		_ = e.st.DeleteApp(name)
		_ = os.RemoveAll(e.appDir(name))
		return a, err
	}
	return e.st.GetApp(name)
}

// RemoveApp stops everything the app runs and deletes its data.
func (e *Engine) RemoveApp(ctx context.Context, name string) error {
	if _, err := e.st.GetApp(name); err != nil {
		return err
	}
	e.mu.Lock()
	q := e.queues[name]
	busy := q != nil && (q.running || q.pending != nil)
	e.mu.Unlock()
	if busy {
		return fmt.Errorf("app %q has a deploy in progress", name)
	}
	cs, err := e.st.AppContainers(name)
	if err != nil {
		return err
	}
	for _, c := range cs {
		if err := e.dk.Remove(ctx, c.DockerID); err != nil {
			return err
		}
	}
	rels, _ := e.st.ListReleases(name)
	for _, r := range rels {
		_ = e.dk.RemoveImage(ctx, r.Image)
	}
	if err := e.st.DeleteApp(name); err != nil {
		return err
	}
	_ = e.dk.RemoveNetwork(ctx, e.networkName(name))
	_ = os.RemoveAll(e.appDir(name))
	_ = os.Remove(e.secretsFile(name))
	return e.ApplyProxy(ctx)
}

// SetDomains replaces the app's domains and updates the proxy.
func (e *Engine) SetDomains(ctx context.Context, app string, domains []string) error {
	if err := e.st.SetDomains(app, domains); err != nil {
		return err
	}
	return e.ApplyProxy(ctx)
}

// ---- proxy ----

// Routes computes the routing table from the current releases.
func (e *Engine) Routes() ([]proxy.Route, error) {
	apps, err := e.st.ListApps()
	if err != nil {
		return nil, err
	}
	var routes []proxy.Route
	for _, a := range apps {
		if a.CurrentRelease == 0 {
			continue
		}
		rel, err := e.st.GetRelease(a.Name, a.CurrentRelease)
		if err != nil {
			continue
		}
		cfg, err := config.FromJSON(rel.Config)
		if err != nil {
			continue
		}
		seen := map[string]bool{}
		var hosts []string
		for _, h := range append(append([]string{}, a.Domains...), cfg.Domains...) {
			if h != "" && !seen[h] {
				seen[h] = true
				hosts = append(hosts, h)
			}
		}
		cs, err := e.st.ReleaseContainers(a.Name, a.CurrentRelease)
		if err != nil {
			return nil, err
		}
		var ups []string
		for _, c := range cs {
			if c.Role == "web" && c.HostPort > 0 {
				ups = append(ups, fmt.Sprintf("%s:%d", e.opt.UpstreamHost, c.HostPort))
			}
		}
		if len(hosts) > 0 && len(ups) > 0 {
			sort.Strings(ups)
			routes = append(routes, proxy.Route{App: a.Name, Hosts: hosts, Upstreams: ups})
		}
	}
	return routes, nil
}

// ApplyProxy pushes the current routing table to the proxy. It is idempotent.
func (e *Engine) ApplyProxy(ctx context.Context) error {
	e.proxyMu.Lock()
	defer e.proxyMu.Unlock()
	routes, err := e.Routes()
	if err != nil {
		return err
	}
	return e.px.Apply(ctx, routes)
}

// ---- submitting deploys ----

// Request asks for a deploy.
type Request struct {
	App        string
	Ref        string // git ref or sha; empty = head of the app's deploy branch
	Source     string // push | cli | redeploy (current image with current secrets)
	RollbackTo int    // > 0: re-activate that release

	Image      string // Source "image": a prebuilt image to deploy instead of building
	ConfigYAML string // Source "image": the contents of an appdeploy.yml to use
}

var imageRefRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]*$`)

// Submit queues a deploy and returns immediately. Deploys of one app run one
// at a time; a deploy still waiting when a newer one arrives is superseded.
func (e *Engine) Submit(req Request) (store.Deploy, error) {
	app, err := e.st.GetApp(req.App)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.Deploy{}, fmt.Errorf("app %q not found", req.App)
		}
		return store.Deploy{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	d := store.Deploy{ID: newID(), App: app.Name, State: store.StateQueued, Ref: req.Ref, Source: req.Source}
	switch {
	case req.RollbackTo > 0:
		rel, err := e.st.GetRelease(app.Name, req.RollbackTo)
		if err != nil {
			return d, fmt.Errorf("release v%d of %s not found (it may have been pruned)", req.RollbackTo, app.Name)
		}
		d.Release = rel.Number
		d.Source = "rollback"
	case req.Source == "redeploy":
		if app.CurrentRelease == 0 {
			return d, fmt.Errorf("app %q has nothing deployed yet", app.Name)
		}
		d.Release, _ = e.st.NextReleaseNumber(app.Name)
	case req.Image != "":
		if !imageRefRe.MatchString(req.Image) {
			return d, fmt.Errorf("invalid image reference %q", req.Image)
		}
		if req.ConfigYAML != "" {
			if _, err := config.Parse([]byte(req.ConfigYAML)); err != nil {
				return d, err
			}
		}
		d.Source, d.Ref = "image", req.Image
		d.Release, _ = e.st.NextReleaseNumber(app.Name)
	default:
		if d.Source == "" {
			d.Source = "cli"
		}
		d.Release, _ = e.st.NextReleaseNumber(app.Name)
	}
	if err := e.st.CreateDeploy(d); err != nil {
		return d, err
	}
	if d.Source == "image" {
		b, _ := json.Marshal(PeerPayload{ConfigYAML: req.ConfigYAML})
		if err := e.st.PutPayload(d.ID, b); err != nil {
			return d, err
		}
	}
	e.enqueueLocked(d, "queued ("+d.Source+")")
	return d, nil
}

func (e *Engine) enqueueLocked(d store.Deploy, why string) {
	key := queueKey(d)
	q := e.queues[key]
	if q == nil {
		q = &queue{}
		e.queues[key] = q
	}
	if q.pending != nil {
		_ = e.st.SetDeployState(q.pending.ID, store.StateSuperseded, "superseded by "+d.ID)
		_ = e.st.AddEvent(q.pending.ID, "superseded by a newer deploy")
	}
	_ = e.st.AddEvent(d.ID, why)
	q.pending = &d
	if !q.running {
		q.running = true
		e.wg.Add(1)
		go e.worker(key)
	}
}

func (e *Engine) worker(app string) {
	defer e.wg.Done()
	for {
		e.mu.Lock()
		q := e.queues[app]
		d := q.pending
		q.pending = nil
		if d == nil || e.ctx.Err() != nil {
			q.running = false
			e.mu.Unlock()
			return
		}
		e.mu.Unlock()
		e.execute(*d)
	}
}

func (e *Engine) execute(d store.Deploy) {
	r := &run{e: e, d: d}
	defer func() {
		if p := recover(); p != nil {
			r.fail(fmt.Errorf("internal error: %v", p))
		}
	}()
	var err error
	if d.Source == "undo" {
		err = r.undo()
	} else if d.State == store.StateSwitching || d.State == store.StateObserving {
		err = r.recoverSwitching()
	} else {
		err = r.pipeline()
	}
	if err == nil {
		return
	}
	if e.ctx.Err() != nil {
		// The agent is shutting down. Leave the journal at the last step so
		// the deploy resumes after restart.
		return
	}
	r.fail(err)
}

// ---- crash recovery ----

// Recover reconciles the store with Docker and resumes deploys that a
// previous agent run left unfinished. Call it once at startup.
func (e *Engine) Recover(ctx context.Context) error {
	cs, err := e.st.AllContainers()
	if err != nil {
		return err
	}
	for _, c := range cs {
		if _, err := e.dk.Inspect(ctx, c.DockerID); err != nil && docker.IsNoSuchObject(err) {
			_ = e.st.RemoveContainer(c.DockerID)
		}
	}
	unfinished, err := e.st.UnfinishedDeploys()
	if err != nil {
		return err
	}
	e.mu.Lock()
	for _, d := range unfinished {
		n, _ := e.st.BumpAttempts(d.ID)
		if n > e.opt.MaxAttempts {
			_ = e.st.SetDeployState(d.ID, store.StateFailed, fmt.Sprintf("gave up after %d attempts interrupted by agent restarts", n-1))
			_ = e.st.AddEvent(d.ID, "gave up: interrupted by agent restarts too many times")
			// A deploy that reached the switch may have left the proxy and
			// the store disagreeing; reconcile below.
			continue
		}
		d.Attempts = n
		e.enqueueLocked(d, fmt.Sprintf("agent restarted; resuming from state %q (attempt %d)", d.State, n))
	}
	e.mu.Unlock()
	return e.ApplyProxy(ctx)
}

// PostgresApps lists apps whose live release uses the shared Postgres.
func (e *Engine) PostgresApps() []string {
	apps, _ := e.st.ListApps()
	var out []string
	for _, a := range apps {
		if a.CurrentRelease == 0 {
			continue
		}
		rel, err := e.st.GetRelease(a.Name, a.CurrentRelease)
		if err != nil {
			continue
		}
		if cfg, err := config.FromJSON(rel.Config); err == nil && cfg.Services.Postgres {
			out = append(out, a.Name)
		}
	}
	return out
}

// NotifyGlobal sends an event that belongs to no deploy to the agent-wide webhooks.
func (e *Engine) NotifyGlobal(event, app, errMsg string) {
	if len(e.opt.NotifyURLs) == 0 {
		return
	}
	ev := notify.Event{Event: event, App: app, Error: errMsg, Time: time.Now()}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		ctx, cancel := context.WithTimeout(e.ctx, 45*time.Second)
		defer cancel()
		for _, u := range e.opt.NotifyURLs {
			if err := e.opt.Notifier.Send(ctx, u, ev); err != nil && e.ctx.Err() == nil {
				log.Printf("notify: %v", err)
			}
		}
	}()
}
