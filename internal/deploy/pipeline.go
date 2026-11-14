package deploy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"appdeploy/internal/config"
	"appdeploy/internal/docker"
	"appdeploy/internal/group"
	"appdeploy/internal/notify"
	"appdeploy/internal/store"
)

// run is one execution of a deploy.
type run struct {
	e   *Engine
	d   store.Deploy
	app store.App
	cfg *config.App

	prev       int // release that served traffic before this deploy; 0 if none
	image      string
	sha        string
	buildDir   string
	builtImage bool // this run created the image tag

	builderSrv *group.Server // the build server used for this deploy, if any

	migAt   time.Time // when the release command finished; zero if there was none
	migBase float64   // previous release's 5xx rate before the release command
}

func (r *run) logf(format string, args ...any) {
	_ = r.e.st.AddEvent(r.d.ID, fmt.Sprintf(format, args...))
}

func (r *run) state(s string) {
	if err := r.e.st.SetDeployState(r.d.ID, s, ""); err != nil {
		// A journal write failure is fatal for the step: panic is recovered
		// by execute and turned into a failed deploy.
		panic(fmt.Sprintf("journal write: %v", err))
	}
	r.d.State = s
	r.logf("==> %s", s)
}

// fail cleans up everything this run created (but never the live release)
// and records the failure.
func (r *run) fail(err error) {
	r.logf("deploy failed: %v", err)
	r.dropRemoteBuild()
	r.cleanup()
	_ = r.e.st.SetDeployState(r.d.ID, store.StateFailed, err.Error())
	r.d.State = store.StateFailed
	r.emit(notify.DeployFailed, err.Error())
}

func (r *run) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	e := r.e
	if r.buildDir != "" {
		_ = os.RemoveAll(r.buildDir)
	}
	app, err := e.st.GetApp(r.d.App)
	if err != nil || app.CurrentRelease == r.d.Release {
		return // never touch the live release
	}
	cs, _ := e.st.ReleaseContainers(r.d.App, r.d.Release)
	for _, c := range cs {
		if err := e.dk.Remove(ctx, c.DockerID); err != nil {
			r.logf("cleanup: removing container %s: %v", c.Name, err)
			continue
		}
		_ = e.st.RemoveContainer(c.DockerID)
	}
	if r.d.Source == "rollback" {
		return // the release and its image existed before this deploy
	}
	_ = e.st.DeleteRelease(r.d.App, r.d.Release)
	_ = os.Remove(e.snapshotFile(r.d.App, r.d.Release))
	if r.builtImage {
		if err := e.dk.RemoveImage(ctx, e.imageTag(r.d.App, r.d.Release)); err != nil {
			r.logf("cleanup: removing image: %v", err)
		}
	}
}

