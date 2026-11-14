//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func contains(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("output does not contain %q:\n%s", needle, haystack)
	}
}

func notContains(t *testing.T, haystack, needle string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Fatalf("output unexpectedly contains %q:\n%s", needle, haystack)
	}
}

// A repository with only a Dockerfile deploys, and a second push replaces the
// first release without a single failed request in between.
func TestDeployUpdateWithoutDowntime(t *testing.T) {
	e := env
	e.newApp(t, "hello", "hello.test")
	r := e.newRepo(t, "hello")

	out, err := r.push("hello")
	if err != nil {
		t.Fatalf("push failed: %v\n%s", err, out)
	}
	contains(t, out, "no appdeploy.yml: using defaults")
	contains(t, out, "==> succeeded")
	e.expectBody(t, "hello.test", "/", "hello v1")

	// Hammer the app while the second release goes out.
	var total, failed atomic.Int64
	var firstFailure atomic.Value
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			code, body := e.get("hello.test", "/")
			total.Add(1)
			if code != 200 || (body != "hello v1" && body != "hello v2") {
				failed.Add(1)
				firstFailure.CompareAndSwap(nil, fmt.Sprintf("%d %q", code, body))
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	r.write("message.txt", "hello v2\n")
	r.commit("v2")
	out, err = r.push("hello")
	if err != nil {
		t.Fatalf("second push failed: %v\n%s", err, out)
	}
	contains(t, out, "stopped ad")
	e.expectBody(t, "hello.test", "/", "hello v2")
	time.Sleep(500 * time.Millisecond)
	close(stop)
	<-done

	if failed.Load() != 0 {
		t.Errorf("%d of %d requests failed during the deploy; first: %v", failed.Load(), total.Load(), firstFailure.Load())
	}
	t.Logf("%d requests during the deploy, none failed", total.Load())

	// Only the new release's container is left.
	cs := e.containers("hello")
	if len(cs) != 1 || !strings.HasSuffix(cs[0], "-hello-v2-web-1") {
		t.Errorf("containers after update: %v", cs)
	}
	s := e.status(t, "hello")
	if s.App.CurrentRelease != 2 || len(s.Releases) != 2 {
		t.Errorf("status: current=%d releases=%d", s.App.CurrentRelease, len(s.Releases))
	}
}

// A release that never becomes healthy is discarded and traffic never moves.
func TestFailedDeployKeepsServingOldRelease(t *testing.T) {
	e := env
	e.newApp(t, "keep", "keep.test")
	r := e.newRepo(t, "hello")
	if out, err := r.push("keep"); err != nil {
		t.Fatalf("first push: %v\n%s", err, out)
	}
	e.expectBody(t, "keep.test", "/", "hello v1")

	// v2 checks a health URL the app does not serve.
	r.write("message.txt", "hello v2\n")
	r.write("appdeploy.yml", "roles:\n  web:\n    healthcheck:\n      path: /nope\n      start_period: 4s\n")
	r.commit("bad v2")
	out, err := r.push("keep")
	if err != nil {
		t.Fatalf("git itself accepts the push even when the deploy fails: %v\n%s", err, out)
	}
	contains(t, out, "did not pass within 4s")
	contains(t, out, "deploy failed")
	// The failed release left nothing behind.
	e.expectBody(t, "keep.test", "/", "hello v1")
	if cs := e.containers("keep"); len(cs) != 1 || !strings.Contains(cs[0], "-v1-") {
		t.Errorf("containers after failed deploy: %v", cs)
	}
	if e.imageExists("keep", 2) {
		t.Errorf("image of the failed release was not removed")
	}
	s := e.status(t, "keep")
	if s.App.CurrentRelease != 1 || s.Deploys[0].State != "failed" {
		t.Errorf("status: current=%d latest deploy=%s", s.App.CurrentRelease, s.Deploys[0].State)
	}

	// Fixing the config deploys fine; the failed attempt's number is not reused.
	r.write("appdeploy.yml", "")
	r.commit("fix")
	if out, err := r.push("keep"); err != nil {
		t.Fatalf("fix push: %v\n%s", err, out)
	}
	e.expectBody(t, "keep.test", "/", "hello v2")
	if s := e.status(t, "keep"); s.App.CurrentRelease != 3 {
		t.Errorf("release after fix = v%d, want v3", s.App.CurrentRelease)
	}
}

// Apps that crash or never listen are rejected even on their very first deploy.
func TestBrokenApps(t *testing.T) {
	e := env
	for _, tc := range []struct{ mode, want string }{
		{"crash", "boom: crashing as requested"},
		{"unhealthy", "did not pass within 4s"},
		{"silent", "did not pass within 4s"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			app := "broken-" + tc.mode
			e.newApp(t, app, app+".test")
			r := e.newRepo(t, "broken")
			r.write("mode.txt", tc.mode+"\n")
			r.write("appdeploy.yml", "roles:\n  web:\n    healthcheck:\n      start_period: 4s\n")
			r.commit("mode " + tc.mode)
			out, _ := r.push(app)
			contains(t, out, "deploy failed")
			contains(t, out, tc.want)
			if code, _ := e.get(app+".test", "/"); code != 404 {
				t.Errorf("a failed first deploy must not get a route, got status %d", code)
			}
			if cs := e.containers(app); len(cs) != 0 {
				t.Errorf("containers left behind: %v", cs)
			}
			if e.imageExists(app, 1) {
				t.Errorf("image left behind")
			}
		})
	}
}

