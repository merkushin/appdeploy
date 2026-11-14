package export

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"appdeploy/internal/config"
	"appdeploy/internal/group"

	"gopkg.in/yaml.v3"
)

func spec(t *testing.T) Spec {
	t.Helper()
	cfg, err := config.Parse([]byte(`
roles:
  web:
    port: 3000
    memory: 1G
    cpus: 2
    replicas_per_server: 2
    healthcheck: {path: /healthz, timeout: 3s, start_period: 20s}
  worker:
    on: [worker]
    cmd: "worker --queue \"high priority\""
    memory: 128M
services:
  postgres: false
`))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Services.Postgres = true // the config parser accepts it; set directly for clarity
	return Spec{
		App: "shop", Release: 7, Config: cfg, Domains: []string{"shop.example.com", "www.shop.example.com"},
		SecretNames: []string{"STRIPE_KEY", "SESSION_SECRET"},
		Servers: []group.Server{
			{Name: "a", Addr: "10.0.0.1:7070", Labels: []string{"web"}},
			{Name: "b", Addr: "10.0.0.2:7070", Labels: []string{"web"}},
			{Name: "c", Addr: "10.0.0.3:7070", Labels: []string{"worker"}},
		},
	}
}

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("not valid YAML: %v\n%s", err, s)
	}
	return m
}

func get(t *testing.T, m any, path ...string) any {
	t.Helper()
	cur := m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("%v is not a map at %q", cur, p)
		}
		cur = mm[p]
	}
	return cur
}

func TestNoSecretValuesAndUnknownFormat(t *testing.T) {
	s := spec(t)
	if _, err := Render("terraform", s); err == nil {
		t.Errorf("unknown format accepted")
	}
	if _, err := Render("kamal", Spec{App: "x"}); err == nil {
		t.Errorf("an app without a release was exported")
	}
	for _, f := range Formats {
		files, err := Render(f, s)
		if err != nil {
			t.Fatal(err)
		}
		all := ""
		for _, c := range files {
			all += c
		}
		for _, n := range []string{"STRIPE_KEY", "SESSION_SECRET", "DATABASE_URL"} {
			if !strings.Contains(all, n) {
				t.Errorf("%s: the secret name %s is missing", f, n)
			}
		}
	}
}

func TestKamal(t *testing.T) {
	files, _ := Render("kamal", spec(t))
	m := decode(t, files["config/deploy.yml"])
	if get(t, m, "service") != "shop" {
		t.Errorf("service = %v", get(t, m, "service"))
	}
	web := get(t, m, "servers", "web", "hosts").([]any)
	if len(web) != 2 || web[0] != "10.0.0.1" || web[1] != "10.0.0.2" {
		t.Errorf("web hosts = %v", web)
	}
	if get(t, m, "servers", "worker", "hosts").([]any)[0] != "10.0.0.3" || get(t, m, "servers", "worker", "cmd") != `worker --queue "high priority"` {
		t.Errorf("worker = %v", get(t, m, "servers", "worker"))
	}
	if get(t, m, "proxy", "app_port") != 3000 || get(t, m, "proxy", "healthcheck", "path") != "/healthz" || get(t, m, "proxy", "ssl") != true {
		t.Errorf("proxy = %v", get(t, m, "proxy"))
	}
	if !strings.Contains(files[".kamal/secrets"], "STRIPE_KEY=$STRIPE_KEY") {
		t.Errorf(".kamal/secrets = %q", files[".kamal/secrets"])
	}
	if get(t, m, "accessories", "postgres", "image") != "postgres:17-alpine" {
		t.Errorf("accessories = %v", get(t, m, "accessories"))
	}
}

func TestK8s(t *testing.T) {
	files, _ := Render("k8s", spec(t))
	kinds := map[string]int{}
	var web map[string]any
	for _, doc := range strings.Split(files["k8s.yml"], "\n---\n") {
		m := decode(t, doc)
		if m == nil {
			continue
		}
		k := get(t, m, "kind").(string)
		kinds[k]++
		if k == "Deployment" && get(t, m, "metadata", "name") == "shop-web" {
			web = m
		}
	}
	if kinds["Deployment"] != 2 || kinds["Service"] != 1 || kinds["Ingress"] != 1 || kinds["Secret"] != 1 {
		t.Errorf("kinds = %v", kinds)
	}
	if web == nil || get(t, web, "spec", "replicas") != 4 { // 2 per server on 2 servers
		t.Errorf("web deployment: %v", web)
	}
	c := get(t, web, "spec", "template", "spec", "containers").([]any)[0]
	if get(t, c, "resources", "limits", "memory") != "1Gi" || get(t, c, "readinessProbe", "httpGet", "path") != "/healthz" {
		t.Errorf("container = %v", c)
	}
}

func TestFly(t *testing.T) {
	files, _ := Render("fly", spec(t))
	toml := files["fly.toml"]
	for _, want := range []string{`app = "shop"`, `internal_port = 3000`, `path = "/healthz"`, `[processes]`, `worker = "worker --queue \"high priority\""`, `processes = ["web"]`, `memory = "1gb"`, `fly certs add shop.example.com`} {
		if !strings.Contains(toml, want) {
			t.Errorf("fly.toml is missing %q:\n%s", want, toml)
		}
	}
	if !strings.Contains(files["secrets.sh"], `STRIPE_KEY="${STRIPE_KEY:?set STRIPE_KEY}"`) {
		t.Errorf("secrets.sh = %s", files["secrets.sh"])
	}
}

// Compose output is validated by Compose itself when it is installed.
func TestComposeIsAcceptedByDockerCompose(t *testing.T) {
	files, err := Render("compose", spec(t))
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, files["compose.yml"])
	for _, svc := range []string{"web", "worker", "postgres", "caddy"} {
		if get(t, m, "services", svc) == nil {
			t.Errorf("service %s is missing", svc)
		}
	}
	if !strings.Contains(files["Caddyfile"], "shop.example.com, www.shop.example.com {\n\treverse_proxy web:3000") {
		t.Errorf("Caddyfile = %q", files["Caddyfile"])
	}
	if exec.Command("docker", "compose", "version").Run() != nil {
		t.Skip("docker compose is not installed")
	}
	dir := t.TempDir()
	for name, content := range files {
		_ = os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
	}
	_ = os.WriteFile(filepath.Join(dir, ".env"), []byte("STRIPE_KEY=x\nSESSION_SECRET=y\nDATABASE_URL=z\nPOSTGRES_PASSWORD=p\n"), 0o600)
	cmd := exec.Command("docker", "compose", "-f", "compose.yml", "config", "--quiet")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("docker compose rejected the file: %v\n%s\n%s", err, out, files["compose.yml"])
	}
}
