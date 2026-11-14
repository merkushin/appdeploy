package deploy

import (
	"context"
	"fmt"
	"log"
	"syscall"
	"time"

	"appdeploy/internal/config"
	"appdeploy/internal/docker"
	"appdeploy/internal/notify"
	"appdeploy/internal/store"
)

// alertCooldown is how long a container that keeps crashing stays quiet after an alert.
const alertCooldown = 10 * time.Minute

// alerter decides when a running container or the disk deserves a notification.
// It remembers what it has already reported so that one incident is one alert.
type alerter struct {
	restarts map[string]int       // container -> restart count last seen
	down     map[string]bool      // container -> reported as not running
	last     map[string]time.Time // container -> time of its last alert
	diskLow  bool
}

func newAlerter() *alerter {
	return &alerter{restarts: map[string]int{}, down: map[string]bool{}, last: map[string]time.Time{}}
}

// container returns an alert message if the container crashed since it was last
// looked at, or "". Pass the state as Docker reports it.
func (a *alerter) container(name string, st docker.State, now time.Time) string {
	prev, seen := a.restarts[name]
	a.restarts[name] = st.RestartCount
	msg := ""
	switch {
	case !st.Running && !st.Restarting:
		if !a.down[name] {
			a.down[name] = true
			msg = fmt.Sprintf("%s is not running (exit code %d)%s", name, st.ExitCode, oomNote(st))
		}
	case st.Restarting:
		a.down[name] = false
		msg = fmt.Sprintf("%s is restarting repeatedly (%d restarts)", name, st.RestartCount)
	default:
		a.down[name] = false
		if seen && st.RestartCount > prev {
			msg = fmt.Sprintf("%s crashed and was restarted (%d restarts so far)%s", name, st.RestartCount, oomNote(st))
		}
	}
	if msg == "" {
		return ""
	}
	if t, ok := a.last[name]; ok && now.Sub(t) < alertCooldown {
		return "" // already told someone about this container a moment ago
	}
	a.last[name] = now
	return msg
}

// forget drops what is known about containers that no longer exist.
func (a *alerter) forget(live map[string]bool) {
	for name := range a.restarts {
		if !live[name] {
			delete(a.restarts, name)
			delete(a.down, name)
			delete(a.last, name)
		}
	}
}

// disk returns an alert when free space falls below the thresholds, once, and
// re-arms when it has recovered by a margin.
func (a *alerter) disk(free, size, minBytes int64, minFrac float64) string {
	low := free < minBytes || float64(free) < float64(size)*minFrac
	switch {
	case low && !a.diskLow:
		a.diskLow = true
		return fmt.Sprintf("only %s free of %s on the data disk (alert below %s and %.0f%%)", human(free), human(size), human(minBytes), minFrac*100)
	case !low && a.diskLow && free > minBytes*12/10 && float64(free) > float64(size)*minFrac*1.2:
		a.diskLow = false
	}
	return ""
}

// notifyApp sends an event to the global webhooks and to those of the app's live release.
func (e *Engine) notifyApp(event, app, msg string) {
	urls := append([]string{}, e.opt.NotifyURLs...)
	if a, err := e.st.GetApp(app); err == nil && a.CurrentRelease > 0 {
		if rel, err := e.st.GetRelease(app, a.CurrentRelease); err == nil {
			if cfg, err := config.FromJSON(rel.Config); err == nil {
				urls = append(urls, cfg.Notify...)
			}
		}
	}
	if len(urls) == 0 {
		return
	}
	ev := notify.Event{Event: event, App: app, Error: msg, Time: time.Now()}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		ctx, cancel := context.WithTimeout(e.ctx, 45*time.Second)
		defer cancel()
		for _, u := range urls {
			if err := e.opt.Notifier.Send(ctx, u, ev); err != nil && e.ctx.Err() == nil {
				log.Printf("notify: %v", err)
			}
		}
	}()
}

// RunMonitor looks at the containers of live releases and at the disk every
// interval and sends app.crashed and disk.low. It stays quiet about apps that
// are being deployed: the deploy's own checks and watchdog deal with those.
func (e *Engine) RunMonitor(ctx context.Context, every time.Duration) {
	a := newAlerter()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		e.monitorOnce(ctx, a)
	}
}

func (e *Engine) monitorOnce(ctx context.Context, a *alerter) {
	busy := map[string]bool{}
	if un, err := e.st.UnfinishedDeploys(); err == nil {
		for _, d := range un {
			busy[d.App] = true
		}
	}
	apps, err := e.st.ListApps()
	if err != nil {
		return
	}
	live := map[string]bool{}
	for _, app := range apps {
		if app.CurrentRelease == 0 {
			continue
		}
		cs, _ := e.st.ReleaseContainers(app.Name, app.CurrentRelease)
		for _, c := range cs {
			live[c.Name] = true
			st, err := e.dk.Inspect(ctx, c.DockerID)
			if err != nil {
				continue
			}
			if msg := a.container(c.Name, st, time.Now()); msg != "" && !busy[app.Name] {
				log.Printf("app.crashed: %s", msg)
				e.notifyApp(notify.AppCrashed, app.Name, msg)
			}
		}
	}
	a.forget(live)

	var fs syscall.Statfs_t
	if syscall.Statfs(e.opt.DataDir, &fs) == nil {
		free, size := int64(fs.Bavail)*int64(fs.Bsize), int64(fs.Blocks)*int64(fs.Bsize)
		if msg := a.disk(free, size, e.opt.MinFreeBytes, e.opt.MinFreeFrac); msg != "" {
			log.Printf("disk.low: %s", msg)
			e.notifyApp(notify.DiskLow, "", msg)
		}
	}
}

var _ = store.StateQueued
