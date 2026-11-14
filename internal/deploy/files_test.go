package deploy

import (
	"appdeploy/internal/config"
	"appdeploy/internal/docker"
	"appdeploy/internal/group"
	"appdeploy/internal/store"
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestEnvFileRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "app.env")
	if m, err := ReadEnvFile(p); err != nil || len(m) != 0 {
		t.Fatalf("missing file: %v %v", m, err)
	}
	in := map[string]string{"B": "2", "A": "has = sign", "EMPTY": ""}
	if err := WriteEnvFile(p, in); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("secrets file mode = %v, want 0600", st.Mode().Perm())
	}
	out, err := ReadEnvFile(p)
	if err != nil || len(out) != 3 || out["A"] != "has = sign" || out["EMPTY"] != "" {
		t.Fatalf("round trip: %v %v", out, err)
	}
	b, _ := os.ReadFile(p)
	if !strings.HasPrefix(string(b), "A=") {
		t.Errorf("file is not sorted: %q", b)
	}
}

func TestSetSecretsValidates(t *testing.T) {
	e := &Engine{opt: Options{DataDir: t.TempDir()}}
	if err := e.SetSecrets("a", map[string]string{"GOOD_1": "x"}, nil); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []map[string]string{{"bad-name": "x"}, {"1ABC": "x"}, {"OK": "multi\nline"}, {"": "x"}} {
		if err := e.SetSecrets("a", bad, nil); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	if err := e.SetSecrets("a", map[string]string{"OTHER": "y"}, []string{"GOOD_1"}); err != nil {
		t.Fatal(err)
	}
	names, _ := e.SecretNames("a")
	if len(names) != 1 || names[0] != "OTHER" {
		t.Errorf("names = %v", names)
	}
}

func TestInsideDir(t *testing.T) {
	base := "/work/build"
	for _, ok := range []string{".", "sub", "a/b/../c", "Dockerfile"} {
		if _, err := insideDir(base, ok); err != nil {
			t.Errorf("%q should be allowed: %v", ok, err)
		}
	}
	for _, bad := range []string{"..", "../x", "a/../../x", "/../etc"} {
		if _, err := insideDir(base, bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestGitResolveShowAndCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(dir, "appdeploy.yml"), []byte("branch: main\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644)
	run("add", "-A")
	run("commit", "-q", "-m", "one")

	repo := filepath.Join(dir, ".git")
	sha, err := gitResolve(repo, "refs/heads/main")
	if err != nil || len(sha) != 40 {
		t.Fatalf("resolve: %q %v", sha, err)
	}
	if short, err := gitResolve(repo, sha[:10]); err != nil || short != sha {
		t.Fatalf("abbreviated: %q %v", short, err)
	}
	for _, bad := range []string{"--upload-pack=x", "-h", "nope", "a b", ""} {
		if _, err := gitResolve(repo, bad); err == nil {
			t.Errorf("ref %q should be refused", bad)
		}
	}
	if b, _ := gitShow(repo, sha, "appdeploy.yml"); string(b) != "branch: main\n" {
		t.Errorf("gitShow = %q", b)
	}
	if b, _ := gitShow(repo, sha, "missing.yml"); b != nil {
		t.Errorf("gitShow of a missing file = %q", b)
	}
	out := filepath.Join(t.TempDir(), "co")
	if err := gitCheckout(repo, sha, out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "Dockerfile")); err != nil {
		t.Errorf("checkout is missing the Dockerfile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, ".git")); err == nil {
		t.Errorf("checkout must not contain .git")
	}
}

func TestHuman(t *testing.T) {
	for in, want := range map[int64]string{512: "512B", 2048: "2.0KiB", 512 << 20: "512.0MiB", 3 << 30: "3.0GiB"} {
		if got := human(in); got != want {
			t.Errorf("human(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestNewIDIsSortable(t *testing.T) {
	a, b := newID(), newID()
	if a == b || len(a) != len(b) {
		t.Fatalf("ids: %q %q", a, b)
	}
}

func TestWatchdogDecisions(t *testing.T) {
	if got := parsePercent("5%"); got != 0.05 {
		t.Errorf("5%% = %v", got)
	}
	if got := parsePercent("0.1"); got != 0.1 {
		t.Errorf("0.1 = %v", got)
	}
	if got := parsePercent("lots"); got != 0.02 {
		t.Errorf("garbage = %v, want the 2%% default", got)
	}
	// Twice the baseline wins over a lower configured rate, and not over a higher one.
	if got := effectiveLimit(0.02, 0.05); got != 0.10 {
		t.Errorf("limit = %v", got)
	}
	if got := effectiveLimit(0.02, 0.005); got != 0.02 {
		t.Errorf("limit = %v", got)
	}
	cases := []struct {
		total, errs, min int
		limit            float64
		want             bool
	}{
		{100, 3, 20, 0.02, true},
		{100, 2, 20, 0.02, false}, // exactly at the limit is fine
		{10, 10, 20, 0.02, false}, // too few requests to judge
		{0, 0, 0, 0.02, false},
		{20, 20, 20, 0.5, true},
	}
	for _, c := range cases {
		if got := errorRateExceeds(c.total, c.errs, c.min, c.limit); got != c.want {
			t.Errorf("errorRateExceeds(%d,%d,%d,%v) = %v, want %v", c.total, c.errs, c.min, c.limit, got, c.want)
		}
	}
}

func TestPlanWaves(t *testing.T) {
	cases := map[int][][]int{
		0: nil,
		1: {{0}},
		2: {{0}, {1}},
		3: {{0}, {1}, {2}},
		4: {{0}, {1}, {2}, {3}},
		8: {{0}, {1, 2}, {3, 4}, {5, 6}, {7}},
	}
	for n, want := range cases {
		if got := planWaves(n); !reflect.DeepEqual(got, want) {
			t.Errorf("planWaves(%d) = %v, want %v", n, got, want)
		}
	}
}

func TestTarRoundTripAndHostileArchives(t *testing.T) {
	src := t.TempDir()
	_ = os.MkdirAll(filepath.Join(src, "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(src, "Dockerfile"), []byte("FROM scratch\n"), 0o644)
	_ = os.WriteFile(filepath.Join(src, "sub", "run.sh"), []byte("#!/bin/sh\n"), 0o755)
	_ = os.Symlink("sub/run.sh", filepath.Join(src, "link"))

	var buf bytes.Buffer
	if err := tarDir(&buf, src); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "out")
	if err := untarTo(bytes.NewReader(buf.Bytes()), dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "Dockerfile")); string(b) != "FROM scratch\n" {
		t.Errorf("Dockerfile = %q", b)
	}
	if st, err := os.Stat(filepath.Join(dst, "sub", "run.sh")); err != nil || st.Mode().Perm()&0o100 == 0 {
		t.Errorf("the executable bit was lost: %v %v", st, err)
	}
	if l, err := os.Readlink(filepath.Join(dst, "link")); err != nil || l != "sub/run.sh" {
		t.Errorf("symlink = %q %v", l, err)
	}

	evil := func(build func(tw *tar.Writer)) error {
		var b bytes.Buffer
		zw := gzip.NewWriter(&b)
		tw := tar.NewWriter(zw)
		build(tw)
		tw.Close()
		zw.Close()
		return untarTo(&b, filepath.Join(t.TempDir(), "x"))
	}
	for name, f := range map[string]func(*tar.Writer){
		"path traversal": func(tw *tar.Writer) {
			tw.WriteHeader(&tar.Header{Name: "../escape", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
			tw.Write([]byte("x"))
		},
		"absolute link": func(tw *tar.Writer) {
			tw.WriteHeader(&tar.Header{Name: "l", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink})
		},
		"link out": func(tw *tar.Writer) {
			tw.WriteHeader(&tar.Header{Name: "l", Linkname: "../../x", Typeflag: tar.TypeSymlink})
		},
		"device": func(tw *tar.Writer) { tw.WriteHeader(&tar.Header{Name: "d", Typeflag: tar.TypeChar}) },
	} {
		if err := evil(f); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestAlerterReportsOneIncidentOnce(t *testing.T) {
	a := newAlerter()
	t0 := time.Unix(1_000_000, 0)
	up := docker.State{Running: true}
	if m := a.container("c", up, t0); m != "" {
		t.Errorf("a healthy container alerted: %q", m)
	}
	// A crash that Docker already healed shows as a higher restart count.
	if m := a.container("c", docker.State{Running: true, RestartCount: 1}, t0.Add(time.Minute)); !strings.Contains(m, "crashed and was restarted") {
		t.Errorf("restart: %q", m)
	}
	// Further crashes within the cooldown stay quiet; after it they are reported again.
	if m := a.container("c", docker.State{Running: true, RestartCount: 2}, t0.Add(2*time.Minute)); m != "" {
		t.Errorf("repeated alert within the cooldown: %q", m)
	}
	if m := a.container("c", docker.State{Running: true, RestartCount: 3}, t0.Add(time.Minute+alertCooldown+time.Second)); m == "" {
		t.Errorf("no alert after the cooldown")
	}
	// A stopped container is reported once, and again only after it ran in between.
	a = newAlerter()
	down := docker.State{ExitCode: 137, OOMKilled: true}
	if m := a.container("d", down, t0); !strings.Contains(m, "not running (exit code 137)") || !strings.Contains(m, "memory limit") {
		t.Errorf("down: %q", m)
	}
	if m := a.container("d", down, t0.Add(time.Hour)); m != "" {
		t.Errorf("the same outage was reported twice: %q", m)
	}
	a.container("d", up, t0.Add(2*time.Hour))
	if m := a.container("d", down, t0.Add(3*time.Hour)); m == "" {
		t.Errorf("a new outage was not reported")
	}
	// A crash loop reports as such.
	if m := newAlerter().container("e", docker.State{Restarting: true, RestartCount: 4}, t0); !strings.Contains(m, "restarting repeatedly") {
		t.Errorf("crash loop: %q", m)
	}
	// Forgotten containers start fresh.
	a.forget(map[string]bool{})
	if len(a.restarts)+len(a.down)+len(a.last) != 0 {
		t.Errorf("state was kept for a removed container")
	}
}

func TestAlerterDiskHasHysteresis(t *testing.T) {
	a := newAlerter()
	const gb = 1 << 30
	if m := a.disk(50*gb, 100*gb, 5*gb, 0.10); m != "" {
		t.Errorf("plenty of space: %q", m)
	}
	if m := a.disk(4*gb, 100*gb, 5*gb, 0.10); !strings.Contains(m, "free of") {
		t.Errorf("low space: %q", m)
	}
	if m := a.disk(3*gb, 100*gb, 5*gb, 0.10); m != "" {
		t.Errorf("reported twice: %q", m)
	}
	// Barely above the line is not recovered; clearly above is.
	a.disk(5*gb+1, 100*gb, 5*gb, 0.10)
	if m := a.disk(4*gb, 100*gb, 5*gb, 0.10); m != "" {
		t.Errorf("flapping around the threshold reported again: %q", m)
	}
	a.disk(40*gb, 100*gb, 5*gb, 0.10)
	if m := a.disk(2*gb, 100*gb, 5*gb, 0.10); m == "" {
		t.Errorf("a new low-disk incident was not reported after recovery")
	}
}

// fakeRemote answers Ping only; the rollout guard needs nothing else.
type fakeRemote struct {
	protocol map[string]int
	down     map[string]bool
}

func (f fakeRemote) Ping(_ context.Context, s group.Server) (int, error) {
	if f.down[s.Name] {
		return 0, errors.New("connection refused")
	}
	return f.protocol[s.Name], nil
}
func (fakeRemote) SendImage(context.Context, group.Server, string, int, io.Reader) error  { return nil }
func (fakeRemote) SendSource(context.Context, group.Server, string, int, io.Reader) error { return nil }
func (fakeRemote) FetchImage(context.Context, group.Server, string, int) (io.ReadCloser, error) {
	return nil, errors.New("not used")
}
func (fakeRemote) Announce(context.Context, group.Server, store.Final) error   { return nil }
func (fakeRemote) Finals(context.Context, group.Server) ([]store.Final, error) { return nil, nil }
func (fakeRemote) FetchRelease(context.Context, group.Server, string, int) (PeerPayload, error) {
	return PeerPayload{}, errors.New("not used")
}
func (fakeRemote) StartDeploy(context.Context, group.Server, PeerPayload) (string, error) {
	return "", errors.New("not used")
}
func (fakeRemote) StreamDeploy(context.Context, group.Server, string, func(string)) (string, string, error) {
	return "", "", errors.New("not used")
}

func TestRolloutRefusesServersOnAnotherProtocol(t *testing.T) {
	g, err := group.Init(t.TempDir(), "g", "a", "127.0.0.1:1", []string{"web"})
	if err != nil {
		t.Fatal(err)
	}
	b := group.Server{Name: "b", Addr: "127.0.0.1:2", Labels: []string{"web"}}
	self := group.Server{Name: "a", Addr: "127.0.0.1:1", Labels: []string{"web"}}
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	run := func(remote fakeRemote, allowPartial bool) ([]group.Server, error) {
		e := &Engine{ctx: context.Background(), st: st, opt: Options{Group: g, Remote: remote}}
		r := &run{e: e, cfg: &config.App{AllowPartial: allowPartial}}
		return r.reachable([]group.Server{self, b})
	}
	same := fakeRemote{protocol: map[string]int{"b": group.Protocol}}
	if got, err := run(same, false); err != nil || len(got) != 2 {
		t.Fatalf("matching protocol: %v %v", got, err)
	}
	other := fakeRemote{protocol: map[string]int{"b": group.Protocol + 1}}
	for _, partial := range []bool{false, true} {
		// Even allow_partial does not skip a server that is reachable but incompatible.
		if _, err := run(other, partial); err == nil || !strings.Contains(err.Error(), "being updated") {
			t.Errorf("a different protocol (partial=%v): %v", partial, err)
		}
	}
	down := fakeRemote{down: map[string]bool{"b": true}}
	if _, err := run(down, false); err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Errorf("an unreachable server: %v", err)
	}
	if got, err := run(down, true); err != nil || len(got) != 1 || got[0].Name != "a" {
		t.Errorf("allow_partial: %v %v", got, err)
	}
}
