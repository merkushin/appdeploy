// Package docker drives the Docker Engine through the docker CLI.
package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// Client runs docker commands. The zero value uses "docker" from PATH.
type Client struct {
	Bin string
	// NetworkPool, if set (a CIDR such as 10.80.0.0/20), is where /24 subnets
	// for new networks come from. Docker's own default pool holds only about
	// 30 networks, which limits how many apps one server can run.
	NetworkPool string
}

func (c *Client) bin() string {
	if c.Bin == "" {
		return "docker"
	}
	return c.Bin
}

// Error is a failed docker command with its stderr.
type Error struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = e.Err.Error()
	}
	return fmt.Sprintf("docker %s: %s", strings.Join(e.Args[:min(len(e.Args), 2)], " "), msg)
}

func (e *Error) Unwrap() error { return e.Err }

// run executes docker and returns trimmed stdout.
func (c *Client) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, c.bin(), args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", &Error{Args: args, Stderr: errb.String(), Err: err}
	}
	return strings.TrimSpace(out.String()), nil
}

// IsNoSuchObject reports whether err means the container/image/network is gone.
func IsNoSuchObject(err error) bool {
	var de *Error
	if errors.As(err, &de) {
		s := strings.ToLower(de.Stderr)
		return strings.Contains(s, "no such") || strings.Contains(s, "not found")
	}
	return false
}

// Ping checks that the Docker daemon is reachable.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.run(ctx, "version", "--format", "{{.Server.Version}}")
	return err
}

// MemTotal returns the memory available to Docker, in bytes.
func (c *Client) MemTotal(ctx context.Context) (int64, error) {
	s, err := c.run(ctx, "info", "--format", "{{.MemTotal}}")
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(s, 10, 64)
}

// ---- images ----

type BuildSpec struct {
	Tag        string
	ContextDir string
	Dockerfile string // path relative to ContextDir
	Args       map[string]string
	Labels     map[string]string
}

// Build builds an image, streaming build output lines to out.
func (c *Client) Build(ctx context.Context, b BuildSpec, out io.Writer) error {
	args := []string{"build", "--progress=plain", "-t", b.Tag, "-f", b.Dockerfile}
	for k, v := range b.Args {
		args = append(args, "--build-arg", k+"="+v)
	}
	for k, v := range b.Labels {
		args = append(args, "--label", k+"="+v)
	}
	args = append(args, ".")
	cmd := exec.CommandContext(ctx, c.bin(), args...)
	cmd.Dir = b.ContextDir
	cmd.Env = append(cmd.Environ(), "DOCKER_BUILDKIT=1")
	var tail tailBuffer
	w := io.MultiWriter(out, &tail)
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Run(); err != nil {
		return &Error{Args: args, Stderr: tail.String(), Err: err}
	}
	return nil
}

// tailBuffer keeps the last few KB written to it, for error messages.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 4096 {
		t.buf = t.buf[len(t.buf)-4096:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// ImageExposedPort returns the lowest TCP port the image EXPOSEs, or 0.
func (c *Client) ImageExposedPort(ctx context.Context, image string) (int, error) {
	s, err := c.run(ctx, "image", "inspect", "--format", "{{json .Config.ExposedPorts}}", image)
	if err != nil {
		return 0, err
	}
	var ports map[string]struct{}
	if s == "" || s == "null" {
		return 0, nil
	}
	if err := json.Unmarshal([]byte(s), &ports); err != nil {
		return 0, err
	}
	best := 0
	for p := range ports {
		num, proto, _ := strings.Cut(p, "/")
		if proto != "tcp" {
			continue
		}
		n, err := strconv.Atoi(num)
		if err == nil && (best == 0 || n < best) {
			best = n
		}
	}
	return best, nil
}

// Pull downloads an image, streaming progress lines to out.
func (c *Client) Pull(ctx context.Context, ref string, out io.Writer) error {
	cmd := exec.CommandContext(ctx, c.bin(), "pull", "--quiet", ref)
	var tail tailBuffer
	w := io.MultiWriter(out, &tail)
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Run(); err != nil {
		return &Error{Args: []string{"pull", ref}, Stderr: tail.String(), Err: err}
	}
	return nil
}

// Tag adds a new name to an existing image.
func (c *Client) Tag(ctx context.Context, src, dst string) error {
	_, err := c.run(ctx, "tag", src, dst)
	return err
}

func (c *Client) ImageExists(ctx context.Context, image string) bool {
	_, err := c.run(ctx, "image", "inspect", "--format", "{{.Id}}", image)
	return err == nil
}

// RemoveImage removes an image tag. A missing image is not an error.
func (c *Client) RemoveImage(ctx context.Context, image string) error {
	_, err := c.run(ctx, "rmi", image)
	if err != nil && IsNoSuchObject(err) {
		return nil
	}
	return err
}

// Save writes the image as a tar stream to w.
func (c *Client) Save(ctx context.Context, image string, w io.Writer) error {
	cmd := exec.CommandContext(ctx, c.bin(), "save", image)
	cmd.Stdout = w
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return &Error{Args: []string{"save", image}, Stderr: errb.String(), Err: err}
	}
	return nil
}