func TestRollback(t *testing.T) {
	e := env
	e.newApp(t, "rb", "rb.test")
	r := e.newRepo(t, "hello")
	for _, v := range []string{"v1", "v2"} {
		r.write("message.txt", "hello "+v+"\n")
		r.commit(v)
		if out, err := r.push("rb"); err != nil {
			t.Fatalf("push %s: %v\n%s", v, err, out)
		}
	}
	e.expectBody(t, "rb.test", "/", "hello v2")

	out := e.mustAd(t, "rollback", "rb")
	contains(t, out, "rolling back to v1")
	contains(t, out, "==> succeeded")
	e.expectBody(t, "rb.test", "/", "hello v1")
	if s := e.status(t, "rb"); s.App.CurrentRelease != 1 {
		t.Errorf("current release = %d", s.App.CurrentRelease)
	}
	if cs := e.containers("rb"); len(cs) != 1 {
		t.Errorf("containers: %v", cs)
	}
	// Rolling back to the release that is already live is refused.
	if out, err := e.ad("rollback", "rb", "--to", "1"); err == nil || !strings.Contains(out, "already the current") {
		t.Errorf("rollback to current: err=%v out=%s", err, out)
	}
}

func TestSeveralAppsOnOneServer(t *testing.T) {
	e := env
	e.newApp(t, "alpha", "alpha.test")
	e.newApp(t, "beta", "beta.test")
	a := e.newRepo(t, "hello")
	b := e.newRepo(t, "hello")
	a.write("message.txt", "I am alpha\n")
	a.commit("alpha")
	b.write("message.txt", "I am beta\n")
	b.commit("beta")
	for _, p := range []struct {
		r   *repo
		app string
	}{{a, "alpha"}, {b, "beta"}} {
		if out, err := p.r.push(p.app); err != nil {
			t.Fatalf("push %s: %v\n%s", p.app, err, out)
		}
	}
	e.expectBody(t, "alpha.test", "/", "I am alpha")
	e.expectBody(t, "beta.test", "/", "I am beta")
	if code, body := e.get("nobody.test", "/"); code != 404 {
		t.Errorf("unknown host: %d %q", code, body)
	}

	// Redeploying alpha does not disturb beta.
	beta := e.containers("beta")
	a.write("message.txt", "I am alpha 2\n")
	a.commit("alpha 2")
	if out, err := a.push("alpha"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	e.expectBody(t, "alpha.test", "/", "I am alpha 2")
	e.expectBody(t, "beta.test", "/", "I am beta")
	if got := e.containers("beta"); strings.Join(got, ",") != strings.Join(beta, ",") {
		t.Errorf("beta's containers changed: %v -> %v", beta, got)
	}

	// Domains can be changed without a deploy.
	e.mustAd(t, "domains", "set", "alpha", "alpha.test", "alpha2.test")
	e.expectBody(t, "alpha2.test", "/", "I am alpha 2")

	// The app list shows both.
	list := e.mustAd(t, "apps", "list")
	contains(t, list, "alpha")
	contains(t, list, "beta")
}

func TestMemoryLimitsAreEnforced(t *testing.T) {
	e := env
	e.newApp(t, "hog", "hog.test")
	r := e.newRepo(t, "hello")
	r.write("appdeploy.yml", "roles:\n  web:\n    memory: 100000G\n")
	r.commit("greedy")
	out, _ := r.push("hog")
	contains(t, out, "deploy failed: not enough memory")
	// It was refused before any build.
	notContains(t, out, "==> building")
}

func TestSecretsAreAppliedWithoutRebuild(t *testing.T) {
	e := env
	e.newApp(t, "sec", "sec.test")
	r := e.newRepo(t, "hello")
	if out, err := r.push("sec"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	e.expectBody(t, "sec.test", "/env?name=GREETING", "")

	out := e.mustAd(t, "secrets", "set", "sec", "GREETING=hi there")
	contains(t, out, "redeploying v1's image as v2")
	notContains(t, out, "==> building")
	e.expectBody(t, "sec.test", "/env?name=GREETING", "hi there")

	// Names are listed, values only on explicit export.
	list := e.mustAd(t, "secrets", "list", "sec")
	contains(t, list, "GREETING")
	notContains(t, list, "hi there")
	contains(t, e.mustAd(t, "secrets", "export", "sec"), "GREETING=hi there")

	// A rollback brings back the secrets the old release had.
	e.mustAd(t, "rollback", "sec")
	e.expectBody(t, "sec.test", "/env?name=GREETING", "")

	// Bad names and values are refused.
	if out, err := e.ad("secrets", "set", "sec", "bad-name=1"); err == nil {
		t.Errorf("invalid name accepted: %s", out)
	}
	e.mustAd(t, "secrets", "set", "sec", "--no-deploy", "LATER=1")
	if s := e.status(t, "sec"); s.App.CurrentRelease != 1 {
		t.Errorf("--no-deploy changed the release: v%d", s.App.CurrentRelease)
	}
}

func TestWorkerRoleAndReleaseCommand(t *testing.T) {
	e := env
	e.newApp(t, "multi", "multi.test")
	r := e.newRepo(t, "hello")
	r.write("appdeploy.yml", `
roles:
  web: {}
  worker:
    cmd: worker
    memory: 64M
release_command: migrate
`)
	r.commit("web + worker")
	out, err := r.push("multi")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	contains(t, out, "==> migrating")
	contains(t, out, "migrate: ok")
	e.expectBody(t, "multi.test", "/", "hello v1")
	s := e.status(t, "multi")
	roles := map[string]bool{}
	for _, c := range s.Containers {
		if c.Running {
			roles[c.Role] = true
		}
	}
	if !roles["web"] || !roles["worker"] {
		t.Fatalf("expected web and worker running: %+v", s.Containers)
	}
	logs := e.mustAd(t, "logs", "multi", "--role", "worker", "--tail", "5")
	waitFor(t, 10*time.Second, "worker log lines", func() bool {
		l, _ := e.ad("logs", "multi", "--role", "worker", "--tail", "5")
		logs = l
		return strings.Contains(l, "worker tick")
	})
	contains(t, logs, "worker tick")

	// exec runs inside the web container.
	if out, err := e.ad("exec", "multi", "--", "/server", "migrate"); err != nil || !strings.Contains(out, "migrate: ok") {
		t.Errorf("exec: %v %q", err, out)
	}

	// A failing release command stops the deploy before anything starts.
	e.mustAd(t, "secrets", "set", "multi", "--no-deploy", "FAIL_MIGRATE=1")
	r.write("message.txt", "hello v2\n")
	r.commit("v2")
	out, _ = r.push("multi")
	contains(t, out, "release command failed")
	notContains(t, out, "==> starting")
	e.expectBody(t, "multi.test", "/", "hello v1")

	// A new release replaces both roles.
	e.mustAd(t, "secrets", "unset", "multi", "--no-deploy", "FAIL_MIGRATE")
	// The failed push already updated the repository, so deploy its head.
	e.mustAd(t, "deploy", "multi")
	e.expectBody(t, "multi.test", "/", "hello v2")
	var alive []string
	for _, c := range e.status(t, "multi").Containers {
		if c.Running {
			alive = append(alive, c.Name)
		}
	}
	if len(alive) != 2 {
		t.Errorf("running containers after update: %v", alive)
	}
}

// Killing git push does not stop the deploy: the agent runs it, not the client.
func TestInterruptedPushDoesNotAbortDeploy(t *testing.T) {
	e := env
	e.newApp(t, "intr", "intr.test")
	r := e.newRepo(t, "hello")
	r.slowBuild(6)
	r.commit("slow build")
	p := r.pushAsync("intr")
	p.waitOutput(t, "==> building", 60*time.Second)
	_ = p.cmd.Process.Signal(syscall.SIGKILL)

	waitFor(t, 3*time.Minute, "the deploy to finish without its client", func() bool {
		s, err := e.client.Status(context.Background(), "intr")
		return err == nil && len(s.Deploys) > 0 && s.Deploys[0].State == "succeeded"
	})
	e.expectBody(t, "intr.test", "/", "hello v1")
}

// If the agent dies in the middle of a build, the next agent finishes the deploy.
func TestAgentCrashDuringBuildIsResumed(t *testing.T) {
	e := env
	e.newApp(t, "crash", "crash.test")
	r := e.newRepo(t, "hello")
	r.slowBuild(8)
	r.commit("slow build")
	p := r.pushAsync("crash")
	p.waitOutput(t, "==> building", 60*time.Second)

	e.stopAgent(syscall.SIGKILL)
	_ = p.cmd.Process.Kill()
	if code, _ := e.get("crash.test", "/"); code != 404 {
		t.Fatalf("nothing should be served yet, got %d", code)
	}
	if err := e.startAgent(); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 3*time.Minute, "the resumed deploy to succeed", func() bool {
		s, err := e.client.Status(context.Background(), "crash")
		return err == nil && len(s.Deploys) > 0 && s.Deploys[0].State == "succeeded"
	})
	e.expectBody(t, "crash.test", "/", "hello v1")
	if s := e.status(t, "crash"); len(s.Deploys) != 1 {
		t.Errorf("resuming must continue the same deploy, got %d deploys", len(s.Deploys))
	}
	// Its journal says it was resumed.
	var buf strings.Builder
	st, _, err := e.client.StreamEvents(context.Background(), e.status(t, "crash").Deploys[0].ID, &buf)
	if err != nil || st != "succeeded" {
		t.Fatalf("events: %v state=%s", err, st)
	}
	contains(t, buf.String(), "agent restarted; resuming")
}

// With the agent restarted gracefully, a running app keeps being served and a
// graceful shutdown mid-deploy resumes too.
func TestGracefulAgentRestartKeepsServing(t *testing.T) {
	e := env
	e.newApp(t, "calm", "calm.test")
	r := e.newRepo(t, "hello")
	if out, err := r.push("calm"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	e.expectBody(t, "calm.test", "/", "hello v1")

	e.stopAgent(syscall.SIGTERM)
	// The agent is not in the traffic path.
	e.expectBody(t, "calm.test", "/", "hello v1")
	if err := e.startAgent(); err != nil {
		t.Fatal(err)
	}
	e.expectBody(t, "calm.test", "/", "hello v1")
	s := e.status(t, "calm")
	if s.App.CurrentRelease != 1 || len(s.Containers) != 1 || !s.Containers[0].Running {
		t.Errorf("state after restart: %+v", s)
	}
}

// Of several pushes that queue up behind a running deploy, only the latest runs.
func TestQueuedPushesCollapse(t *testing.T) {
	e := env
	e.newApp(t, "queue", "queue.test")
	r := e.newRepo(t, "hello")
	r.slowBuild(6)
	r.write("message.txt", "hello A\n")
	r.commit("A")
	pa := r.pushAsync("queue")
	pa.waitOutput(t, "==> building", 60*time.Second)

	r.write("message.txt", "hello B\n")
	r.commit("B")
	pb := r.pushAsync("queue")
	pb.waitOutput(t, "queued", 20*time.Second)
	r.write("message.txt", "hello C\n")
	r.commit("C")
	pc := r.pushAsync("queue")
	pc.waitOutput(t, "queued", 20*time.Second)

	waitFor(t, 4*time.Minute, "queued deploys to settle", func() bool {
		s, err := e.client.Status(context.Background(), "queue")
		if err != nil || len(s.Deploys) < 3 {
			return false
		}
		for _, d := range s.Deploys {
			if d.State != "succeeded" && d.State != "superseded" && d.State != "failed" {
				return false
			}
		}
		return true
	})
	e.expectBody(t, "queue.test", "/", "hello C")
	counts := map[string]int{}
	for _, d := range e.status(t, "queue").Deploys {
		counts[d.State]++
	}
	if counts["superseded"] != 1 || counts["succeeded"] != 2 {
		t.Errorf("want A and C to run and B superseded, got %v", counts)
	}
}

// The agent dies at each step boundary of the traffic switch. Whatever the
// moment, the restarted agent completes the deploy and no request fails.
func TestAgentCrashDuringSwitchIsResumed(t *testing.T) {
	e := env
	for _, fp := range []string{"switch:before", "switch:after-db", "switch:after-proxy", "observe:start"} {
		fp := fp
		t.Run(strings.ReplaceAll(fp, ":", "-"), func(t *testing.T) {
			app := "sw-" + strings.ReplaceAll(fp, ":", "-")
			host := app + ".test"
			e.newApp(t, app, host)
			r := e.newRepo(t, "hello")
			if out, err := r.push(app); err != nil {
				t.Fatalf("push: %v\n%s", err, out)
			}
			e.expectBody(t, host, "/", "hello v1")

			// Restart the agent so that it kills itself at fp.
			e.stopAgent(syscall.SIGTERM)
			if err := e.startAgentEnv("APPDEPLOY_FAILPOINT=" + fp); err != nil {
				t.Fatal(err)
			}
			r.write("message.txt", "hello v2\n")
			r.commit("v2")
			p := r.pushAsync(app)
			waitFor(t, 2*time.Minute, "the agent to die at "+fp, func() bool {
				select {
				case <-e.agentDone:
					return true
				default:
					return false
				}
			})
			_ = p.cmd.Process.Kill()
			e.agent = nil
			// Traffic keeps flowing while the agent is down.
			if code, body := e.get(host, "/"); code != 200 || !strings.HasPrefix(body, "hello v") {
				t.Fatalf("app not served while the agent is down: %d %q", code, body)
			}

			if err := e.startAgent(); err != nil {
				t.Fatal(err)
			}
			waitFor(t, 2*time.Minute, "the interrupted deploy to succeed", func() bool {
				s, err := e.client.Status(context.Background(), app)
				return err == nil && len(s.Deploys) > 0 && s.Deploys[0].State == "succeeded"
			})
			e.expectBody(t, host, "/", "hello v2")
			waitFor(t, 30*time.Second, "the old release to be retired", func() bool {
				cs := e.containers(app)
				return len(cs) == 1 && strings.Contains(cs[0], "-v2-")
			})
			if s := e.status(t, app); s.App.CurrentRelease != 2 {
				t.Errorf("current release = v%d", s.App.CurrentRelease)
			}
		})
	}
}

// The SSH entry point: git push and CLI commands go through `appdeploy shell`
// (the sshd forced command), which accepts nothing else. A fake ssh that does
// what sshd does with a forced command stands in for the real one.
func TestPushAndCLIOverSSH(t *testing.T) {
	e := env
	dir := t.TempDir()
	fake := filepath.Join(dir, "ssh")
	script := fmt.Sprintf("#!/bin/sh\nfor last; do :; done\nexport SSH_ORIGINAL_COMMAND=\"$last\" APPDEPLOY_SOCKET=%q APPDEPLOY_DATA_DIR=%q\nPATH=\"$(git --exec-path):$PATH\"\nexec %q shell\n",
		e.socket, e.dataDir, e.bin)
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	viaSSH := func(cmd string) (string, error) { return sh(fake, "deploy@testhost", cmd) }

	e.newApp(t, "viassh", "viassh.test")
	r := e.newRepo(t, "hello")
	push := exec.Command("git", "push", "--force", "deploy@testhost:viassh", "HEAD:refs/heads/main")
	push.Dir = r.dir
	push.Env = append(os.Environ(), "GIT_SSH_COMMAND="+fake)
	out, err := push.CombinedOutput()
	if err != nil {
		t.Fatalf("push over ssh: %v\n%s", err, out)
	}
	contains(t, string(out), "==> succeeded")
	e.expectBody(t, "viassh.test", "/", "hello v1")

	// The client CLI reaches the server with --host.
	cli := exec.Command(e.bin, "--host", "deploy@testhost", "status", "viassh")
	cli.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
	b, err := cli.CombinedOutput()
	if err != nil || !strings.Contains(string(b), "release v1") {
		t.Errorf("status over ssh: %v\n%s", err, b)
	}

	// Everything else is refused.
	for cmd, want := range map[string]string{
		"--host evil.example status viassh": "unexpected option",
		"--socket /tmp/x status":            "unexpected option",
		"git-receive-pack '../../etc'":      "invalid app name",
		"git-receive-pack 'nosuchapp'":      "no such app",
		"agent --data-dir /tmp/x":           "unknown command",
		"sh -c id":                          "unknown command",
		"hook post-receive --app viassh":    "unknown command",
	} {
		out, err := viaSSH(cmd)
		if err == nil || !strings.Contains(out, want) {
			t.Errorf("%q: want failure containing %q, got err=%v out=%q", cmd, want, err, out)
		}
	}
}

// hammer sends requests to the app until stop is closed, like users would.
func (e *testEnv) hammer(host string, stop <-chan struct{}) (wait func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			e.get(host, "/")
			time.Sleep(30 * time.Millisecond)
		}
	}()
	return func() { <-done }
}