func (r *run) pipeline() error {
	e := r.e
	ctx := e.ctx
	d := &r.d

	r.state(store.StateValidating)
	app, err := e.st.GetApp(d.App)
	if err != nil {
		return err
	}
	r.app = app
	r.prev = app.CurrentRelease

	switch d.Source {
	case "build":
		return r.buildForPeer()
	case "rollback":
		if err := r.loadRelease(d.Release); err != nil {
			return err
		}
		if d.Release == app.CurrentRelease {
			return fmt.Errorf("v%d is already the current release", d.Release)
		}
		if !e.dk.ImageExists(ctx, r.image) {
			return fmt.Errorf("image of v%d is gone; it cannot be rolled back to", d.Release)
		}
		r.logf("rolling back to v%d (commit %s)", d.Release, short(r.sha))
		if e.groupMode() && d.Ref != RefPeer {
			return r.groupRollback()
		}
	case "image":
		if err := r.loadImage(); err != nil {
			return err
		}
		if err := r.snapshot(); err != nil {
			return err
		}
	case "peer":
		if app.CurrentRelease == d.Release {
			// A repeated request (the coordinator restarted and asked again):
			// the release is live, so touching its containers would only cause downtime.
			if _, err := e.st.GetRelease(d.App, d.Release); err == nil {
				r.logf("already serving v%d", d.Release)
				_ = e.st.SetDeployState(d.ID, store.StateSucceeded, "")
				r.d.State = store.StateSucceeded
				return nil
			}
		}
		if err := r.loadPeer(); err != nil {
			return err
		}
	case "redeploy":
		if app.CurrentRelease == 0 {
			return errors.New("nothing is deployed yet")
		}
		cur, err := e.st.GetRelease(d.App, app.CurrentRelease)
		if err != nil {
			return err
		}
		cfg, err := config.FromJSON(cur.Config)
		if err != nil {
			return err
		}
		r.cfg, r.sha = cfg, cur.Commit
		r.logf("redeploying v%d's image as v%d with current secrets", cur.Number, d.Release)
		if !e.dk.ImageExists(ctx, cur.Image) {
			return fmt.Errorf("image %s is gone", cur.Image)
		}
		if err := e.dk.Tag(ctx, cur.Image, e.imageTag(d.App, d.Release)); err != nil {
			return err
		}
		r.image, r.builtImage = e.imageTag(d.App, d.Release), true
		if err := r.snapshot(); err != nil {
			return err
		}
	default:
		if err := r.checkout(); err != nil {
			return err
		}
		if err := r.snapshot(); err != nil {
			return err
		}
	}
	for _, w := range r.cfg.Warnings() {
		r.logf("warning: %s", w)
	}
	r.emit(notify.DeployStarted, "")
	if err := r.checkResources(); err != nil {
		return err
	}
	if d.Source == "image" {
		if err := r.migrate(); err != nil {
			return err
		}
	} else if d.Source != "rollback" && d.Source != "redeploy" && d.Source != "peer" {
		if b, ok := e.builder(); ok && e.groupMode() {
			err = r.buildRemote(b)
		} else {
			err = r.build()
		}
		if err != nil {
			return err
		}
		if err := r.migrate(); err != nil {
			return err
		}
	}
	if e.groupMode() && d.Source != "peer" && d.Source != "rollback" {
		return r.rollout()
	}
	if err := r.start(); err != nil {
		return err
	}
	if err := r.verify(); err != nil {
		return err
	}
	r.checkMigration()
	if d.Source != "rollback" {
		// Persist the release before the switch so that recovery after a
		// crash can finish it from the journal alone.
		rel := store.Release{App: d.App, Number: d.Release, Image: r.image, Commit: r.sha, Config: r.cfg.JSON()}
		if err := e.st.PutRelease(rel); err != nil {
			return err
		}
	}
	return r.switchTraffic()
}

