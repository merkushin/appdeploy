package config

import (
	"strings"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	c, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	web := c.WebRole()
	if web == nil {
		t.Fatal("no web role by default")
	}
	if web.Memory.Bytes() != 512<<20 || web.CPUs != 1 || web.ReplicasPerSrv != 1 {
		t.Errorf("bad role defaults: %+v", web)
	}
	if web.Healthcheck.Path != "/up" || web.Healthcheck.StartPeriod.D() != 60*time.Second {
		t.Errorf("bad healthcheck defaults: %+v", web.Healthcheck)
	}
	if c.Branch != "main" || c.KeepReleases != 5 || c.Build.Dockerfile != "Dockerfile" {
		t.Errorf("bad app defaults: %+v", c)
	}
}

func TestParseFull(t *testing.T) {
	c, err := Parse([]byte(`
app: shop
branch: trunk
domains: [shop.example.com]
roles:
  web:
    port: 3000
    memory: 1G
    healthcheck: { path: /healthz, timeout: 2s, start_period: 20s }
    smoke: [/, /about]
  worker:
    cmd: /app/server worker
    memory: 128M
release_command: /app/server migrate up
keep_releases: 3
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Branch != "trunk" || c.KeepReleases != 3 || c.ReleaseCommand == "" {
		t.Errorf("unexpected: %+v", c)
	}
	if c.Roles["web"].Memory.Bytes() != 1<<30 || c.Roles["web"].Port != 3000 {
		t.Errorf("web: %+v", c.Roles["web"])
	}
	w := c.Roles["worker"]
	if w.Cmd != "/app/server worker" || w.Memory.Bytes() != 128<<20 || w.Healthcheck.Path != "" {
		t.Errorf("worker: %+v", w)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"unknown key":       "bogus: 1",
		"bad duration":      "roles:\n  web:\n    stop_timeout: soon",
		"bad size":          "roles:\n  web:\n    memory: lots",
		"bad role name":     "roles:\n  Web!: {}",
		"port out of range": "roles:\n  web:\n    port: 99999",
		"smoke not a path":  "roles:\n  web:\n    smoke: [about]",
		"bad app name":      "app: Shop",
		"memory too small":  "roles:\n  web:\n    memory: 1K",
		"keep_releases < 0": "keep_releases: -1",
	}
	for name, src := range cases {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: expected an error", name)
		} else if !strings.Contains(err.Error(), FileName) {
			t.Errorf("%s: error should name the file: %v", name, err)
		}
	}
}

func TestJSONRoundTrip(t *testing.T) {
	c, err := Parse([]byte("roles:\n  web:\n    memory: 256M\n    stop_timeout: 7s\n"))
	if err != nil {
		t.Fatal(err)
	}
	back, err := FromJSON(c.JSON())
	if err != nil {
		t.Fatal(err)
	}
	if back.Roles["web"].Memory.Bytes() != 256<<20 || back.Roles["web"].StopTimeout.D() != 7*time.Second {
		t.Errorf("round trip lost data: %+v", back.Roles["web"])
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"512M": 512 << 20, "1G": 1 << 30, "64k": 64 << 10, "100": 100, "2 GB": 2 << 30} {
		got, err := ParseSize(in)
		if err != nil || got.Bytes() != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "M", "1.5G", "-1M", "ten"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) should fail", in)
		}
	}
}

func TestAllMistakesAreReportedAtOnce(t *testing.T) {
	_, err := Parse([]byte("roles:\n  web:\n    memroy: 1G\n    memory: lots\n    stop_timeout: soon\nbogus: 1\n"))
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{"memroy", "invalid size", "invalid duration", "bogus"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q:\n%v", want, err)
		}
	}
}

func TestEnvIsValidated(t *testing.T) {
	good, err := Parse([]byte("env:\n  BASE_URL: https://x.example\n  INVITE_ONLY: 1\n  RATIO: 0.5\n  FLAG: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if good.Env["INVITE_ONLY"] != "1" || good.Env["FLAG"] != "true" || good.Env["RATIO"] != "0.5" {
		t.Errorf("scalars must arrive as written: %v", good.Env)
	}
	for name, src := range map[string]string{
		"bad name":        "env:\n  1BAD: x\n",
		"dash in name":    "env:\n  A-B: x\n",
		"PORT":            "env:\n  PORT: 80\n",
		"APPDEPLOY_ name": "env:\n  APPDEPLOY_RELEASE: 9\n",
		"multi-line":      "env:\n  A: |\n    one\n    two\n",
	} {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	back, err := FromJSON(good.JSON())
	if err != nil || back.Env["BASE_URL"] != "https://x.example" {
		t.Errorf("env does not survive the release's JSON: %v %v", back, err)
	}
}

func TestVolumesAreNamedAndSafe(t *testing.T) {
	ok, err := Parse([]byte("roles:\n  web:\n    volumes: [\"data:/data\", \"cache:/var/cache:ro\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ok.Roles["web"].Volumes) != 2 {
		t.Errorf("volumes: %v", ok.Roles["web"].Volumes)
	}
	for name, vol := range map[string]string{
		"a host path": "/srv/data:/data", "a relative path": "./data:/data", "no mount path": "data",
		"a relative mount": "data:data", "mount at root": "data:/", "unclean path": "data:/a/../b",
		"proc": "data:/proc/x", "uppercase name": "Data:/data", "unknown option": "data:/data:rw", "too many parts": "a:/b:ro:c",
	} {
		if _, err := Parse([]byte("roles:\n  web:\n    volumes: [\"" + vol + "\"]\n")); err == nil {
			t.Errorf("%s (%s) was accepted", name, vol)
		}
	}
	if _, err := Parse([]byte("roles:\n  web:\n    volumes: [\"a:/data\", \"b:/data\"]\n")); err == nil {
		t.Errorf("two volumes at one path were accepted")
	}
}

func TestWarnings(t *testing.T) {
	c, err := Parse([]byte("env:\n  SMTP_PASSWORD: x\n  API_KEY: y\n  SESSION_KEY: z\n  BASE_URL: u\n  KEYBOARD: k\nroles:\n  web:\n    replicas_per_server: 2\n    volumes: [\"d:/d\"]\n  ro:\n    replicas_per_server: 2\n    volumes: [\"d:/d:ro\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	w := strings.Join(c.Warnings(), "\n")
	for _, want := range []string{"env.SMTP_PASSWORD", "env.API_KEY", "env.SESSION_KEY", "roles.web: 2 replicas"} {
		if !strings.Contains(w, want) {
			t.Errorf("no warning containing %q:\n%s", want, w)
		}
	}
	for _, not := range []string{"BASE_URL", "KEYBOARD", "roles.ro"} {
		if strings.Contains(w, not) {
			t.Errorf("an unwarranted warning about %q:\n%s", not, w)
		}
	}
}