// A release that passes every deploy-time check but fails real traffic is
// rolled back automatically, and the old release was kept warm for it.
func TestWatchdogRollsBackOnErrors(t *testing.T) {
	e := env
	e.newApp(t, "wd-err", "wd-err.test")
	r := e.newRepo(t, "broken")
	r.write("appdeploy.yml", "watch:\n  window: 20s\n  min_requests: 5\n")
	r.commit("v1")
	if out, err := r.push("wd-err"); err != nil {
		t.Fatalf("v1: %v\n%s", err, out)
	}
	e.expectBody(t, "wd-err.test", "/", "broken app, mode ok")

	r.write("mode.txt", "errors\n")
	r.commit("v2 serves errors")
	p := r.pushAsync("wd-err")
	p.waitOutput(t, "watching v2", 90*time.Second)
	if cs := e.containers("wd-err"); len(cs) != 2 {
		t.Errorf("the old release should stay up while v2 is watched, containers: %v", cs)
	}
	stop := make(chan struct{})
	wait := e.hammer("wd-err.test", stop)
	p.waitOutput(t, "rolled back", 60*time.Second)
	close(stop)
	wait()

	out := p.output()
	contains(t, out, "watchdog:")
	contains(t, out, "5xx")
	contains(t, out, "wd-err is serving v1 again")
	waitFor(t, 30*time.Second, "the failed deploy to be recorded", func() bool {
		s := e.status(t, "wd-err")
		return s.Deploys[0].State == "rolled_back"
	})
	e.expectBody(t, "wd-err.test", "/", "broken app, mode ok")
	s := e.status(t, "wd-err")
	if s.App.CurrentRelease != 1 {
		t.Errorf("current release = v%d, want v1", s.App.CurrentRelease)
	}
	if cs := e.containers("wd-err"); len(cs) != 1 || !strings.Contains(cs[0], "-v1-") {
		t.Errorf("containers after rollback: %v", cs)
	}
	if e.imageExists("wd-err", 2) {
		t.Errorf("the rolled back release's image was not removed")
	}
}

