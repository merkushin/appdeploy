//go:build e2e

// Package e2e runs the real agent against a real Docker daemon and a real
// Caddy (in a container), deploying the Go apps in examples/ through git push.
//
//	go test -tags e2e -timeout 30m ./e2e
//
// Everything the tests create is labeled with a per-run instance name and is
// removed when the run ends: containers, images, networks, directories.
package e2e

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"appdeploy/internal/api"
)

type testEnv struct {
	root       string // temp dir holding everything
	bin        string // built appdeploy binary
	instance   string // label/name prefix of everything this run creates
	dataDir    string
	socket     string
	adminPort  int // host port of Caddy's admin API
	httpPort   int // host port of the apps' HTTP listener
	repoRoot   string
	agent      *exec.Cmd
	agentDone  chan struct{}
	client     *api.Client
	healthAddr string

	// Self-update: the binary trusts updKey and looks for releases of acme/tool at gh.
	updKey ed25519.PrivateKey
	gh     *fakeGitHub
}

var env *testEnv

func TestMain(m *testing.M) {
	if err := exec.Command("docker", "version").Run(); err != nil {
		fmt.Println("e2e: docker is not available, skipping")
		os.Exit(0)
	}
	e, err := setup()
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e setup:", err)
		if e != nil {
			e.teardown()
		}
		os.Exit(1)
	}
	env = e
	code := m.Run()
	e.teardown()
	os.Exit(code)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func sh(name string, args ...string) (string, error) {
	var out bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

func setup() (*testEnv, error) {
	wd, _ := os.Getwd()
	repoRoot, _ := filepath.Abs(filepath.Join(wd, ".."))
	inst := "adt" + randHex(3)
	// Unix socket paths are limited to ~100 bytes, so keep the root short.
	root, err := os.MkdirTemp("", inst)
	if err != nil {
		return nil, err
	}
	e := &testEnv{
		root: root, instance: inst, repoRoot: repoRoot,
		bin:     filepath.Join(root, "appdeploy"),
		dataDir: filepath.Join(root, "data"),
		socket:  filepath.Join(root, "a.sock"),
	}
	e.client = api.NewClient(e.socket)
	e.healthAddr = freeAddr()

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	e.updKey = priv
	e.gh = newFakeGitHub()
	if out, err := e.buildBinary(e.bin, "0.0.1"); err != nil {
		return e, fmt.Errorf("building appdeploy: %v\n%s", err, out)
	}
	if err := e.startCaddy(); err != nil {
		return e, err
	}
	if err := e.startAgent(); err != nil {
		return e, err
	}
	return e, nil
}

func (e *testEnv) label() string { return "appdeploy.instance=" + e.instance }

func (e *testEnv) startCaddy() error {
	name := e.instance + "-caddy"
	if err := os.MkdirAll(filepath.Join(e.root, "caddylogs"), 0o777); err != nil {
		return err
	}
	_ = os.Chmod(filepath.Join(e.root, "caddylogs"), 0o777)
	script := `printf '{\n\tadmin :2019\n}\n' > /tmp/Caddyfile && exec caddy run --config /tmp/Caddyfile --adapter caddyfile`
	if out, err := sh("docker", "run", "-d", "--name", name, "--label", e.label(),
		"-v", filepath.Join(e.root, "caddylogs")+":/var/log/caddy",
		"-p", "127.0.0.1::2019", "-p", "127.0.0.1::8080", "caddy:2", "sh", "-c", script); err != nil {
		return fmt.Errorf("starting caddy: %v\n%s", err, out)
	}
	var err error
	if e.adminPort, err = hostPort(name, 2019); err != nil {
		return err
	}
	if e.httpPort, err = hostPort(name, 8080); err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/config/", e.adminPort))
		if err == nil {
			resp.Body.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	logs, _ := sh("docker", "logs", name)
	return fmt.Errorf("caddy admin API did not come up:\n%s", logs)
}

func hostPort(container string, port int) (int, error) {
	out, err := sh("docker", "port", container, fmt.Sprintf("%d/tcp", port))
	if err != nil {
		return 0, fmt.Errorf("docker port: %v: %s", err, out)
	}
	line := strings.Split(out, "\n")[0]
	return strconv.Atoi(line[strings.LastIndex(line, ":")+1:])
}

func (e *testEnv) agentArgs() []string {
	return []string{"agent",
		"--data-dir", e.dataDir, "--socket", e.socket, "--instance", e.instance,
		"--caddy-admin", fmt.Sprintf("http://127.0.0.1:%d", e.adminPort),
		"--caddy-listen", ":8080", "--caddy-admin-listen", ":2019", "--no-tls",
		// Caddy runs in a container, so it reaches apps through the host.
		"--upstream-host", "host.docker.internal",
		"--min-free-bytes", "1", "--min-free-frac", "0.0001",
		"--network-pool", "10.81.0.0/20", "--health-listen", e.healthAddr,
		"--monitor-every", "2s", "--update-api", e.gh.URL(), "--update-confirm-after", "2s",
		// The watchdog reads the access log Caddy writes into a shared directory.
		"--access-log", filepath.Join(e.root, "caddylogs", "access.log"),
		"--caddy-access-log", "/var/log/caddy/access.log",
		"--default-watch-window", "3s",
	}
}

func (e *testEnv) startAgent() error { return e.startAgentEnv() }

// startAgentEnv starts the agent with extra environment variables (KEY=value).
func (e *testEnv) startAgentEnv(extra ...string) error {
	logf, err := os.OpenFile(filepath.Join(e.root, "agent.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	cmd := exec.Command(e.bin, e.agentArgs()...)
	cmd.Env = append(os.Environ(), extra...)
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); logf.Close(); close(done) }()
	e.agent, e.agentDone = cmd, done
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := e.client.Ping(ctx)
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-done:
			return fmt.Errorf("agent exited early:\n%s", e.agentLog())
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("agent did not become ready:\n%s", e.agentLog())
}

// stopAgent sends sig to the agent and waits for it to exit.
func (e *testEnv) stopAgent(sig syscall.Signal) {
	if e.agent == nil || e.agent.Process == nil {
		return
	}
	_ = e.agent.Process.Signal(sig)
	select {
	case <-e.agentDone:
	case <-time.After(30 * time.Second):
		_ = e.agent.Process.Kill()
		<-e.agentDone
	}
	e.agent = nil
}

func (e *testEnv) agentLog() string {
	b, _ := os.ReadFile(filepath.Join(e.root, "agent.log"))
	return string(b)
}

func (e *testEnv) teardown() {
	e.stopAgent(syscall.SIGTERM)
	for _, f := range extraTeardown {
		f()
	}
	if e.gh != nil {
		e.gh.Close()
	}
	cleanupInstance(e.instance)
	if os.Getenv("E2E_KEEP") != "" {
		fmt.Println("e2e: keeping", e.root)
		return
	}
	_ = os.RemoveAll(e.root)
}

// ---- helpers used by the tests ----

// ad runs the CLI against the test agent and returns its combined output.
func (e *testEnv) ad(args ...string) (string, error) {
	return sh(e.bin, append([]string{"--socket", e.socket}, args...)...)
}

func (e *testEnv) mustAd(t *testing.T, args ...string) string {
	t.Helper()
	out, err := e.ad(args...)
	if err != nil {
		t.Fatalf("appdeploy %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

var httpClient = &http.Client{Timeout: 5 * time.Second}

// get requests path from the app routed by host, through Caddy.
func (e *testEnv) get(host, path string) (int, string) {
	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d%s", e.httpPort, path), nil)
	req.Host = host
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(b))
}

func (e *testEnv) expectBody(t *testing.T, host, path, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var code int
	var body string
	for time.Now().Before(deadline) {
		if code, body = e.get(host, path); code == 200 && body == want {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("GET %s%s = %d %q, want 200 %q", host, path, code, body, want)
}

// containers lists the container names of an app, running or not.
func (e *testEnv) containers(app string) []string {
	out, _ := sh("docker", "ps", "-a", "--filter", "label="+e.label(), "--filter", "label=appdeploy.app="+app,
		"--format", "{{.Names}}", "--no-trunc")
	if out == "" {
		return nil
	}
	lines := strings.Split(out, "\n")
	return lines
}

func (e *testEnv) imageExists(app string, release int) bool {
	_, err := sh("docker", "image", "inspect", fmt.Sprintf("%s/%s:v%d", e.instance, app, release))
	return err == nil
}

func (e *testEnv) status(t *testing.T, app string) api.StatusResponse {
	t.Helper()
	s, err := e.client.Status(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

// newApp creates an app on the agent and destroys it when the test ends.
func (e *testEnv) newApp(t *testing.T, name, domain string) {
	t.Helper()
	e.mustAd(t, "apps", "create", name, "--domain", domain)
	t.Cleanup(func() {
		// Wait for any deploy still in flight, then remove the app.
		for i := 0; i < 120; i++ {
			if _, err := e.ad("apps", "destroy", name); err == nil {
				return
			}
			time.Sleep(time.Second)
		}
	})
}

// ---- a git repository holding an example app ----

type repo struct {
	t    *testing.T
	dir  string
	env  *testEnv
	slow int
}

func (e *testEnv) newRepo(t *testing.T, example string) *repo {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "src")
	if out, err := sh("cp", "-R", filepath.Join(e.repoRoot, "examples", example), dir); err != nil {
		t.Fatalf("copying example: %v\n%s", err, out)
	}
	r := &repo{t: t, dir: dir, env: e}
	r.git("init", "-q", "-b", "main")
	r.commit("initial")
	return r
}

func (r *repo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = r.dir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out.String())
	}
	return strings.TrimSpace(out.String())
}

func (r *repo) write(name, content string) {
	r.t.Helper()
	p := filepath.Join(r.dir, name)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *repo) commit(msg string) {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "-q", "--allow-empty", "-m", msg)
}

// slowBuild makes the next build take about secs seconds, and never hit the cache.
func (r *repo) slowBuild(secs int) {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.dir, "Dockerfile"))
	if err != nil {
		r.t.Fatal(err)
	}
	lines := strings.Split(string(b), "\n")
	var out []string
	for _, l := range lines {
		out = append(out, l)
		if strings.HasPrefix(l, "COPY *.go") {
			out = append(out, fmt.Sprintf("RUN echo %s && sleep %d", randHex(6), secs))
		}
	}
	r.write("Dockerfile", strings.Join(out, "\n"))
}

func (r *repo) remote(app string) string {
	return filepath.Join(r.env.dataDir, "apps", app, "repo.git")
}

// push runs git push and returns its output (the deploy log, as "remote:" lines).
func (r *repo) push(app string) (string, error) {
	cmd := exec.Command("git", "push", "--force", r.remote(app), "HEAD:refs/heads/main")
	cmd.Dir = r.dir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

type asyncPush struct {
	cmd *exec.Cmd
	mu  sync.Mutex
	buf bytes.Buffer
}

func (a *asyncPush) Write(p []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.buf.Write(p)
}

func (a *asyncPush) output() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.buf.String()
}

func (r *repo) pushAsync(app string) *asyncPush {
	a := &asyncPush{}
	a.cmd = exec.Command("git", "push", "--force", r.remote(app), "HEAD:refs/heads/main")
	a.cmd.Dir = r.dir
	a.cmd.Stdout, a.cmd.Stderr = a, a
	if err := a.cmd.Start(); err != nil {
		r.t.Fatal(err)
	}
	go func() { _ = a.cmd.Wait() }()
	return a
}

func (a *asyncPush) waitOutput(t *testing.T, substr string, timeout time.Duration) {
	t.Helper()
	waitFor(t, timeout, fmt.Sprintf("push output to contain %q (got %q)", substr, a.output()), func() bool {
		return strings.Contains(a.output(), substr)
	})
}

// extraTeardown holds cleanups of things tests started lazily (the server group).
var extraTeardown []func()

// cleanupInstance removes every Docker object labeled with the agent instance.
func cleanupInstance(instance string) {
	// Remove everything labeled with this run's instance.
	for _, q := range [][]string{
		{"ps", "-aq", "--filter", "label=" + "appdeploy.instance=" + instance},
	} {
		if ids, _ := sh("docker", q...); ids != "" {
			_, _ = sh("docker", append([]string{"rm", "-f", "-v"}, strings.Fields(ids)...)...)
		}
	}
	if ids, _ := sh("docker", "images", "-q", "--filter", "label="+"appdeploy.instance="+instance); ids != "" {
		_, _ = sh("docker", append([]string{"rmi", "-f"}, strings.Fields(ids)...)...)
	}
	// Image tags of releases whose labels were lost with their build.
	if tags, _ := sh("docker", "images", "--format", "{{.Repository}}:{{.Tag}}", "--filter", "reference="+instance+"/*"); tags != "" {
		_, _ = sh("docker", append([]string{"rmi", "-f"}, strings.Fields(tags)...)...)
	}
	// Volumes of apps (labeled) and of the Postgres/Redis services (named <instance>-svc-*).
	if vols, _ := sh("docker", "volume", "ls", "-q", "--filter", "label=appdeploy.instance="+instance); vols != "" {
		_, _ = sh("docker", append([]string{"volume", "rm", "-f"}, strings.Fields(vols)...)...)
	}
	// Volumes of the Postgres/Redis services (named <instance>-svc-*).
	if vols, _ := sh("docker", "volume", "ls", "-q", "--filter", "name="+instance+"-svc"); vols != "" {
		_, _ = sh("docker", append([]string{"volume", "rm", "-f"}, strings.Fields(vols)...)...)
	}
	if ids, _ := sh("docker", "network", "ls", "-q", "--filter", "label="+"appdeploy.instance="+instance); ids != "" {
		_, _ = sh("docker", append([]string{"network", "rm"}, strings.Fields(ids)...)...)
	}
}

// rawGet requests a URL and returns the status and the first part of the body.
func rawGet(url string) (int, string) {
	resp, err := httpClient.Get(url)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b := make([]byte, 4096)
	n, _ := resp.Body.Read(b)
	return resp.StatusCode, strings.TrimSpace(string(b[:n]))
}

// freeAddr returns a loopback address with a free port.
func freeAddr() string {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	return l.Addr().String()
}

// buildBinary builds the appdeploy binary as the given version. It trusts the
// run's signing key and looks for releases of acme/tool.
func (e *testEnv) buildBinary(path, version string, extraLdflags ...string) (string, error) {
	pub := base64.StdEncoding.EncodeToString(e.updKey.Public().(ed25519.PublicKey))
	ldflags := strings.Join(append([]string{fmt.Sprintf("-X main.version=%s -X main.updatePublicKey=%s -X main.updateRepo=acme/tool", version, pub)}, extraLdflags...), " ")
	return sh("go", "build", "-ldflags", ldflags, "-o", path, filepath.Join(e.repoRoot, "cmd", "appdeploy"))
}

// read returns the content of a file of the repository.
func (r *repo) read(name string) string {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.dir, name))
	if err != nil {
		r.t.Fatal(err)
	}
	return string(b)
}