// loadRelease fills cfg, image and sha from a stored release.
func (r *run) loadRelease(n int) error {
	rel, err := r.e.st.GetRelease(r.d.App, n)
	if err != nil {
		return fmt.Errorf("release v%d not found", n)
	}
	cfg, err := config.FromJSON(rel.Config)
	if err != nil {
		return err
	}
	r.cfg, r.image, r.sha = cfg, rel.Image, rel.Commit
	return nil
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// checkout exports the commit and reads its config.
func (r *run) checkout() error {
	e := r.e
	repo := e.repoDir(r.d.App)
	ref := r.d.Ref
	if ref == "" {
		ref = "refs/heads/" + r.app.Branch
	}
	sha, err := gitResolve(repo, ref)
	if err != nil {
		return err
	}
	r.sha = sha
	r.buildDir = e.buildDir(r.d.App, r.d.ID)
	_ = os.RemoveAll(r.buildDir)
	if err := gitCheckout(repo, sha, r.buildDir); err != nil {
		return err
	}
	data, _ := os.ReadFile(filepath.Join(r.buildDir, config.FileName))
	cfg, err := config.ParseWith(data, config.Defaults{WatchWindow: e.opt.DefaultWatchWindow})
	if err != nil {
		return err
	}
	if cfg.App != "" && cfg.App != r.d.App {
		r.logf("warning: %s says app %q, deploying to %q", config.FileName, cfg.App, r.d.App)
	}
	r.cfg = cfg
	if len(data) == 0 {
		r.logf("commit %s (no %s: using defaults)", short(sha), config.FileName)
	} else {
		r.logf("commit %s", short(sha))
	}
	return nil
}

// checkResources refuses deploys that would overcommit memory or fill the disk.
func (r *run) checkResources() error {
	e := r.e
	ctx := e.ctx
	total, err := e.dk.MemTotal(ctx)
	if err != nil {
		return err
	}
	reserve := int64(float64(total) * e.opt.ReserveFrac)
	if reserve < e.opt.ReserveMin {
		reserve = e.opt.ReserveMin
	}
	var used int64
	cs, err := e.st.AllContainers()
	if err != nil {
		return err
	}
	for _, c := range cs {
		if c.App == r.d.App && c.Release == r.d.Release {
			continue // leftovers of an earlier attempt of this same release
		}
		used += c.Memory
	}
	var need int64
	for _, name := range r.roleNames() {
		role := r.cfg.Roles[name]
		need += role.Memory.Bytes() * int64(role.ReplicasPerSrv)
	}
	if used+need > total-reserve {
		return fmt.Errorf("not enough memory: %s already reserved by running containers + %s needed > %s available (%s total - %s reserve)",
			human(used), human(need), human(total-reserve), human(total), human(reserve))
	}
	r.logf("memory: %s reserved + %s for this release of %s available", human(used), human(need), human(total-reserve))

	if r.d.Source != "rollback" && r.d.Source != "redeploy" && r.d.Source != "peer" && r.d.Source != "image" {
		var st syscall.Statfs_t
		if err := syscall.Statfs(e.opt.DataDir, &st); err == nil {
			free := int64(st.Bavail) * int64(st.Bsize)
			size := int64(st.Blocks) * int64(st.Bsize)
			if free < e.opt.MinFreeBytes || float64(free) < float64(size)*e.opt.MinFreeFrac {
				return fmt.Errorf("not enough disk space to build: %s free of %s (need at least %s and %.0f%%)",
					human(free), human(size), human(e.opt.MinFreeBytes), e.opt.MinFreeFrac*100)
			}
		}
	}
	return nil
}

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// build runs docker build, one build at a time on this server.
func (r *run) build() error {
	e := r.e
	select {
	case e.buildSem <- struct{}{}:
	default:
		r.logf("waiting for the build slot (another build is running)")
		select {
		case e.buildSem <- struct{}{}:
		case <-e.ctx.Done():
			return e.ctx.Err()
		}
	}
	defer func() { <-e.buildSem }()

	r.state(store.StateBuilding)
	ctxDir, err := insideDir(r.buildDir, r.cfg.Build.Context)
	if err != nil {
		return err
	}
	if _, err := insideDir(ctxDir, r.cfg.Build.Dockerfile); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(ctxDir, r.cfg.Build.Dockerfile)); err != nil {
		return fmt.Errorf("%s not found in the repository (build.dockerfile)", r.cfg.Build.Dockerfile)
	}
	tag := e.imageTag(r.d.App, r.d.Release)
	bctx, cancel := context.WithTimeout(e.ctx, e.opt.BuildTimeout)
	defer cancel()
	lw := docker.NewLineWriter(func(s string) { r.logf("  %s", s) })
	err = e.dk.Build(bctx, docker.BuildSpec{
		Tag: tag, ContextDir: ctxDir, Dockerfile: r.cfg.Build.Dockerfile, Args: r.cfg.Build.Args,
		Labels: map[string]string{
			"appdeploy.instance": e.opt.Instance, "appdeploy.app": r.d.App,
			"appdeploy.release": fmt.Sprint(r.d.Release), "appdeploy.commit": r.sha,
		},
	}, lw)
	lw.Flush()
	if err != nil {
		if e.ctx.Err() != nil {
			return e.ctx.Err()
		}
		return fmt.Errorf("build failed: %w", err)
	}
	r.image, r.builtImage = tag, true
	// The checkout is no longer needed.
	_ = os.RemoveAll(r.buildDir)
	return nil
}