// Load reads an image tar stream and returns the tag it loaded.
func (c *Client) Load(ctx context.Context, r io.Reader) (string, error) {
	cmd := exec.CommandContext(ctx, c.bin(), "load")
	cmd.Stdin = r
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", &Error{Args: []string{"load"}, Stderr: errb.String(), Err: err}
	}
	for _, l := range strings.Split(out.String(), "\n") {
		if t, ok := strings.CutPrefix(strings.TrimSpace(l), "Loaded image: "); ok {
			return t, nil
		}
	}
	return "", fmt.Errorf("docker load: no image tag in output %q", strings.TrimSpace(out.String()))
}

// PruneImages removes dangling images carrying the label (leftovers of
// superseded builds), and reports how much space that freed.
func (c *Client) PruneImages(ctx context.Context, label string) (string, error) {
	return c.run(ctx, "image", "prune", "-f", "--filter", "label="+label)
}

// PruneBuildCache removes build cache not used for the given duration.
func (c *Client) PruneBuildCache(ctx context.Context, unusedFor string) (string, error) {
	return c.run(ctx, "builder", "prune", "-f", "--filter", "until="+unusedFor)
}

// ---- networks ----

// EnsureNetwork creates a bridge network if it does not exist.
func (c *Client) EnsureNetwork(ctx context.Context, name string, labels map[string]string) error {
	if _, err := c.run(ctx, "network", "inspect", name); err == nil {
		return nil
	}
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		args := []string{"network", "create"}
		if c.NetworkPool != "" {
			subnet, serr := c.freeSubnet(ctx)
			if serr != nil {
				return serr
			}
			args = append(args, "--subnet", subnet)
		}
		for k, v := range labels {
			args = append(args, "--label", k+"="+v)
		}
		args = append(args, name)
		_, err = c.run(ctx, args...)
		if err == nil || strings.Contains(err.Error(), "already exists") {
			return nil
		}
		// Another creator may have taken the subnet between listing and creating.
		if !strings.Contains(err.Error(), "overlap") {
			return err
		}
	}
	return err
}

// freeSubnet returns the first /24 of the pool that no existing network uses.
func (c *Client) freeSubnet(ctx context.Context) (string, error) {
	pool, err := netip.ParsePrefix(c.NetworkPool)
	if err != nil || !pool.Addr().Is4() || pool.Bits() > 24 {
		return "", fmt.Errorf("network pool %q must be an IPv4 CIDR of at least a /24", c.NetworkPool)
	}
	ids, err := c.run(ctx, "network", "ls", "-q")
	if err != nil {
		return "", err
	}
	var used []netip.Prefix
	if ids != "" {
		out, err := c.run(ctx, append([]string{"network", "inspect", "--format", "{{range .IPAM.Config}}{{.Subnet}} {{end}}"}, strings.Fields(ids)...)...)
		if err != nil {
			return "", err
		}
		for _, f := range strings.Fields(out) {
			if p, err := netip.ParsePrefix(f); err == nil {
				used = append(used, p)
			}
		}
	}
	pool = pool.Masked()
	base := pool.Addr().As4()
	n := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
	size := uint32(1) << (32 - pool.Bits())
	for off := uint32(0); off < size; off += 256 {
		v := n + off
		cand := netip.PrefixFrom(netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}), 24)
		free := true
		for _, u := range used {
			if u.Overlaps(cand) {
				free = false
				break
			}
		}
		if free {
			return cand.String(), nil
		}
	}
	return "", fmt.Errorf("network pool %s has no free /24 left", c.NetworkPool)
}

func (c *Client) RemoveNetwork(ctx context.Context, name string) error {
	_, err := c.run(ctx, "network", "rm", name)
	if err != nil && IsNoSuchObject(err) {
		return nil
	}
	return err
}

// ---- containers ----

