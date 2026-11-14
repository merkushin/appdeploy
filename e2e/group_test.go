//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"appdeploy/internal/api"
)

// member is one simulated server: its own agent, data directory and Docker
// name prefix, sharing the Docker daemon (and the machine) with the others.
type member struct {
	name, instance, dataDir, socket, peerAddr, labels, pool string
	cmd                                                     *exec.Cmd
	done                                                    chan struct{}
	client                                                  *api.Client
}

type groupEnv struct {
	a, b, c, d *member
}

var theGroup *groupEnv

func (m *member) args() []string {
	return []string{"agent", "--data-dir", m.dataDir, "--socket", m.socket, "--instance", m.instance,
		"--caddy-admin", "none", "--min-free-bytes", "1", "--min-free-frac", "0.0001", "--default-watch-window", "3s", "--network-pool", m.pool}
}

func (m *member) start(t testing.TB) {
	t.Helper()
	logf, err := os.OpenFile(filepath.Join(m.dataDir, "..", m.name+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	m.cmd = exec.Command(env.bin, m.args()...)
	m.cmd.Stdout, m.cmd.Stderr = logf, logf
	if err := m.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	m.done = make(chan struct{})
	go func(c *exec.Cmd, done chan struct{}) { _ = c.Wait(); logf.Close(); close(done) }(m.cmd, m.done)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := m.client.Ping(ctx)
		cancel()
		if err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("agent %s did not start", m.name)
}

func (m *member) stop(sig syscall.Signal) {
	if m.cmd == nil || m.cmd.Process == nil {
		return
	}
	_ = m.cmd.Process.Signal(sig)
	select {
	case <-m.done:
	case <-time.After(30 * time.Second):
		_ = m.cmd.Process.Kill()
		<-m.done
	}
	m.cmd = nil
}

func (m *member) cli(args ...string) (string, error) {
	return sh(env.bin, append([]string{"--socket", m.socket}, args...)...)
}

// needGroup starts four servers once: a (web, db; founder), b (web), c (worker)
// and d (builder: runs no app, only builds).
func needGroup(t *testing.T) *groupEnv {
	t.Helper()
	if theGroup != nil {
		return theGroup
	}
	root, err := os.MkdirTemp("", "adg")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name, labels string) *member {
		inst := "adg" + name + randHex(2)
		m := &member{name: name, instance: inst, labels: labels, peerAddr: freeAddr(), pool: "10.80." + map[string]string{"a": "0", "b": "16", "c": "32", "d": "48"}[name] + ".0/20",
			dataDir: filepath.Join(root, name, "data"), socket: filepath.Join(root, name+".sock")}
		_ = os.MkdirAll(m.dataDir, 0o755)
		m.client = api.NewClient(m.socket)
		return m
	}
	g := &groupEnv{a: mk("a", "web,db"), b: mk("b", "web"), c: mk("c", "worker"), d: mk("d", "builder")}
	theGroup = g
	extraTeardown = append(extraTeardown, func() {
		for _, m := range []*member{g.a, g.b, g.c, g.d} {
			m.stop(syscall.SIGTERM)
			cleanupInstance(m.instance)
		}
		if os.Getenv("E2E_KEEP") == "" {
			_ = os.RemoveAll(root)
		}
	})
	run := func(m *member, args ...string) string {
		out, err := sh(env.bin, append([]string{"group"}, args...)...)
		_ = out
		if err != nil {
			t.Fatalf("group %v: %v\n%s", args, err, out)
		}
		return out
	}
	_ = run
	mustGroup := func(args ...string) string {
		out, err := sh(env.bin, args...)
		if err != nil {
			t.Fatalf("appdeploy %v: %v\n%s", args, err, out)
		}
		return out
	}
	mustGroup("group", "init", "--data-dir", g.a.dataDir, "--group", "prod", "--server", "a", "--advertise", g.a.peerAddr, "--labels", g.a.labels)
	g.a.start(t)
	for _, m := range []*member{g.b, g.c, g.d} {
		out, err := g.a.cli("group", "invite")
		if err != nil {
			t.Fatalf("invite: %v\n%s", err, out)
		}
		tok := out[strings.LastIndex(out, "--token ")+len("--token "):]
		mustGroup("group", "join", "--data-dir", m.dataDir, "--server", m.name, "--advertise", m.peerAddr, "--labels", m.labels, "--token", strings.TrimSpace(tok))
		m.start(t)
	}
	return g
}

func (m *member) newApp(t *testing.T, name string) {
	t.Helper()
	if out, err := m.cli("apps", "create", name); err != nil {
		t.Fatalf("apps create: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		for i := 0; i < 60; i++ {
			if _, err := m.cli("apps", "destroy", name); err == nil {
				return
			}
			time.Sleep(time.Second)
		}
	})
}