// A release that keeps crashing after it went live is rolled back too.
func TestWatchdogRollsBackCrashLoop(t *testing.T) {
	e := env
	e.newApp(t, "wd-crash", "wd-crash.test")
	r := e.newRepo(t, "broken")
	r.write("appdeploy.yml", "watch:\n  window: 25s\n")
	r.commit("v1")
	if out, err := r.push("wd-crash"); err != nil {
		t.Fatalf("v1: %v\n%s", err, out)
	}
	r.write("mode.txt", "dies-later\n")
	r.commit("v2 dies later")
	out, _ := r.push("wd-crash")
	contains(t, out, "==> verifying") // it did pass the deploy-time checks
	// Depending on timing the health check or the restart counter notices first.
	contains(t, out, "watchdog:")
	contains(t, out, "wd-crash is serving v1 again")
	e.expectBody(t, "wd-crash.test", "/", "broken app, mode ok")
	if s := e.status(t, "wd-crash"); s.Deploys[0].State != "rolled_back" || s.App.CurrentRelease != 1 {
		t.Errorf("state=%s current=v%d", s.Deploys[0].State, s.App.CurrentRelease)
	}
}

// A healthy release is kept under watch for the whole window, then the old one is retired.
func TestWatchdogAcceptsHealthyRelease(t *testing.T) {
	e := env
	e.newApp(t, "wd-ok", "wd-ok.test")
	r := e.newRepo(t, "hello")
	r.write("appdeploy.yml", "watch:\n  window: 8s\n  min_requests: 5\n")
	r.commit("v1")
	if out, err := r.push("wd-ok"); err != nil {
		t.Fatalf("v1: %v\n%s", err, out)
	}
	r.write("message.txt", "hello v2\n")
	r.commit("v2")
	p := r.pushAsync("wd-ok")
	p.waitOutput(t, "watching v2", 90*time.Second)
	stop := make(chan struct{})
	wait := e.hammer("wd-ok.test", stop)
	p.waitOutput(t, "==> succeeded", 60*time.Second)
	close(stop)
	wait()
	contains(t, p.output(), "==> observing")
	e.expectBody(t, "wd-ok.test", "/", "hello v2")
	waitFor(t, 20*time.Second, "the old release to be retired", func() bool {
		cs := e.containers("wd-ok")
		return len(cs) == 1 && strings.Contains(cs[0], "-v2-")
	})
}

