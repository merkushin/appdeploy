package migrate

import (
	"os"
	"strings"
	"testing"

	"appdeploy/internal/config"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func has(items []Item, key string) bool {
	for _, i := range items {
		if strings.HasPrefix(i.Key, key) {
			return true
		}
	}
	return false
}

func mustParse(t *testing.T, r *Result) *config.App {
	t.Helper()
	cfg, err := config.Parse([]byte(r.Config))
	if err != nil {
		t.Fatalf("the generated appdeploy.yml is invalid: %v\n%s", err, r.Config)
	}
	return cfg
}

func TestAnAppWithAVolumeIsConverted(t *testing.T) {
	r, err := FromKamal(fixture(t, "sqlite-app.yml"), fixture(t, "sqlite-app.secrets"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Verdict != "ready-with-notes" || len(r.Blockers) != 0 {
		t.Fatalf("verdict=%s blockers=%+v", r.Verdict, r.Blockers)
	}
	if !has(r.Notes, "volumes") || !has(r.Notes, "env.clear") {
		t.Errorf("notes: %+v", r.Notes)
	}
	cfg := mustParse(t, r)
	web := cfg.Roles["web"]
	if len(web.Volumes) != 1 || web.Volumes[0] != "notes_data:/data" {
		t.Errorf("volumes: %v", web.Volumes)
	}
	if cfg.Env["DATABASE_PATH"] != "/data/notes.db" || cfg.Env["INVITE_ONLY"] != "1" || cfg.Env["MAIL_FROM"] != "Notes <no-reply@notes.example.com>" {
		t.Errorf("env: %v", cfg.Env)
	}
	if r.App != "notes-app" || web.Port != 8080 || web.Healthcheck.Path != "/api/health" || web.Healthcheck.Timeout.D().Seconds() != 5 {
		t.Errorf("app=%s web=%+v", r.App, web)
	}
	if len(r.Domains) != 1 || r.Domains[0] != "notes.example.com" || len(cfg.Domains) != 1 {
		t.Errorf("domains: %v / %v", r.Domains, cfg.Domains)
	}
	// Secrets: names only, and the registry's credential is not the app's.
	if strings.Join(r.SecretNames, ",") != "ADMIN_EMAIL,ALLOWED_EMAILS,SMTP_PASSWORD,SMTP_USER" {
		t.Errorf("secret names: %v", r.SecretNames)
	}
	all := strings.Join(r.Commands, "\n")
	for _, want := range []string{
		"appdeploy --host deploy@203.0.113.10 apps create notes-app --domain notes.example.com",
		`ADMIN_EMAIL="$ADMIN_EMAIL"`, `SMTP_PASSWORD="$SMTP_PASSWORD"`,
		"--no-deploy", "git remote add prod deploy@203.0.113.10:notes-app",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("the commands lack %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "KAMAL_REGISTRY_PASSWORD") {
		t.Errorf("the registry credential leaked into the commands")
	}
	// SENTRY_DSN is in the secrets file but not under env.secret: the app does not get it.
	if !has(r.Notes, ".kamal/secrets") {
		t.Errorf("no note about SENTRY_DSN: %+v", r.Notes)
	}
	if !has(r.Notes, "registry") || !has(r.Notes, "env.clear") {
		t.Errorf("missing notes: %+v", r.Notes)
	}
}

func TestRolesOptionsAndAccessories(t *testing.T) {
	deploy := `
service: shop
servers:
  web:
    hosts: [10.0.0.1, 10.0.0.2]
    options:
      memory: 256m
      cpus: 2
  jobs:
    hosts: [10.0.0.3]
    cmd: "bin/jobs --queue default"
    options:
      restart: always
proxy:
  hosts: [shop.example.com, www.shop.example.com]
  ssl: true
  response_timeout: 30
env:
  FOO: bar
  secret:
    - DB_PASSWORD:MAIN_DB_PASSWORD
    - API_KEY
  tags:
    eu: {X: 1}
accessories:
  db:
    image: postgres:16
    host: 10.0.0.1
    directories: [data:/var/lib/postgresql/data]
  cache:
    image: redis:7
    host: 10.0.0.1
  search:
    image: getmeili/meilisearch
    host: 10.0.0.1
retain_containers: 3
boot:
  limit: 1
hooks_path: .kamal/hooks
deploy_timeout: 45
drain_timeout: 20
`
	r, err := FromKamal([]byte(deploy), nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := mustParse(t, r)
	if cfg.Roles["web"].Memory.Bytes() != 256<<20 || cfg.Roles["web"].CPUs != 2 {
		t.Errorf("web limits: %+v", cfg.Roles["web"])
	}
	if cfg.Roles["jobs"].Cmd != "bin/jobs --queue default" {
		t.Errorf("jobs cmd: %q", cfg.Roles["jobs"].Cmd)
	}
	// The roles live on different servers, so they need labels.
	if strings.Join(cfg.Roles["jobs"].On, ",") != "jobs" || strings.Join(cfg.Roles["web"].On, ",") != "web" {
		t.Errorf("labels: web=%v jobs=%v", cfg.Roles["web"].On, cfg.Roles["jobs"].On)
	}
	if !cfg.Services.Postgres || !cfg.Services.Redis || cfg.KeepReleases != 3 {
		t.Errorf("services/keep: %+v keep=%d", cfg.Services, cfg.KeepReleases)
	}
	if cfg.Roles["web"].Healthcheck.StartPeriod.D().Seconds() != 45 || cfg.Roles["web"].StopTimeout.D().Seconds() != 20 {
		t.Errorf("timeouts: %+v / %v", cfg.Roles["web"].Healthcheck, cfg.Roles["web"].StopTimeout)
	}
	if len(cfg.Domains) != 2 {
		t.Errorf("domains: %v", cfg.Domains)
	}
	// Meilisearch cannot be run by appdeploy; the postgres directories are called out.
	if r.Verdict != "blocked" || !has(r.Blockers, "accessories.search") || has(r.Blockers, "accessories.db") || has(r.Blockers, "accessories.cache") {
		t.Errorf("blockers: %+v", r.Blockers)
	}
	for _, key := range []string{"servers.jobs.options.restart", "env.tags", "proxy.response_timeout", "boot"} {
		if !has(r.Unsupported, key) {
			t.Errorf("%s should be listed as unsupported: %+v", key, r.Unsupported)
		}
	}
	if !has(r.Notes, "hooks_path") || !has(r.Notes, "env.secret") || !has(r.Notes, "servers") {
		t.Errorf("notes: %+v", r.Notes)
	}
	if r.Env["FOO"] != "bar" {
		t.Errorf("a plain key under env is a clear variable: %v", r.Env)
	}
	if strings.Join(r.SecretNames, ",") != "API_KEY,DB_PASSWORD" {
		t.Errorf("the alias should be set under the name the app reads: %v", r.SecretNames)
	}
}

func TestTheSimplestAppsAreReady(t *testing.T) {
	for name, deploy := range map[string]string{
		"kamal 1 style": "service: blog\nservers:\n  - 10.0.0.9\nhealthcheck:\n  path: /health\n  port: 3000\n",
		"kamal 2 style": "service: blog\nservers:\n  web:\n    - 10.0.0.9\nproxy:\n  host: blog.example.com\n  ssl: true\n",
	} {
		r, err := FromKamal([]byte(deploy), nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		mustParse(t, r)
		if r.Verdict == "blocked" || len(r.Blockers) > 0 {
			t.Errorf("%s: blocked: %+v", name, r.Blockers)
		}
	}
	r, _ := FromKamal([]byte("service: blog\nservers: [10.0.0.9]\nhealthcheck:\n  path: /health\n  port: 3000\n"), nil)
	web := mustParse(t, r).Roles["web"]
	if web.Port != 3000 || web.Healthcheck.Path != "/health" {
		t.Errorf("legacy healthcheck: %+v", web)
	}
}

func TestNamesAndBadInput(t *testing.T) {
	for in, want := range map[string]string{"My_App.v2": "my-app-v2", "2fast": "app-2fast", "ok": "ok"} {
		if got, _ := appName(in); got != want {
			t.Errorf("appName(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := FromKamal([]byte(""), nil); err == nil {
		t.Errorf("an empty file was accepted")
	}
	if _, err := FromKamal([]byte("servers: [1.2.3.4]\n"), nil); err == nil || !strings.Contains(err.Error(), "service") {
		t.Errorf("no service: %v", err)
	}
	if _, err := FromKamal([]byte("service: [unclosed"), nil); err == nil {
		t.Errorf("invalid YAML was accepted")
	}
	r, _ := FromKamal([]byte("service: x\nsomething_new: 1\nservers: [1.2.3.4]\n"), nil)
	if !has(r.Unsupported, "something_new") {
		t.Errorf("an unknown key should be reported: %+v", r.Unsupported)
	}
}

func TestAHostDirectoryMountIsStillABlocker(t *testing.T) {
	deploy := "service: app\nimage: u/app\nservers:\n  web: [203.0.113.10]\nproxy:\n  host: a.example.com\n  app_port: 8080\nvolumes:\n  - /srv/app-data:/data\n"
	r, err := FromKamal([]byte(deploy), nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Verdict != "blocked" || !has(r.Blockers, "volumes") {
		t.Fatalf("verdict=%s blockers=%+v", r.Verdict, r.Blockers)
	}
}