// gpush pushes the repo to the app on member m.
func (r *repo) gpush(m *member, app string) (string, error) {
	cmd := exec.Command("git", "push", "--force", filepath.Join(m.dataDir, "apps", app, "repo.git"), "HEAD:refs/heads/main")
	cmd.Dir = r.dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// webBody asks the app's web container on the member (through its published port).
func (m *member) webBody(t *testing.T, app string) string {
	t.Helper()
	s, err := m.client.Status(context.Background(), app)
	if err != nil {
		return "status: " + err.Error()
	}
	for _, c := range s.Containers {
		if c.Role == "web" && c.Running && c.Release == s.App.CurrentRelease && c.HostPort > 0 {
			code, body := rawGet(fmt.Sprintf("http://127.0.0.1:%d/", c.HostPort))
			if code == 200 {
				return body
			}
			return fmt.Sprintf("HTTP %d", code)
		}
	}
	return "no web container"
}

func (m *member) expectWeb(t *testing.T, app, want string) {
	t.Helper()
	waitFor(t, 15*time.Second, m.name+" to serve "+want, func() bool { return m.webBody(t, app) == want })
}

func (m *member) roles(t *testing.T, app string) string {
	t.Helper()
	s, err := m.client.Status(context.Background(), app)
	if err != nil {
		return "(" + err.Error() + ")"
	}
	var rs []string
	for _, c := range s.Containers {
		if c.Running {
			rs = append(rs, fmt.Sprintf("%s:v%d", c.Role, c.Release))
		}
	}
	if len(rs) == 0 {
		return fmt.Sprintf("none running; current=v%d containers=%+v deploys=%+v", s.App.CurrentRelease, s.Containers, s.Deploys)
	}
	return strings.Join(rs, ",")
}

const groupConfig = `
watch:
  window: 6s
roles:
  web:
    on: [web]
    memory: 64M
    healthcheck:
      start_period: 6s
  worker:
    on: [worker]
    cmd: worker
    memory: 32M
`

func TestGroupMembership(t *testing.T) {
	g := needGroup(t)
	for _, m := range []*member{g.a, g.b, g.c, g.d} {
		m := m
		waitFor(t, 15*time.Second, m.name+" to know all 4 servers", func() bool {
			out, err := m.cli("group", "servers")
			return err == nil && strings.Count(out, "\n") == 4 // header + 4 lines, trailing newline trimmed
		})
	}
	// Only the founder can invite or remove servers.
	if out, err := g.b.cli("group", "invite"); err == nil {
		t.Errorf("a member invited: %s", out)
	}
	if out, err := g.b.cli("group", "remove", "c"); err == nil {
		t.Errorf("a member removed a server: %s", out)
	}
}

// One push builds once, ships the image to the other servers and starts each
// role only where its labels say, one wave after the other.
func TestGroupRolloutAndRollback(t *testing.T) {
	g := needGroup(t)
	g.a.newApp(t, "gapp")
	r := env.newRepo(t, "hello")
	r.write("appdeploy.yml", groupConfig)
	r.commit("v1")
	out, err := r.gpush(g.a, "gapp")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	contains(t, out, "rolling out v1 to 3 server(s): a, b, c")
	contains(t, out, "building on d")
	contains(t, out, "[d] ==> building") // the build ran on the builder, not on a
	contains(t, out, "image delivered to b")
	contains(t, out, "wave 1: a")
	contains(t, out, "wave 3: c")
	contains(t, out, "==> succeeded")
	if got := g.a.roles(t, "gapp"); got != "web:v1" {
		t.Errorf("a runs %q, want web only", got)
	}
	if got := g.b.roles(t, "gapp"); got != "web:v1" {
		t.Errorf("b runs %q, want web only", got)
	}
	if got := g.c.roles(t, "gapp"); got != "worker:v1" {
		t.Errorf("c runs %q, want the worker only", got)
	}
	g.a.expectWeb(t, "gapp", "hello v1")
	g.b.expectWeb(t, "gapp", "hello v1")
	// The build server keeps nothing of it, and runs no app.
	if _, err := sh("docker", "image", "inspect", g.d.instance+"/gapp:v1"); err == nil {
		t.Errorf("the builder still has the image of v1")
	}
	if got := g.d.roles(t, "gapp"); !strings.HasPrefix(got, "none running") {
		t.Errorf("the builder runs %q", got)
	}

	// An update replaces the release everywhere and retires the old one.
	r.write("message.txt", "hello v2\n")
	r.commit("v2")
	if out, err := r.gpush(g.a, "gapp"); err != nil || !strings.Contains(out, "==> succeeded") {
		t.Fatalf("push v2: %v\n%s", err, out)
	}
	g.a.expectWeb(t, "gapp", "hello v2")
	g.b.expectWeb(t, "gapp", "hello v2")
	waitFor(t, 20*time.Second, "old releases to be retired", func() bool {
		return g.a.roles(t, "gapp") == "web:v2" && g.b.roles(t, "gapp") == "web:v2" && g.c.roles(t, "gapp") == "worker:v2"
	})

	// A rollback goes back everywhere.
	out, err = g.a.cli("rollback", "gapp")
	if err != nil {
		t.Fatalf("rollback: %v\n%s", err, out)
	}
	g.a.expectWeb(t, "gapp", "hello v1")
	g.b.expectWeb(t, "gapp", "hello v1")
	waitFor(t, 20*time.Second, fmt.Sprintf("rollback to settle (a=%q b=%q c=%q)", g.a.roles(t, "gapp"), g.b.roles(t, "gapp"), g.c.roles(t, "gapp")), func() bool {
		return g.a.roles(t, "gapp") == "web:v1" && g.b.roles(t, "gapp") == "web:v1" && g.c.roles(t, "gapp") == "worker:v1"
	})
}

// A failure in a later wave takes the release off the servers that already switched.
func TestGroupFailureUndoesEarlierWaves(t *testing.T) {
	g := needGroup(t)
	g.a.newApp(t, "gfail")
	r := env.newRepo(t, "broken")
	cfg := "watch:\n  window: 4s\nroles:\n  web:\n    on: [web]\n    memory: 64M\n    healthcheck:\n      start_period: 4s\n"
	r.write("appdeploy.yml", cfg)
	r.commit("v1")
	if out, err := r.gpush(g.a, "gfail"); err != nil || !strings.Contains(out, "==> succeeded") {
		t.Fatalf("v1: %v\n%s", err, out)
	}
	g.a.expectWeb(t, "gfail", "broken app, mode ok")

	r.write("mode.txt", "fail-on:b\n")
	r.commit("v2 fails on b")
	out, _ := r.gpush(g.a, "gfail")
	contains(t, out, "wave 1: a")
	contains(t, out, "[a] ==> succeeded") // the canary was fine
	contains(t, out, "[b] ")
	contains(t, out, "rollout stopped in wave 2")
	contains(t, out, "undoing v2 on 1 server(s)")

	// Everything is back on v1; nothing of v2 is left behind.
	for _, m := range []*member{g.a, g.b} {
		m.expectWeb(t, "gfail", "broken app, mode ok")
		waitFor(t, 20*time.Second, m.name+" to be only on v1", func() bool { return m.roles(t, "gfail") == "web:v1" })
		s := env.status2(t, m, "gfail")
		if s.App.CurrentRelease != 1 {
			t.Errorf("%s is on v%d", m.name, s.App.CurrentRelease)
		}
	}
	for _, d := range env.status2(t, g.a, "gfail").Deploys {
		if d.Source == "push" && d.Release == 2 && d.State != "failed" {
			t.Errorf("the coordinator's deploy of v2 is %q", d.State)
		}
	}
}

func (e *testEnv) status2(t *testing.T, m *member, app string) api.StatusResponse {
	t.Helper()
	s, err := m.client.Status(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The coordinator dies in the middle of a rollout. When it is back it asks the
// servers again; those that already switched keep serving without a restart.
func TestGroupCoordinatorCrashResumes(t *testing.T) {
	g := needGroup(t)
	g.a.newApp(t, "gcrash")
	r := env.newRepo(t, "hello")
	r.write("appdeploy.yml", groupConfig)
	r.commit("v1")
	if out, err := r.gpush(g.a, "gcrash"); err != nil || !strings.Contains(out, "==> succeeded") {
		t.Fatalf("v1: %v\n%s", err, out)
	}
	r.write("message.txt", "hello v2\n")
	r.commit("v2")
	cmd := exec.Command("git", "push", "--force", filepath.Join(g.a.dataDir, "apps", "gcrash", "repo.git"), "HEAD:refs/heads/main")
	cmd.Dir = r.dir
	pushed := &asyncPush{cmd: cmd}
	cmd.Stdout, cmd.Stderr = pushed, pushed
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	pushed.waitOutput(t, "wave 2: b", 120*time.Second)

	g.a.stop(syscall.SIGKILL)
	_ = cmd.Process.Kill()
	g.a.start(t)

	waitFor(t, 3*time.Minute, "the resumed rollout to finish on every server", func() bool {
		s, err := g.a.client.Status(context.Background(), "gcrash")
		if err != nil {
			return false
		}
		for _, d := range s.Deploys {
			if d.Source == "push" && d.Release == 2 {
				return d.State == "succeeded"
			}
		}
		return false
	})
	g.a.expectWeb(t, "gcrash", "hello v2")
	g.b.expectWeb(t, "gcrash", "hello v2")
	waitFor(t, 30*time.Second, "old releases to be retired", func() bool {
		return g.a.roles(t, "gcrash") == "web:v2" && g.b.roles(t, "gcrash") == "web:v2" && g.c.roles(t, "gcrash") == "worker:v2"
	})
}

// A server that is down during a rollout is skipped when the app allows it,
// refused otherwise, and brings itself up to date when it is back.
func TestGroupServerCatchesUpAfterDowntime(t *testing.T) {
	g := needGroup(t)
	g.a.newApp(t, "gcatch")
	r := env.newRepo(t, "hello")
	r.write("appdeploy.yml", groupConfig+"allow_partial: true\n")
	r.commit("v1")
	if out, err := r.gpush(g.a, "gcatch"); err != nil || !strings.Contains(out, "==> succeeded") {
		t.Fatalf("v1: %v\n%s", err, out)
	}
	waitFor(t, 20*time.Second, "c to run the worker", func() bool { return g.c.roles(t, "gcatch") == "worker:v1" })

	g.c.stop(syscall.SIGKILL)

	// With allow_partial the others get the release and the missing server is named.
	r.write("message.txt", "hello v2\n")
	r.commit("v2")
	out, _ := r.gpush(g.a, "gcatch")
	contains(t, out, "WARNING: skipping c")
	contains(t, out, "==> succeeded")
	g.a.expectWeb(t, "gcatch", "hello v2")
	g.b.expectWeb(t, "gcatch", "hello v2")

	// Without it, an unreachable server stops the deploy before anything changes.
	r.write("appdeploy.yml", groupConfig)
	r.write("message.txt", "hello v3\n")
	r.commit("v3 without allow_partial")
	out, _ = r.gpush(g.a, "gcatch")
	contains(t, out, "server c is not reachable")
	g.a.expectWeb(t, "gcatch", "hello v2")

	// When c is back it finds out what the group runs and runs its part of it.
	g.c.start(t)
	waitFor(t, 90*time.Second, "c to catch up to v2", func() bool { return g.c.roles(t, "gcatch") == "worker:v2" })
	s := env.status2(t, g.c, "gcatch")
	if s.App.CurrentRelease != 2 {
		t.Errorf("c is on v%d, want v2", s.App.CurrentRelease)
	}
	// The build server carries no role label, so it catches up with nothing.
	if got := g.d.roles(t, "gcatch"); !strings.HasPrefix(got, "none running") {
		t.Errorf("the builder runs %q", got)
	}
}
