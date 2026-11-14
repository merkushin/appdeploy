package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