type RunSpec struct {
	Name        string
	Image       string
	Network     string
	EnvFile     string
	Memory      int64 // bytes, 0 = unlimited
	CPUs        float64
	Cmd         []string
	PublishPort int // container port to publish on 127.0.0.1 with a random host port; 0 = none
	Labels      map[string]string
	StopTimeout int      // seconds
	Volumes     []string // "name:/path" mounts
}

// Run starts a detached container and returns its id.
func (c *Client) Run(ctx context.Context, s RunSpec) (string, error) {
	args := []string{"run", "-d", "--name", s.Name, "--init",
		"--restart", "unless-stopped",
		"--log-driver", "json-file", "--log-opt", "max-size=20m", "--log-opt", "max-file=3"}
	if s.Network != "" {
		args = append(args, "--network", s.Network)
	}
	if s.EnvFile != "" {
		args = append(args, "--env-file", s.EnvFile)
	}
	if s.Memory > 0 {
		// Setting swap equal to memory disables swap for the container.
		args = append(args, "--memory", strconv.FormatInt(s.Memory, 10), "--memory-swap", strconv.FormatInt(s.Memory, 10))
	}
	if s.CPUs > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(s.CPUs, 'f', -1, 64))
	}
	if s.PublishPort > 0 {
		args = append(args, "-p", fmt.Sprintf("127.0.0.1::%d", s.PublishPort))
	}
	if s.StopTimeout > 0 {
		args = append(args, "--stop-timeout", strconv.Itoa(s.StopTimeout))
	}
	for _, v := range s.Volumes {
		args = append(args, "-v", v)
	}
	for k, v := range s.Labels {
		args = append(args, "--label", k+"="+v)
	}
	args = append(args, s.Image)
	args = append(args, s.Cmd...)
	return c.run(ctx, args...)
}

// HostPort returns the loopback host port a container port is published on.
func (c *Client) HostPort(ctx context.Context, id string, containerPort int) (int, error) {
	s, err := c.run(ctx, "port", id, strconv.Itoa(containerPort)+"/tcp")
	if err != nil {
		return 0, err
	}
	// Output lines look like "127.0.0.1:49153" (and possibly an IPv6 line).
	for _, line := range strings.Split(s, "\n") {
		i := strings.LastIndex(line, ":")
		if i < 0 {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(line[i+1:])); err == nil {
			return n, nil
		}
	}
	return 0, fmt.Errorf("no published port for container %s port %d", id, containerPort)
}

type State struct {
	Running      bool
	Restarting   bool
	ExitCode     int
	RestartCount int
	OOMKilled    bool
}

func (c *Client) Inspect(ctx context.Context, id string) (State, error) {
	s, err := c.run(ctx, "inspect", "--format", "{{json .State}}|{{.RestartCount}}", id)
	if err != nil {
		return State{}, err
	}
	js, rc, _ := strings.Cut(s, "|")
	var raw struct {
		Running    bool
		Restarting bool
		ExitCode   int
		OOMKilled  bool
	}
	if err := json.Unmarshal([]byte(js), &raw); err != nil {
		return State{}, err
	}
	n, _ := strconv.Atoi(rc)
	return State{Running: raw.Running, Restarting: raw.Restarting, ExitCode: raw.ExitCode, RestartCount: n, OOMKilled: raw.OOMKilled}, nil
}

// Stop sends SIGTERM and waits up to timeoutSec before killing the container.
func (c *Client) Stop(ctx context.Context, id string, timeoutSec int) error {
	_, err := c.run(ctx, "stop", "-t", strconv.Itoa(timeoutSec), id)
	if err != nil && IsNoSuchObject(err) {
		return nil
	}
	return err
}

// Remove force-removes a container. A missing container is not an error.
func (c *Client) Remove(ctx context.Context, id string) error {
	_, err := c.run(ctx, "rm", "-f", "-v", id)
	if err != nil && IsNoSuchObject(err) {
		return nil
	}
	return err
}

// ListByLabel returns ids of all containers (running or not) with the label.
func (c *Client) ListByLabel(ctx context.Context, label string) ([]string, error) {
	s, err := c.run(ctx, "ps", "-aq", "--filter", "label="+label)
	if err != nil || s == "" {
		return nil, err
	}
	return strings.Fields(s), nil
}