// Deploy events reach the webhooks listed in appdeploy.yml.
func TestWebhookNotifications(t *testing.T) {
	e := env
	var mu sync.Mutex
	var events []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			mu.Lock()
			events = append(events, m)
			mu.Unlock()
		}
	}))
	defer srv.Close()
	names := func() string {
		mu.Lock()
		defer mu.Unlock()
		var out []string
		for _, m := range events {
			out = append(out, fmt.Sprintf("%v:v%v", m["event"], m["release"]))
		}
		return strings.Join(out, " ")
	}

	e.newApp(t, "hooks", "hooks.test")
	r := e.newRepo(t, "hello")
	r.write("appdeploy.yml", "notify: ["+srv.URL+"/hook]\n")
	r.commit("v1")
	if out, err := r.push("hooks"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	waitFor(t, 10*time.Second, "deploy.succeeded", func() bool { return strings.Contains(names(), "deploy.succeeded:v1") })
	contains(t, names(), "deploy.started:v1")

	// A failing deploy is reported with its reason.
	r.write("appdeploy.yml", "notify: ["+srv.URL+"/hook]\nroles:\n  web:\n    healthcheck:\n      path: /nope\n      start_period: 3s\n")
	r.commit("bad")
	r.push("hooks")
	waitFor(t, 10*time.Second, "deploy.failed", func() bool { return strings.Contains(names(), "deploy.failed:v2") })
	mu.Lock()
	last := events[len(events)-1]
	mu.Unlock()
	if last["app"] != "hooks" || !strings.Contains(fmt.Sprint(last["error"]), "did not pass") || last["deploy_id"] == "" {
		t.Errorf("failure event: %v", last)
	}
}

