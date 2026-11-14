package deploy

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"appdeploy/internal/config"
	"appdeploy/internal/notify"
	"appdeploy/internal/store"
)

// parsePercent reads "2%" (or "0.02") as a fraction; bad input gives the 2% default.
func parsePercent(s string) float64 {
	s = strings.TrimSpace(s)
	if p, ok := strings.CutSuffix(s, "%"); ok {
		if v, err := strconv.ParseFloat(strings.TrimSpace(p), 64); err == nil && v >= 0 {
			return v / 100
		}
		return 0.02
	}
	if v, err := strconv.ParseFloat(s, 64); err == nil && v >= 0 && v <= 1 {
		return v
	}
	return 0.02
}

// effectiveLimit is the 5xx rate above which a release is considered bad: the
// configured rate, or twice what the previous release was doing, whichever is higher.
func effectiveLimit(configured, baseline float64) float64 {
	if 2*baseline > configured {
		return 2 * baseline
	}
	return configured
}

// errorRateExceeds reports whether errs of total responses is above limit,
// given enough requests to mean something.
func errorRateExceeds(total, errs, minRequests int, limit float64) bool {
	return total >= minRequests && total > 0 && float64(errs)/float64(total) > limit
}

// hostsOf lists the hostnames that route to the app.
func (e *Engine) hostsOf(a store.App, cfg *config.App) []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range append(append([]string{}, a.Domains...), cfg.Domains...) {
		if h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// findPrev returns the newest release other than this one that still has a
// running web container (the release traffic can go back to).
func (r *run) findPrev() int {
	e := r.e
	prev := 0
	all, _ := e.st.AppContainers(r.d.App)
	for _, c := range all {
		if c.Release == r.d.Release || c.Role != "web" || c.Release <= prev {
			continue
		}
		if st, err := e.dk.Inspect(e.ctx, c.DockerID); err == nil && st.Running {
			prev = c.Release
		}
	}
	return prev
}

// observeAndFinish runs after traffic moved to the new release. It keeps the old
// release running while it watches the new one, so a rollback is only a proxy
// switch. Without a previous release (or for manual rollbacks) there is nothing
// to compare against or return to, so the deploy completes at once.
func (r *run) observeAndFinish() error {
	e := r.e
	if r.d.Source == "rollback" || r.prev == 0 {
		r.finish()
		return nil
	}
	r.state(store.StateObserving)
	r.failPoint("observe:start")
	reason, err := r.observe()
	if err != nil {
		return err
	}
	if reason == "" {
		r.finish()
		return nil
	}
	r.autoRollback(reason)
	_ = e
	return nil
}

// observe returns a non-empty reason when the new release must be rolled back.
func (r *run) observe() (string, error) {
	e := r.e
	ctx := e.ctx
	w := r.cfg.Watch
	hosts := e.hostsOf(r.app, r.cfg)
	t0 := time.Now()

	var baseRate float64
	var baseTotal int
	if e.opt.Traffic != nil && len(hosts) > 0 {
		total, errs := e.opt.Traffic.Window(hosts, t0.Add(-w.Window.D()), t0)
		baseTotal = total
		if total > 0 {
			baseRate = float64(errs) / float64(total)
		}
	}
	limit := effectiveLimit(parsePercent(w.ErrorRate), baseRate)
	r.logf("watching v%d for %s: roll back above %.1f%% 5xx (at least %d requests), %d restarts, or 3 failed health checks; v%d stays up meanwhile (baseline %.1f%% over %d requests)",
		r.d.Release, w.Window.D(), limit*100, w.MinRequests, w.MaxRestarts, r.prev, baseRate*100, baseTotal)

	deadline := t0.Add(w.Window.D())
	lastReport := t0
	failStreak := 0
	var lastProbe time.Time
	for {
		now := time.Now()
		if now.After(deadline) {
			return "", nil
		}
		if !sleep(ctx, 250*time.Millisecond) {
			return "", ctx.Err()
		}
		now = time.Now()

		cs, err := e.st.ReleaseContainers(r.d.App, r.d.Release)
		if err != nil {
			return "", err
		}
		for _, c := range cs {
			st, err := e.dk.Inspect(ctx, c.DockerID)
			if err != nil {
				return fmt.Sprintf("%s disappeared: %v", c.Name, err), nil
			}
			if st.RestartCount >= w.MaxRestarts {
				return fmt.Sprintf("%s restarted %d times", c.Name, st.RestartCount), nil
			}
			if !st.Running && !st.Restarting {
				return fmt.Sprintf("%s exited (code %d)%s", c.Name, st.ExitCode, oomNote(st)), nil
			}
		}

		if hc := r.cfg.Roles["web"]; hc != nil && now.Sub(lastProbe) >= time.Second {
			lastProbe = now
			bad := false
			for _, c := range cs {
				if c.Role == "web" && c.HostPort > 0 {
					code := probe(ctx, fmt.Sprintf("http://%s:%d%s", e.opt.ProbeHost, c.HostPort, hc.Healthcheck.Path), hc.Healthcheck.Timeout.D())
					if code < 200 || code >= 400 {
						bad = true
					}
				}
			}
			if bad {
				failStreak++
			} else {
				failStreak = 0
			}
			if failStreak >= 3 {
				return fmt.Sprintf("health check GET %s failed 3 times in a row", hc.Healthcheck.Path), nil
			}
		}

		if e.opt.Traffic != nil && len(hosts) > 0 {
			total, errs := e.opt.Traffic.Window(hosts, t0, now.Add(time.Second))
			if errorRateExceeds(total, errs, w.MinRequests, limit) {
				return fmt.Sprintf("%d of %d requests (%.1f%%) failed with 5xx, above the %.1f%% limit", errs, total, 100*float64(errs)/float64(total), limit*100), nil
			}
			if now.Sub(lastReport) >= 10*time.Second {
				lastReport = now
				r.logf("observing v%d: %d requests, %d errors so far", r.d.Release, total, errs)
			}
		}
	}
}

// autoRollback puts traffic back on the previous release and discards this one.
func (r *run) autoRollback(reason string) {
	e := r.e
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	r.logf("watchdog: %s", reason)
	if r.prev == 0 {
		r.logf("there is no previous release to go back to; v%d stays live", r.d.Release)
		r.finish()
		return
	}
	if err := e.st.SetCurrentRelease(r.d.App, r.prev); err != nil {
		r.logf("rollback failed: %v", err)
		_ = e.st.SetDeployState(r.d.ID, store.StateFailed, "rollback failed: "+err.Error())
		return
	}
	if err := e.ApplyProxy(ctx); err != nil {
		r.logf("rollback: updating the proxy failed: %v", err)
	}
	r.logf("==> rolled back: %s is serving v%d again", r.d.App, r.prev)
	r.cleanup() // removes this release's containers, row and image; v<prev> is current now
	_ = e.st.SetDeployState(r.d.ID, store.StateRolledBack, reason)
	r.d.State = store.StateRolledBack
	r.emit(notify.DeployRolledBack, reason)
}

// checkMigration looks at how the still-live previous release fared since the
// release command ran. The migration cannot be undone, and finishing the deploy
// is the fastest way to a working schema/code pair, so this only warns loudly
// (event and webhook); the watchdog then watches the new release as usual.
func (r *run) checkMigration() {
	e := r.e
	if r.migAt.IsZero() || e.opt.Traffic == nil || r.prev == 0 {
		return
	}
	hosts := e.hostsOf(r.app, r.cfg)
	total, errs := e.opt.Traffic.Window(hosts, r.migAt, time.Now().Add(time.Second))
	limit := effectiveLimit(parsePercent(r.cfg.Watch.ErrorRate), r.migBase)
	min := r.cfg.Watch.MinRequests
	if min > 10 {
		min = 10
	}
	if !errorRateExceeds(total, errs, min, limit) {
		return
	}
	msg := fmt.Sprintf("v%d, still serving traffic, answered %d of %d requests with 5xx since the release command ran (it was at %.1f%% before); the new schema may be incompatible with the old code. The migration is already applied; continuing so that v%d takes over",
		r.prev, errs, total, r.migBase*100, r.d.Release)
	r.logf("WARNING migration.suspect: %s", msg)
	r.emit(notify.MigrationSuspect, msg)
}