// envFor writes a temporary env file for a container and returns its path.
func (r *run) envFor(role string, port int) (string, error) {
	e := r.e
	secrets, err := ReadEnvFile(e.snapshotFile(r.d.App, r.d.Release))
	if err != nil {
		return "", err
	}
	// Plain environment from appdeploy.yml first; secrets of the same name win.
	env := map[string]string{}
	for k, v := range r.cfg.Env {
		env[k] = v
	}
	for k, v := range secrets {
		env[k] = v
	}
	env["APPDEPLOY_APP"] = r.d.App
	env["APPDEPLOY_RELEASE"] = fmt.Sprint(r.d.Release)
	env["APPDEPLOY_ROLE"] = role
	if g := e.opt.Group; g != nil {
		env["APPDEPLOY_SERVER"] = g.Self
	}
	if port > 0 {
		env["PORT"] = fmt.Sprint(port)
	}
	if err := os.MkdirAll(e.tmpDir(), 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(e.tmpDir(), "env-*")
	if err != nil {
		return "", err
	}
	path := f.Name()
	f.Close()
	if err := WriteEnvFile(path, env); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// migrate runs release_command in a one-off container before any traffic moves.
func (r *run) migrate() error {
	if r.cfg.ReleaseCommand == "" {
		return nil
	}
	e := r.e
	r.state(store.StateMigrating)
	args, err := docker.SplitCommand(r.cfg.ReleaseCommand)
	if err != nil {
		return fmt.Errorf("release_command: %w", err)
	}
	if err := e.dk.EnsureNetwork(e.ctx, e.networkName(r.d.App), map[string]string{"appdeploy.instance": e.opt.Instance}); err != nil {
		return err
	}
	envFile, err := r.envFor("release", 0)
	if err != nil {
		return err
	}
	defer os.Remove(envFile)
	name := fmt.Sprintf("%s-%s-v%d-release", e.opt.Instance, r.d.App, r.d.Release)
	_ = e.dk.Remove(e.ctx, name)
	if e.opt.Traffic != nil && r.prev != 0 {
		now := time.Now()
		total, errs := e.opt.Traffic.Window(e.hostsOf(r.app, r.cfg), now.Add(-time.Minute), now)
		if total > 0 {
			r.migBase = float64(errs) / float64(total)
		}
	}
	r.logf("running release command: %s", r.cfg.ReleaseCommand)
	lw := docker.NewLineWriter(func(s string) { r.logf("  %s", s) })
	err = e.dk.RunOnce(e.ctx, docker.RunSpec{
		Name: name, Image: r.image, Network: e.networkName(r.d.App), EnvFile: envFile,
		Memory: 512 << 20, Cmd: args, Labels: e.labels(r.d.App, r.d.Release, "release"),
	}, lw)
	lw.Flush()
	if err != nil {
		if e.ctx.Err() != nil {
			return e.ctx.Err()
		}
		return fmt.Errorf("release command failed: %w", err)
	}
	r.migAt = time.Now()
	return nil
}

func sortedRoles(cfg *config.App) []string {
	names := make([]string, 0, len(cfg.Roles))
	for n := range cfg.Roles {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if (names[i] == "web") != (names[j] == "web") {
			return names[i] == "web"
		}
		return names[i] < names[j]
	})
	return names
}

// start launches the new release's containers next to the old ones.
func (r *run) start() error {
	e := r.e
	ctx := e.ctx
	r.state(store.StateStarting)
	if err := e.dk.EnsureNetwork(ctx, e.networkName(r.d.App), map[string]string{"appdeploy.instance": e.opt.Instance}); err != nil {
		return err
	}
	if err := r.prepareVolumes(); err != nil {
		return err
	}
	// Leftovers of an earlier attempt of this release.
	if old, _ := e.st.ReleaseContainers(r.d.App, r.d.Release); len(old) > 0 {
		for _, c := range old {
			_ = e.dk.Remove(ctx, c.DockerID)
			_ = e.st.RemoveContainer(c.DockerID)
		}
	}
	for _, roleName := range r.roleNames() {
		role := r.cfg.Roles[roleName]
		port := 0
		if roleName == "web" {
			port = role.Port
			if port == 0 {
				if p, err := e.dk.ImageExposedPort(ctx, r.image); err == nil && p > 0 {
					port = p
				} else {
					port = config.DefaultPort
				}
			}
		}
		var cmd []string
		if role.Cmd != "" {
			var err error
			if cmd, err = docker.SplitCommand(role.Cmd); err != nil {
				return fmt.Errorf("roles.%s.cmd: %w", roleName, err)
			}
		}
		for i := 1; i <= role.ReplicasPerSrv; i++ {
			name := e.containerName(r.d.App, r.d.Release, roleName, i)
			_ = e.dk.Remove(ctx, name)
			envFile, err := r.envFor(roleName, port)
			if err != nil {
				return err
			}
			id, err := e.dk.Run(ctx, docker.RunSpec{
				Name: name, Image: r.image, Network: e.networkName(r.d.App), EnvFile: envFile,
				Memory: role.Memory.Bytes(), CPUs: role.CPUs, Cmd: cmd, PublishPort: port,
				Labels: e.labels(r.d.App, r.d.Release, roleName), StopTimeout: int(role.StopTimeout.D().Seconds()),
				Volumes: r.mounts(role),
			})
			os.Remove(envFile)
			if err != nil {
				return fmt.Errorf("starting %s: %w", name, err)
			}
			c := store.Container{DockerID: id, Name: name, App: r.d.App, Release: r.d.Release, Role: roleName, Memory: role.Memory.Bytes()}
			if err := e.st.AddContainer(c); err != nil {
				return err
			}
			if port > 0 {
				hp, err := e.dk.HostPort(ctx, id, port)
				if err != nil {
					r.dumpLogs(c)
					if st, ierr := e.dk.Inspect(ctx, id); ierr == nil && !st.Running {
						return fmt.Errorf("%s exited right after starting (code %d)%s", name, st.ExitCode, oomNote(st))
					}
					return fmt.Errorf("%s: no published port: %w", name, err)
				}
				c.HostPort = hp
				if err := e.st.AddContainer(c); err != nil {
					return err
				}
			}
			r.logf("started %s (role %s)", name, roleName)
		}
	}
	return nil
}

func (r *run) dumpLogs(c store.Container) {
	var sb strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = r.e.dk.Logs(ctx, c.DockerID, false, 20, &sb)
	if sb.Len() == 0 {
		return
	}
	r.logf("last log lines of %s:", c.Name)
	for _, l := range strings.Split(strings.TrimRight(sb.String(), "\n"), "\n") {
		r.logf("  | %s", l)
	}
}

var probeClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// probe makes one GET request and returns the status code (0 on error).
func probe(ctx context.Context, url string, timeout time.Duration) int {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// verify checks the new containers before any traffic reaches them.
func (r *run) verify() error {
	e := r.e
	ctx := e.ctx
	r.state(store.StateVerifying)
	cs, err := e.st.ReleaseContainers(r.d.App, r.d.Release)
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(cs))
	for _, c := range cs {
		c := c
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c.Role == "web" && c.HostPort > 0 {
				errs <- r.verifyWeb(ctx, c)
			} else {
				errs <- r.verifySettled(ctx, c)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *run) verifyWeb(ctx context.Context, c store.Container) error {
	e := r.e
	hc := r.cfg.Roles["web"].Healthcheck
	base := fmt.Sprintf("http://%s:%d", e.opt.ProbeHost, c.HostPort)
	deadline := time.Now().Add(hc.StartPeriod.D())
	passes := 0
	for passes < 2 {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		st, err := e.dk.Inspect(ctx, c.DockerID)
		if err != nil {
			return err
		}
		if !st.Running {
			r.dumpLogs(c)
			return fmt.Errorf("%s exited (code %d)%s", c.Name, st.ExitCode, oomNote(st))
		}
		if code := probe(ctx, base+hc.Path, hc.Timeout.D()); code >= 200 && code < 400 {
			passes++
		} else {
			passes = 0
		}
		if passes >= 2 {
			break
		}
		if time.Now().After(deadline) {
			r.dumpLogs(c)
			return fmt.Errorf("%s: healthcheck GET %s did not pass within %s", c.Name, hc.Path, hc.StartPeriod.D())
		}
		if !sleep(ctx, e.opt.ProbeInterval) {
			return ctx.Err()
		}
	}
	r.logf("%s: healthcheck GET %s ok", c.Name, hc.Path)
	for _, p := range r.cfg.Roles["web"].Smoke {
		code := probe(ctx, base+p, hc.Timeout.D())
		if code == 0 || code >= 500 {
			r.dumpLogs(c)
			return fmt.Errorf("%s: smoke check GET %s failed (status %d)", c.Name, p, code)
		}
		r.logf("%s: smoke GET %s -> %d", c.Name, p, code)
	}
	return nil
}

func oomNote(st docker.State) string {
	if st.OOMKilled {
		return ", killed for exceeding its memory limit"
	}
	return ""
}

// verifySettled passes a container that has no HTTP check if it stays up.
func (r *run) verifySettled(ctx context.Context, c store.Container) error {
	e := r.e
	if !sleep(ctx, e.opt.WorkerSettle) {
		return ctx.Err()
	}
	st, err := e.dk.Inspect(ctx, c.DockerID)
	if err != nil {
		return err
	}
	if !st.Running || st.Restarting || st.RestartCount > 0 {
		r.dumpLogs(c)
		return fmt.Errorf("%s did not stay up (running=%v, restarts=%d, exit code %d)%s", c.Name, st.Running, st.RestartCount, st.ExitCode, oomNote(st))
	}
	r.logf("%s: up", c.Name)
	return nil
}

// switchTraffic points the proxy at the new release, then retires the old one.
func (r *run) switchTraffic() error {
	e := r.e
	r.state(store.StateSwitching)
	r.failPoint("switch:before")
	if err := e.st.SetCurrentRelease(r.d.App, r.d.Release); err != nil {
		return err
	}
	r.failPoint("switch:after-db")
	if err := e.ApplyProxy(e.ctx); err != nil {
		if e.ctx.Err() != nil {
			return e.ctx.Err()
		}
		_ = e.st.SetCurrentRelease(r.d.App, r.prev)
		bg, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = e.ApplyProxy(bg)
		return fmt.Errorf("switching traffic: %w", err)
	}
	r.failPoint("switch:after-proxy")
	return r.observeAndFinish()
}

func (r *run) failPoint(name string) {
	if r.e.opt.FailPoint != nil {
		r.e.opt.FailPoint(name)
	}
}

// finish runs after traffic moved: it never fails the deploy.
func (r *run) finish() {
	e := r.e
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	r.stopOthers(ctx)
	r.prune(ctx)
	r.cleanDisk(ctx)
	_ = os.RemoveAll(r.buildDir)
	_ = e.st.SetDeployState(r.d.ID, store.StateSucceeded, "")
	r.d.State = store.StateSucceeded
	r.logf("==> succeeded: %s is now serving v%d", r.d.App, r.d.Release)
	r.emit(notify.DeploySucceeded, "")
}

// stopOthers stops every container of the app that is not part of the live release.
func (r *run) stopOthers(ctx context.Context) {
	e := r.e
	cs, err := e.st.AppContainers(r.d.App)
	if err != nil {
		r.logf("warning: listing old containers: %v", err)
		return
	}
	var wg sync.WaitGroup
	for _, c := range cs {
		if c.Release == r.d.Release {
			continue
		}
		c := c
		wg.Add(1)
		go func() {
			defer wg.Done()
			timeout := int(config.DefaultStopTimeout.Seconds())
			if rel, err := e.st.GetRelease(c.App, c.Release); err == nil {
				if cfg, err := config.FromJSON(rel.Config); err == nil {
					if role := cfg.Roles[c.Role]; role != nil && role.StopTimeout > 0 {
						timeout = int(role.StopTimeout.D().Seconds())
					}
				}
			}
			if err := e.dk.Stop(ctx, c.DockerID, timeout); err != nil {
				r.logf("warning: stopping %s: %v", c.Name, err)
			}
			if err := e.dk.Remove(ctx, c.DockerID); err != nil {
				r.logf("warning: removing %s: %v", c.Name, err)
				return
			}
			_ = e.st.RemoveContainer(c.DockerID)
			r.logf("stopped %s", c.Name)
		}()
	}
	wg.Wait()
}

// prune deletes releases beyond keep_releases (never the live one).
func (r *run) prune(ctx context.Context) {
	e := r.e
	rels, err := e.st.ListReleases(r.d.App)
	if err != nil {
		return
	}
	keep := r.cfg.KeepReleases
	for i, rel := range rels {
		if i < keep || rel.Number == r.d.Release {
			continue
		}
		if cs, _ := e.st.ReleaseContainers(r.d.App, rel.Number); len(cs) > 0 {
			continue
		}
		if err := e.dk.RemoveImage(ctx, rel.Image); err != nil {
			r.logf("warning: removing image of v%d: %v", rel.Number, err)
			continue
		}
		_ = os.Remove(e.snapshotFile(r.d.App, rel.Number))
		_ = e.st.DeleteRelease(r.d.App, rel.Number)
		r.logf("pruned release v%d", rel.Number)
	}
}

// recoverSwitching finishes or reverts a deploy the agent died in the middle of.
func (r *run) recoverSwitching() error { // also resumes the observing state
	e := r.e
	ctx := e.ctx
	r.logf("recovering a deploy interrupted during the traffic switch")
	app, err := e.st.GetApp(r.d.App)
	if err != nil {
		return err
	}
	r.app = app
	rel, err := e.st.GetRelease(r.d.App, r.d.Release)
	if err != nil {
		return fmt.Errorf("release v%d missing after restart: %w", r.d.Release, err)
	}
	if r.cfg, err = config.FromJSON(rel.Config); err != nil {
		return err
	}
	r.image, r.sha = rel.Image, rel.Commit
	r.builtImage = r.d.Source != "rollback"

	cs, err := e.st.ReleaseContainers(r.d.App, r.d.Release)
	if err != nil {
		return err
	}
	allUp := len(cs) > 0
	for _, c := range cs {
		st, err := e.dk.Inspect(ctx, c.DockerID)
		if err != nil || !st.Running {
			allUp = false
		}
	}
	if allUp {
		// Everything verified before the crash; complete the switch.
		if err := e.st.SetCurrentRelease(r.d.App, r.d.Release); err != nil {
			return err
		}
		if err := e.ApplyProxy(ctx); err != nil {
			return fmt.Errorf("switching traffic: %w", err)
		}
		r.prev = r.findPrev()
		return r.observeAndFinish()
	}

	// The new containers are gone: put traffic back on whatever else runs.
	prev := r.findPrev()
	if err := e.st.SetCurrentRelease(r.d.App, prev); err != nil {
		return err
	}
	if err := e.ApplyProxy(ctx); err != nil {
		return err
	}
	return fmt.Errorf("interrupted during the switch and the new containers are gone; traffic restored to v%d", prev)
}

// cleanDisk drops leftovers of superseded builds. It runs only when no build is
// in progress (it would otherwise race with the build's own layers).
func (r *run) cleanDisk(ctx context.Context) {
	e := r.e
	select {
	case e.buildSem <- struct{}{}:
		defer func() { <-e.buildSem }()
	default:
		return
	}
	if _, err := e.dk.PruneImages(ctx, "appdeploy.instance="+e.opt.Instance); err != nil {
		r.logf("warning: pruning images: %v", err)
	}
	if e.opt.PruneBuildCache > 0 {
		if _, err := e.dk.PruneBuildCache(ctx, e.opt.PruneBuildCache.String()); err != nil {
			r.logf("warning: pruning build cache: %v", err)
		}
	}
}

// snapshot freezes the release's environment: its secrets plus the URLs of the
// services it asked for, which are provisioned here (idempotently).
func (r *run) snapshot() error {
	e := r.e
	extra := map[string]string{}
	sv := r.cfg.Services
	if sv.Postgres || sv.Redis {
		if e.opt.Services == nil {
			return errors.New("this agent has no service support")
		}
		net := e.networkName(r.d.App)
		if err := e.dk.EnsureNetwork(e.ctx, net, map[string]string{"appdeploy.instance": e.opt.Instance}); err != nil {
			return err
		}
		if sv.Postgres {
			r.logf("providing postgres (database for %s)", r.d.App)
			env, err := e.opt.Services.ProvisionPostgres(e.ctx, r.d.App, net)
			if err != nil {
				return err
			}
			for k, v := range env {
				extra[k] = v
			}
		}
		if sv.Redis {
			r.logf("providing redis")
			env, err := e.opt.Services.ProvisionRedis(e.ctx, r.d.App, net)
			if err != nil {
				return err
			}
			for k, v := range env {
				extra[k] = v
			}
		}
	}
	return e.snapshotSecrets(r.d.App, r.d.Release, extra)
}

// loadImage prepares a deploy of a prebuilt image: its config comes from the
// --config the client sent, else from the live release, else the defaults, and
// the image is used from this server's Docker or pulled.
func (r *run) loadImage() error {
	e := r.e
	p, err := r.payload()
	if err != nil {
		return err
	}
	switch {
	case p.ConfigYAML != "":
		r.cfg, err = config.ParseWith([]byte(p.ConfigYAML), config.Defaults{WatchWindow: e.opt.DefaultWatchWindow})
	case r.app.CurrentRelease > 0:
		var rel store.Release
		if rel, err = e.st.GetRelease(r.d.App, r.app.CurrentRelease); err == nil {
			r.cfg, err = config.FromJSON(rel.Config)
		}
	default:
		r.cfg, err = config.ParseWith(nil, config.Defaults{WatchWindow: e.opt.DefaultWatchWindow})
	}
	if err != nil {
		return err
	}
	ref := r.d.Ref
	if e.dk.ImageExists(e.ctx, ref) {
		r.logf("using image %s from this server", ref)
	} else {
		r.logf("pulling %s", ref)
		lw := docker.NewLineWriter(func(s string) { r.logf("  %s", s) })
		err := e.dk.Pull(e.ctx, ref, lw)
		lw.Flush()
		if err != nil {
			if e.ctx.Err() != nil {
				return e.ctx.Err()
			}
			return fmt.Errorf("pulling %s: %w (private registries need docker login on the server)", ref, err)
		}
	}
	tag := e.imageTag(r.d.App, r.d.Release)
	if err := e.dk.Tag(e.ctx, ref, tag); err != nil {
		return err
	}
	r.image, r.builtImage, r.sha = tag, true, ref
	return nil
}

// volumeName is the Docker name of an app's volume.
func (e *Engine) volumeName(app, name string) string {
	return fmt.Sprintf("%s-%s-%s", e.opt.Instance, app, name)
}

// mounts returns the docker -v arguments of a role.
func (r *run) mounts(role *config.Role) []string {
	var out []string
	for _, v := range role.Volumes {
		name, path, ro, err := config.ParseVolume(v)
		if err != nil {
			continue // validated when the config was read
		}
		m := r.e.volumeName(r.d.App, name) + ":" + path
		if ro {
			m += ":ro"
		}
		out = append(out, m)
	}
	return out
}

// prepareVolumes creates the volumes this release mounts and refuses to start
// containers whose image declares a VOLUME that nothing is mounted over: Docker
// would back it with an anonymous volume that is deleted with the container, so
// the data written there would be lost at the next deploy.
func (r *run) prepareVolumes() error {
	e := r.e
	ctx := e.ctx
	declared, err := e.dk.ImageVolumes(ctx, r.image)
	if err != nil {
		return err
	}
	used := map[string]bool{}
	for _, roleName := range r.roleNames() {
		role := r.cfg.Roles[roleName]
		mounted := map[string]bool{}
		for _, v := range role.Volumes {
			name, path, _, err := config.ParseVolume(v)
			if err != nil {
				return err
			}
			mounted[path] = true
			used[name] = true
		}
		for _, p := range declared {
			if !mounted[p] {
				return fmt.Errorf("the image declares VOLUME %s but role %s mounts nothing there, so Docker would use an anonymous volume that is deleted with the container and the data would be lost at the next deploy. Add roles.%s.volumes: [\"data:%s\"] to appdeploy.yml, or remove VOLUME from the Dockerfile", p, roleName, roleName, p)
			}
		}
	}
	names := make([]string, 0, len(used))
	for n := range used {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := e.dk.EnsureVolume(ctx, e.volumeName(r.d.App, n), map[string]string{
			"appdeploy.instance": e.opt.Instance, "appdeploy.app": r.d.App, "appdeploy.volume": n,
		}); err != nil {
			return fmt.Errorf("creating volume %s: %w", n, err)
		}
	}
	if len(names) > 0 && r.prev != 0 && r.prev != r.d.Release {
		r.logf("WARNING: volume(s) %s are used by v%d and by v%d at the same time until v%d stops (after the %s watch window). Databases such as SQLite on the volume have two processes on them meanwhile; shorten watch.window if that matters",
			strings.Join(names, ", "), r.prev, r.d.Release, r.prev, r.cfg.Watch.Window.D())
	}
	return nil
}