// Postgres and Redis come with the app: its own database, URLs in its
// environment, and backups that bring the data back.
func TestServicesAndBackups(t *testing.T) {
	e := env
	e.newApp(t, "svc", "svc.test")
	r := e.newRepo(t, "hello")
	r.write("appdeploy.yml", "services:\n  postgres: true\n  redis: true\n")
	r.commit("with services")
	out, err := r.push("svc")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	contains(t, out, "providing postgres")
	contains(t, out, "==> succeeded")

	_, dbURL := e.get("svc.test", "/env?name=DATABASE_URL")
	_, redisURL := e.get("svc.test", "/env?name=REDIS_URL")
	if !strings.HasPrefix(dbURL, "postgres://app_svc:") || !strings.HasPrefix(redisURL, "redis://default:") {
		t.Fatalf("urls: %q %q", dbURL, redisURL)
	}
	net := e.instance + "-svc"
	psql := func(args ...string) string {
		t.Helper()
		out, err := sh("docker", append([]string{"run", "--rm", "--network", net, "postgres:17-alpine", "psql", dbURL, "-tA", "-v", "ON_ERROR_STOP=1"}, args...)...)
		if err != nil {
			t.Fatalf("psql %v: %v\n%s", args, err, out)
		}
		return out
	}
	// The app's database works with the URL it was given.
	psql("-c", "create table t(x int); insert into t values (41), (42)")
	if got := psql("-c", "select count(*) from t"); got != "2" {
		t.Fatalf("count = %q", got)
	}
	if out, err := sh("docker", "run", "--rm", "--network", net, "redis:7-alpine", "redis-cli", "-u", redisURL, "set", "k", "v"); err != nil || !strings.Contains(out, "OK") {
		t.Fatalf("redis: %v %s", err, out)
	}

	// Back up, lose the data, restore.
	bk := e.mustAd(t, "db", "backup", "svc")
	contains(t, bk, "backup written:")
	names := strings.Fields(e.mustAd(t, "db", "list", "svc"))
	if len(names) != 1 || !strings.HasSuffix(names[0], ".sql.gz") {
		t.Fatalf("backups: %v", names)
	}
	psql("-c", "delete from t")
	if got := psql("-c", "select count(*) from t"); got != "0" {
		t.Fatalf("count after delete = %q", got)
	}
	e.mustAd(t, "db", "restore", "svc", names[0])
	if got := psql("-c", "select count(*) from t"); got != "2" {
		t.Errorf("count after restore = %q, want 2", got)
	}
	if out, err := e.ad("db", "restore", "svc", "../../etc/passwd"); err == nil {
		t.Errorf("path traversal accepted: %s", out)
	}

	// A redeploy keeps the same database credentials.
	e.mustAd(t, "redeploy", "svc")
	if _, again := e.get("svc.test", "/env?name=DATABASE_URL"); again != dbURL {
		t.Errorf("database URL changed on redeploy")
	}
	// An app without services has no database to back up.
	e.newApp(t, "plain", "plain.test")
	if out, err := e.ad("db", "backup", "plain"); err == nil || !strings.Contains(out, "no database") {
		t.Errorf("backup of an app without a database: %v %s", err, out)
	}
}