// RunOnce runs a one-off container to completion (e.g. a release command),
// streaming its output to out. The container is removed afterwards.
func (c *Client) RunOnce(ctx context.Context, s RunSpec, out io.Writer) error {
	args := []string{"run", "--rm", "--init", "--name", s.Name}
	if s.Network != "" {
		args = append(args, "--network", s.Network)
	}
	if s.EnvFile != "" {
		args = append(args, "--env-file", s.EnvFile)
	}
	if s.Memory > 0 {
		args = append(args, "--memory", strconv.FormatInt(s.Memory, 10), "--memory-swap", strconv.FormatInt(s.Memory, 10))
	}
	for k, v := range s.Labels {
		args = append(args, "--label", k+"="+v)
	}
	args = append(args, s.Image)
	args = append(args, s.Cmd...)
	cmd := exec.CommandContext(ctx, c.bin(), args...)
	var tail tailBuffer
	w := io.MultiWriter(out, &tail)
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Run(); err != nil {
		return &Error{Args: args, Stderr: tail.String(), Err: err}
	}
	return nil
}

// Logs streams a container's logs to out. With follow it runs until ctx ends.
func (c *Client) Logs(ctx context.Context, id string, follow bool, tail int, out io.Writer) error {
	args := []string{"logs", "--timestamps"}
	if follow {
		args = append(args, "-f")
	}
	if tail >= 0 {
		args = append(args, "--tail", strconv.Itoa(tail))
	}
	args = append(args, id)
	cmd := exec.CommandContext(ctx, c.bin(), args...)
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// Exec runs a command in a running container (no TTY), streaming output to out.
func (c *Client) Exec(ctx context.Context, id string, cmdline []string, out io.Writer) error {
	args := append([]string{"exec", id}, cmdline...)
	cmd := exec.CommandContext(ctx, c.bin(), args...)
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}

// ExecIO runs a command in a running container with optional environment
// (KEY=value) and stdin, writing its output to stdout and stderr.
func (c *Client) ExecIO(ctx context.Context, id string, env, cmdline []string, stdin io.Reader, stdout, stderr io.Writer) error {
	args := []string{"exec"}
	if stdin != nil {
		args = append(args, "-i")
	}
	for _, e := range env {
		args = append(args, "-e", e)
	}
	args = append(args, id)
	args = append(args, cmdline...)
	cmd := exec.CommandContext(ctx, c.bin(), args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return cmd.Run()
}

// ConnectNetwork attaches a running container to a network. Already being
// attached is not an error.
func (c *Client) ConnectNetwork(ctx context.Context, network, container string) error {
	_, err := c.run(ctx, "network", "connect", network, container)
	if err != nil && strings.Contains(err.Error(), "already exists") {
		return nil
	}
	return err
}

// ContainerRunning reports whether the named container exists and runs.
func (c *Client) ContainerRunning(ctx context.Context, name string) (exists, running bool) {
	st, err := c.Inspect(ctx, name)
	if err != nil {
		return false, false
	}
	return true, st.Running
}

// StartContainer starts an existing stopped container.
func (c *Client) StartContainer(ctx context.Context, name string) error {
	_, err := c.run(ctx, "start", name)
	return err
}

// RemoveVolume removes a named volume. A missing volume is not an error.
func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	_, err := c.run(ctx, "volume", "rm", "-f", name)
	return err
}

// LineWriter turns writes into complete lines passed to fn.
type LineWriter struct {
	fn  func(string)
	buf []byte
	mu  sync.Mutex
}

func NewLineWriter(fn func(string)) *LineWriter { return &LineWriter{fn: fn} }

func (w *LineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(w.buf[:i]), "\r")
		w.buf = w.buf[i+1:]
		w.fn(line)
	}
	return len(p), nil
}

// Flush emits any trailing partial line.
func (w *LineWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.fn(strings.TrimRight(string(w.buf), "\r"))
		w.buf = nil
	}
}

// SplitCommand splits a command line into arguments, honoring single and
// double quotes. It does not do any shell expansion.
func SplitCommand(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	var quote rune
	inArg := false
	r := bufio.NewReader(strings.NewReader(s))
	for {
		ch, _, err := r.ReadRune()
		if err != nil {
			break
		}
		switch {
		case quote != 0:
			if ch == quote {
				quote = 0
			} else if ch == '\\' && quote == '"' {
				next, _, err := r.ReadRune()
				if err != nil {
					return nil, errors.New("trailing backslash")
				}
				cur.WriteRune(next)
			} else {
				cur.WriteRune(ch)
			}
		case ch == '\'' || ch == '"':
			quote = ch
			inArg = true
		case ch == ' ' || ch == '\t':
			if inArg {
				out = append(out, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(ch)
			inArg = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote")
	}
	if inArg {
		out = append(out, cur.String())
	}
	return out, nil
}