// Load balancers can ask each server whether it serves an app.
func TestHealthEndpoint(t *testing.T) {
	e := env
	url := func(app string) int {
		code, _ := rawGet("http://" + e.healthAddr + "/health/" + app)
		return code
	}
	e.newApp(t, "lb", "lb.test")
	if got := url("lb"); got != 503 {
		t.Errorf("before the first deploy: %d, want 503", got)
	}
	r := e.newRepo(t, "hello")
	if out, err := r.push("lb"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if got := url("lb"); got != 200 {
		t.Errorf("serving: %d, want 200", got)
	}
	if got := url("nosuchapp"); got != 503 {
		t.Errorf("unknown app: %d, want 503", got)
	}
	// Stopping the container takes the server out of rotation.
	for _, c := range e.containers("lb") {
		sh("docker", "stop", "-t", "1", c)
	}
	waitFor(t, 10*time.Second, "503 after the container stopped", func() bool { return url("lb") == 503 })
}

// Leaving is one command: the live app's setup comes out in the format of
// another tool, with the names of its secrets and never their values.
func TestExportToOtherTools(t *testing.T) {
	e := env
	e.newApp(t, "exp", "exp.test")
	r := e.newRepo(t, "hello")
	r.write("appdeploy.yml", "roles:\n  web:\n    port: 8080\n    memory: 96M\n")
	r.commit("config")
	if out, err := r.push("exp"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	e.mustAd(t, "secrets", "set", "exp", "API_TOKEN=hunter2-never-exported")

	for _, format := range []string{"kamal", "compose", "k8s", "fly"} {
		out := e.mustAd(t, "export", "exp", "--format", format)
		contains(t, out, "API_TOKEN")
		contains(t, out, "exp.test")
		notContains(t, out, "hunter2-never-exported")
	}
	dir := filepath.Join(t.TempDir(), "out")
	e.mustAd(t, "export", "exp", "--format", "compose", "--out", dir)
	for _, f := range []string{"compose.yml", ".env.example", "Caddyfile"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	// The instructions in the file: copy .env.example to .env and fill it in.
	example, _ := os.ReadFile(filepath.Join(dir, ".env.example"))
	if err := os.WriteFile(filepath.Join(dir, ".env"), example, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := sh("docker", "compose", "-f", filepath.Join(dir, "compose.yml"), "config", "--quiet"); err != nil {
		t.Errorf("docker compose rejected the exported file: %v\n%s", err, out)
	}
	if out, err := e.ad("export", "exp", "--format", "terraform"); err == nil || !strings.Contains(out, "unknown format") {
		t.Errorf("unknown format: %v %s", err, out)
	}
	e.newApp(t, "fresh", "fresh.test")
	if out, err := e.ad("export", "fresh", "--format", "kamal"); err == nil || !strings.Contains(out, "no release") {
		t.Errorf("app without a release: %v %s", err, out)
	}
}
