// Package migrate turns the configuration of other tools into an appdeploy setup,
// and says plainly what it could not carry over.
package migrate

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"appdeploy/internal/config"
)

// Item is something the importer wants the reader to know about one key.
type Item struct {
	Key     string `json:"key"`
	Message string `json:"message"`
}

// Result is what an import produces.
type Result struct {
	App         string            `json:"app"`
	Domains     []string          `json:"domains"`
	Hosts       []string          `json:"hosts"`
	Config      string            `json:"appdeploy_yml"`
	Env         map[string]string `json:"env_clear"`    // non-secret environment, to be set as secrets for now
	SecretNames []string          `json:"secret_names"` // names only
	Commands    []string          `json:"commands"`
	Blockers    []Item            `json:"blockers"`    // things that stop the app from running on appdeploy as it is
	Unsupported []Item            `json:"unsupported"` // settings with no equivalent; the app may still run
	Notes       []Item            `json:"notes"`       // things to decide or check
	Verdict     string            `json:"verdict"`     // ready, ready-with-notes or blocked
}

type importer struct {
	r    *Result
	root map[string]any
	seen map[string]bool // top-level keys handled
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func asString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int, int64, float64, bool:
		return fmt.Sprint(x)
	}
	return ""
}

func asStrings(v any) []string {
	var out []string
	for _, e := range asList(v) {
		if s := asString(e); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	k := make([]string, 0, len(m))
	for key := range m {
		k = append(k, key)
	}
	sort.Strings(k)
	return k
}

func (im *importer) block(key, msg string) { im.r.Blockers = append(im.r.Blockers, Item{key, msg}) }
func (im *importer) unsupported(key, msg string) {
	im.r.Unsupported = append(im.r.Unsupported, Item{key, msg})
}
func (im *importer) note(key, msg string) { im.r.Notes = append(im.r.Notes, Item{key, msg}) }

var badName = regexp.MustCompile(`[^a-z0-9-]+`)

// appName makes a valid appdeploy app name out of a Kamal service name.
func appName(service string) (string, bool) {
	n := strings.Trim(badName.ReplaceAllString(strings.ToLower(service), "-"), "-")
	if n == "" || n[0] < 'a' || n[0] > 'z' {
		n = "app-" + n
	}
	if len(n) > 32 {
		n = strings.TrimRight(n[:32], "-")
	}
	return n, n != service
}

// roleSpec is a Kamal server role as appdeploy will run it.
type roleSpec struct {
	name   string
	hosts  []string
	cmd    string
	memory string
	cpus   string
}

// FromKamal reads a Kamal deploy.yml (and, optionally, .kamal/secrets) and
// builds the equivalent appdeploy setup.
func FromKamal(deploy, secrets []byte) (*Result, error) {
	var root map[string]any
	if err := yaml.Unmarshal(deploy, &root); err != nil {
		return nil, fmt.Errorf("deploy.yml: %w", err)
	}
	if root == nil {
		return nil, fmt.Errorf("deploy.yml is empty")
	}
	im := &importer{r: &Result{Env: map[string]string{}}, root: root, seen: map[string]bool{}}
	r := im.r

	service := asString(root["service"])
	if service == "" {
		return nil, fmt.Errorf("deploy.yml has no service: name")
	}
	im.seen["service"] = true
	var changed bool
	r.App, changed = appName(service)
	if changed {
		im.note("service", fmt.Sprintf("the app is called %q here: appdeploy names are lowercase letters, digits and dashes", r.App))
	}

	cfg := &configBuilder{keepReleases: 0}
	roles := im.servers(cfg)
	im.proxy(cfg, roles)
	im.build(cfg)
	im.env()
	im.volumes(cfg)
	im.accessories(cfg)
	im.misc(cfg)
	im.secretNames(secrets)
	im.finish(cfg, roles)
	return r, nil
}

// configBuilder collects what goes into appdeploy.yml.
type configBuilder struct {
	roles        []roleSpec
	port         int
	healthPath   string
	healthTimeou string
	startPeriod  string
	stopTimeout  string
	buildDF      string
	buildCtx     string
	buildArgs    map[string]string
	postgres     bool
	redis        bool
	keepReleases int
	volumes      []string // in appdeploy's form, name:/path[:ro]; mounted by every role
}

// servers reads the server roles; a plain list is the web role.
func (im *importer) servers(cfg *configBuilder) []roleSpec {
	im.seen["servers"] = true
	var out []roleSpec
	switch s := im.root["servers"].(type) {
	case []any:
		out = append(out, roleSpec{name: "web", hosts: hostList(s)})
	case map[string]any:
		for _, name := range sortedKeys(s) {
			spec := roleSpec{name: name}
			switch v := s[name].(type) {
			case []any:
				spec.hosts = hostList(v)
			case map[string]any:
				spec.hosts = hostList(asList(v["hosts"]))
				spec.cmd = asString(v["cmd"])
				if opts := asMap(v["options"]); opts != nil {
					for _, k := range sortedKeys(opts) {
						switch k {
						case "memory":
							spec.memory = asString(opts[k])
						case "cpus":
							spec.cpus = asString(opts[k])
						default:
							im.unsupported("servers."+name+".options."+k, "a docker option appdeploy does not set: containers run with limits for memory and CPU, an init process and no extra privileges")
						}
					}
				}
				if v["env"] != nil {
					im.unsupported("servers."+name+".env", "environment per role: appdeploy's environment is per app (all roles get the same variables)")
				}
				if v["labels"] != nil {
					im.unsupported("servers."+name+".labels", "docker labels on containers")
				}
				if v["proxy"] != nil && name != "web" {
					im.note("servers."+name+".proxy", "only the role named web gets traffic; rename or reorganise if this role serves HTTP")
				}
				for _, k := range sortedKeys(v) {
					if k != "hosts" && k != "cmd" && k != "options" && k != "env" && k != "labels" && k != "proxy" && k != "traefik" {
						im.unsupported("servers."+name+"."+k, "not understood by the importer")
					}
				}
			}
			out = append(out, spec)
		}
	}
	if len(out) == 0 {
		im.block("servers", "no servers found in deploy.yml")
	}
	hostSet := map[string]bool{}
	for _, role := range out {
		for _, h := range role.hosts {
			if !hostSet[h] {
				hostSet[h] = true
				im.r.Hosts = append(im.r.Hosts, h)
			}
		}
	}
	if len(im.r.Hosts) > 1 {
		im.note("servers", fmt.Sprintf("%d servers: appdeploy runs them as a group (labels web, ...; see docs/groups.md); one push builds once and rolls out in waves", len(im.r.Hosts)))
	}
	cfg.roles = out
	return out
}

func hostList(l []any) []string {
	var hosts []string
	for _, e := range l {
		switch h := e.(type) {
		case string:
			hosts = append(hosts, h)
		case map[string]any: // "host: tag" form
			for _, k := range sortedKeys(h) {
				hosts = append(hosts, k)
			}
		}
	}
	return hosts
}

func (im *importer) proxy(cfg *configBuilder, roles []roleSpec) {
	im.seen["proxy"] = true
	im.seen["healthcheck"] = true
	im.seen["traefik"] = true
	if im.root["traefik"] != nil {
		im.unsupported("traefik", "Kamal 1's Traefik settings: appdeploy uses Caddy and configures it itself")
	}
	if hc := asMap(im.root["healthcheck"]); hc != nil { // Kamal 1 style
		if p := asString(hc["path"]); p != "" {
			cfg.healthPath = p
		}
		if p := asString(hc["port"]); p != "" {
			if n, err := strconv.Atoi(p); err == nil {
				cfg.port = n
			}
		}
		if hc["max_attempts"] != nil || hc["interval"] != nil || hc["cmd"] != nil {
			im.unsupported("healthcheck", "interval, attempts and command checks: appdeploy probes the HTTP path from outside until it answers (up to start_period)")
		}
	}
	switch p := im.root["proxy"].(type) {
	case nil:
	case bool:
		if !p {
			im.note("proxy", "proxy: false means no HTTP routing: the app gets no domain from appdeploy either")
		}
	case map[string]any:
		if h := asString(p["host"]); h != "" {
			im.r.Domains = append(im.r.Domains, h)
		}
		im.r.Domains = append(im.r.Domains, asStrings(p["hosts"])...)
		if ap := asString(p["app_port"]); ap != "" {
			if n, err := strconv.Atoi(ap); err == nil {
				cfg.port = n
			}
		}
		if hc := asMap(p["healthcheck"]); hc != nil {
			if path := asString(hc["path"]); path != "" {
				cfg.healthPath = path
			}
			if t := asString(hc["timeout"]); t != "" {
				cfg.healthTimeou = secondsOrDuration(t)
			}
			if hc["interval"] != nil {
				im.note("proxy.healthcheck.interval", "appdeploy probes every second while a new container starts; there is no interval setting")
			}
		}
		switch ssl := p["ssl"].(type) {
		case bool:
			if !ssl {
				im.note("proxy.ssl", "ssl: false: with a domain appdeploy always serves HTTPS; plain HTTP is only a server-wide agent flag (--no-tls)")
			}
		case map[string]any:
			im.unsupported("proxy.ssl", "your own certificate: appdeploy gets Let's Encrypt certificates through Caddy")
		}
		for _, k := range sortedKeys(p) {
			switch k {
			case "host", "hosts", "app_port", "healthcheck", "ssl":
			case "response_timeout", "buffering", "forward_headers", "logging", "path_prefix", "strip_path_prefix", "error_pages":
				im.unsupported("proxy."+k, "a kamal-proxy setting with no equivalent: Caddy runs with its defaults")
			default:
				im.unsupported("proxy."+k, "not understood by the importer")
			}
		}
	}
	if len(im.r.Domains) == 0 {
		im.note("proxy", "no domain found: set one with `apps create --domain` or `domains set`, or the app is not reachable by name")
	}
	sort.Strings(im.r.Domains)
	if dt := asString(im.root["drain_timeout"]); dt != "" {
		cfg.stopTimeout = secondsOrDuration(dt)
	}
	if dt := asString(im.root["deploy_timeout"]); dt != "" {
		cfg.startPeriod = secondsOrDuration(dt)
	}
	im.seen["drain_timeout"], im.seen["deploy_timeout"] = true, true
}

// secondsOrDuration turns Kamal's plain seconds into a Go duration string.
func secondsOrDuration(s string) string {
	if _, err := strconv.Atoi(s); err == nil {
		return s + "s"
	}
	return s
}

func (im *importer) build(cfg *configBuilder) {
	im.seen["builder"], im.seen["registry"] = true, true
	if reg := asMap(im.root["registry"]); reg != nil {
		im.note("registry", "not needed: appdeploy builds on the server. To keep building elsewhere, run `appdeploy deploy <app> --image <image>` (the server needs `docker login` for a private registry)")
	}
	b := asMap(im.root["builder"])
	for _, k := range sortedKeys(b) {
		switch k {
		case "arch", "remote", "local", "driver", "cache":
			// The image is built on the server, for its own architecture.
		case "dockerfile":
			cfg.buildDF = asString(b[k])
		case "context":
			cfg.buildCtx = asString(b[k])
		case "args":
			cfg.buildArgs = map[string]string{}
			for ak, av := range asMap(b[k]) {
				cfg.buildArgs[ak] = asString(av)
			}
		case "secrets":
			im.unsupported("builder.secrets", "build secrets: appdeploy passes no secrets to the build (build args are not secret)")
		case "target":
			im.unsupported("builder.target", "a multi-stage build target: the Dockerfile's last stage is built")
		default:
			im.unsupported("builder."+k, "not understood by the importer")
		}
	}
}

func (im *importer) env() {
	im.seen["env"] = true
	env := asMap(im.root["env"])
	clear := map[string]any{}
	for _, k := range sortedKeys(env) {
		switch k {
		case "clear":
			for ck, cv := range asMap(env[k]) {
				clear[ck] = cv
			}
		case "secret":
			for _, name := range asStrings(env[k]) {
				n, _, aliased := strings.Cut(name, ":")
				if aliased {
					im.note("env.secret", fmt.Sprintf("%s is read from a secret under another name; set it as %s", name, n))
				}
				im.r.SecretNames = append(im.r.SecretNames, n)
			}
		case "tags":
			im.unsupported("env.tags", "environment that differs per server (tags): appdeploy's environment is the same on every server of an app")
		default: // a plain key: value under env is a clear variable
			clear[k] = env[k]
		}
	}
	for k, v := range clear {
		im.r.Env[k] = asString(v)
	}
}

func (im *importer) volumes(cfg *configBuilder) {
	im.seen["volumes"] = true
	for _, v := range asStrings(im.root["volumes"]) {
		parts := strings.Split(v, ":")
		if len(parts) < 2 || parts[0] == "" {
			im.unsupported("volumes", fmt.Sprintf("%q: not understood", v))
			continue
		}
		src, dst := parts[0], parts[1]
		if strings.HasPrefix(src, "/") || strings.HasPrefix(src, ".") || strings.HasPrefix(src, "~") {
			im.block("volumes", fmt.Sprintf("%s mounts a directory of the host: appdeploy mounts only named volumes. Copy the data into a named volume and use that.", v))
			continue
		}
		name := strings.ToLower(strings.Trim(badVolume.ReplaceAllString(strings.TrimPrefix(src, asString(im.root["service"])+"_"), "-"), "-"))
		if name == "" || name[0] < 'a' || name[0] > 'z' {
			name = "vol-" + name
		}
		entry := name + ":" + dst
		for _, opt := range parts[2:] {
			switch opt {
			case "ro":
				entry += ":ro"
			case "rw", "z", "Z", "delegated", "cached", "consistent":
			default:
				im.unsupported("volumes", fmt.Sprintf("%s: the mount option %q is ignored", v, opt))
			}
		}
		if _, _, _, err := config.ParseVolume(entry); err != nil {
			im.block("volumes", fmt.Sprintf("%s: cannot be expressed as an appdeploy volume (%v)", v, err))
			continue
		}
		cfg.volumes = append(cfg.volumes, entry)
		im.note("volumes", fmt.Sprintf("%s becomes the appdeploy volume %q mounted at %s. The data does not move by itself: copy it into the new volume before the first deploy (docs/migration.md#moving-a-volume). During a deploy the old and the new container share the volume until the watch window ends (3 minutes by default; set watch.window shorter if two processes on the same files are a problem, as for SQLite). Volumes are not backed up by appdeploy; copy them off the server yourself.", src, name, dst))
		if name != src {
			im.note("volumes", fmt.Sprintf("the volume is renamed from %s to %s", src, name))
		}
	}
}

var badVolume = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

func (im *importer) accessories(cfg *configBuilder) {
	im.seen["accessories"] = true
	acc := asMap(im.root["accessories"])
	for _, name := range sortedKeys(acc) {
		a := asMap(acc[name])
		image := strings.ToLower(asString(a["image"]))
		switch {
		case strings.HasPrefix(image, "postgres") || strings.HasPrefix(image, "postgis/") || strings.Contains(image, "/postgres"):
			cfg.postgres = true
			im.note("accessories."+name, "Postgres: `services: {postgres: true}` gives the app its own database and DATABASE_URL, with daily backups. Your app must read DATABASE_URL (or you set it yourself as a secret). Move the data with pg_dump (docs/migration.md).")
		case strings.HasPrefix(image, "redis") || strings.HasPrefix(image, "valkey") || strings.Contains(image, "/redis"):
			cfg.redis = true
			im.note("accessories."+name, "Redis: `services: {redis: true}` provides REDIS_URL. Redis data is not backed up by appdeploy.")
		default:
			im.block("accessories."+name, fmt.Sprintf("%s (%s): appdeploy only runs Postgres and Redis for you. Run it yourself next to appdeploy, or use a managed service, and point the app at it with a secret.", name, asString(a["image"])))
		}
		if a["directories"] != nil || a["volumes"] != nil || a["files"] != nil {
			im.note("accessories."+name, "its directories, volumes and files are not carried over: copy the data by hand")
		}
	}
}

// misc handles the remaining top-level keys.
func (im *importer) misc(cfg *configBuilder) {
	if v := asString(im.root["retain_containers"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.keepReleases = n
		}
	}
	im.seen["retain_containers"] = true
	handled := map[string]string{
		"boot":                "rolling boot limits: appdeploy rolls out to a group in waves by itself (a canary first, then a quarter at a time)",
		"asset_path":          "asset bridging between versions: appdeploy does not keep the old release's assets",
		"hooks_path":          "Kamal hooks: appdeploy has no hooks. Migrations belong in `release_command`; check .kamal/hooks for anything else you rely on",
		"ssh":                 "ssh settings: appdeploy is used through the user `deploy` created by install.sh",
		"aliases":             "aliases",
		"labels":              "docker labels",
		"logging":             "log settings: containers keep three 20 MB log files",
		"primary_role":        "primary role",
		"readiness_delay":     "readiness delay",
		"require_destination": "destinations",
		"minimum_version":     "minimum Kamal version",
		"image":               "the image name: appdeploy names images itself (ad/<app>:vN)",
		"sshkit":              "SSH client settings",
		"run_directory":       "Kamal's run directory",
		"secrets_path":        "the secrets file location",
		"allow_empty_roles":   "empty roles",
	}
	for _, k := range sortedKeys(im.root) {
		if im.seen[k] {
			continue
		}
		if msg, ok := handled[k]; ok {
			switch k {
			case "image", "aliases", "ssh", "primary_role", "minimum_version", "require_destination", "secrets_path", "run_directory", "sshkit", "logging", "readiness_delay", "allow_empty_roles":
				// ignored silently: nothing to carry over
			case "hooks_path":
				im.note(k, msg)
			default:
				im.unsupported(k, msg)
			}
			continue
		}
		im.unsupported(k, "not understood by the importer")
	}
}

var secretLine = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=`)

// secretNames adds the names listed in .kamal/secrets that the app uses. Values
// are never read: the file only names where they come from.
func (im *importer) secretNames(secrets []byte) {
	if secrets == nil {
		return
	}
	used := map[string]bool{}
	for _, n := range im.r.SecretNames {
		used[n] = true
	}
	for _, line := range strings.Split(string(secrets), "\n") {
		if m := secretLine.FindStringSubmatch(line); m != nil {
			n := m[1]
			if strings.HasPrefix(n, "KAMAL_") || strings.HasSuffix(n, "REGISTRY_PASSWORD") {
				continue // the registry's credential, not the app's
			}
			if !used[n] {
				im.note(".kamal/secrets", fmt.Sprintf("%s is defined there but not listed under env.secret, so the app does not receive it; add it if it should", n))
			}
		}
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// finish builds appdeploy.yml, validates it and writes the commands and verdict.
func (im *importer) finish(cfg *configBuilder, roles []roleSpec) {
	r := im.r
	sort.Strings(r.SecretNames)
	r.SecretNames = dedupe(r.SecretNames)

	var b bytes.Buffer
	fmt.Fprintf(&b, "# Generated by `appdeploy import kamal` from the Kamal service %q.\n# Check it with: appdeploy check appdeploy.yml\n", asString(im.root["service"]))
	if cfg.buildDF != "" || cfg.buildCtx != "" || len(cfg.buildArgs) > 0 {
		b.WriteString("build:\n")
		if cfg.buildDF != "" {
			fmt.Fprintf(&b, "  dockerfile: %s\n", yamlStr(cfg.buildDF))
		}
		if cfg.buildCtx != "" {
			fmt.Fprintf(&b, "  context: %s\n", yamlStr(cfg.buildCtx))
		}
		if len(cfg.buildArgs) > 0 {
			b.WriteString("  args:\n")
			keys := make([]string, 0, len(cfg.buildArgs))
			for k := range cfg.buildArgs {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(&b, "    %s: %s\n", k, yamlStr(cfg.buildArgs[k]))
			}
		}
	}
	if len(r.Domains) > 0 {
		fmt.Fprintf(&b, "domains: [%s]\n", strings.Join(quoteAll(r.Domains), ", "))
	}
	if len(r.Env) > 0 {
		b.WriteString("env:\n")
		keys := make([]string, 0, len(r.Env))
		for k := range r.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "  %s: %s\n", k, strconv.Quote(r.Env[k]))
		}
	}
	hostsOf := func(role roleSpec) string { return strings.Join(role.hosts, ",") }
	sameHosts := true
	for _, role := range roles {
		if hostsOf(role) != hostsOf(roles[0]) {
			sameHosts = false
		}
	}
	b.WriteString("roles:\n")
	hasWeb := false
	for _, role := range roles {
		hasWeb = hasWeb || role.name == "web"
	}
	if !hasWeb {
		b.WriteString("  web: {}\n")
	}
	for _, role := range roles {
		fmt.Fprintf(&b, "  %s:\n", role.name)
		wrote := false
		if !sameHosts {
			fmt.Fprintf(&b, "    on: [%s]\n", role.name)
			wrote = true
		}
		if role.name == "web" {
			if cfg.port > 0 {
				fmt.Fprintf(&b, "    port: %d\n", cfg.port)
				wrote = true
			}
			if cfg.healthPath != "" || cfg.healthTimeou != "" || cfg.startPeriod != "" {
				b.WriteString("    healthcheck:\n")
				if cfg.healthPath != "" {
					fmt.Fprintf(&b, "      path: %s\n", yamlStr(cfg.healthPath))
				}
				if cfg.healthTimeou != "" {
					fmt.Fprintf(&b, "      timeout: %s\n", cfg.healthTimeou)
				}
				if cfg.startPeriod != "" {
					fmt.Fprintf(&b, "      start_period: %s\n", cfg.startPeriod)
				}
				wrote = true
			}
			if cfg.stopTimeout != "" {
				fmt.Fprintf(&b, "    stop_timeout: %s\n", cfg.stopTimeout)
				wrote = true
			}
		}
		if role.cmd != "" {
			fmt.Fprintf(&b, "    cmd: %s\n", yamlStr(role.cmd))
			wrote = true
		}
		if role.memory != "" {
			fmt.Fprintf(&b, "    memory: %s\n", yamlStr(strings.ToUpper(role.memory)))
			wrote = true
		}
		if role.cpus != "" {
			fmt.Fprintf(&b, "    cpus: %s\n", role.cpus)
			wrote = true
		}
		if len(cfg.volumes) > 0 {
			fmt.Fprintf(&b, "    volumes: [%s]\n", strings.Join(quoteAll(cfg.volumes), ", "))
			wrote = true
		}
		if !wrote {
			b.Truncate(b.Len() - len(fmt.Sprintf("  %s:\n", role.name)))
			fmt.Fprintf(&b, "  %s: {}\n", role.name)
		}
	}
	if !sameHosts {
		im.note("servers", "roles run on different servers: label the servers (web, "+roleNames(roles)+") when you create the group; see docs/groups.md")
	}
	if cfg.postgres || cfg.redis {
		b.WriteString("services:\n")
		if cfg.postgres {
			b.WriteString("  postgres: true\n")
		}
		if cfg.redis {
			b.WriteString("  redis: true\n")
		}
	}
	if cfg.keepReleases > 0 {
		fmt.Fprintf(&b, "keep_releases: %d\n", cfg.keepReleases)
	}
	r.Config = b.String()
	if parsed, err := config.Parse([]byte(r.Config)); err != nil {
		// A bug of the importer, not of the input: say so instead of printing a file that will not deploy.
		im.block("appdeploy.yml", "the generated file is not valid ("+err.Error()+"): please report this")
	} else {
		for _, w := range parsed.Warnings() {
			im.note("appdeploy.yml", w)
		}
	}

	host := "SERVER"
	if len(r.Hosts) > 0 {
		host = r.Hosts[0]
	}
	remote := "deploy@" + host
	create := fmt.Sprintf("appdeploy --host %s apps create %s", remote, r.App)
	for _, d := range r.Domains {
		create += " --domain " + d
	}
	r.Commands = append(r.Commands, create)
	var sets []string
	for _, n := range r.SecretNames {
		sets = append(sets, fmt.Sprintf(`%s="$%s"`, n, n))
	}
	if len(sets) > 0 {
		r.Commands = append(r.Commands, fmt.Sprintf("appdeploy --host %s secrets set %s %s --no-deploy", remote, r.App, strings.Join(sets, " ")))
	}
	if len(r.Env) > 0 {
		im.note("env.clear", fmt.Sprintf("%d non-secret variables go into the env: section of appdeploy.yml (kept in git, changed by a commit); only real secrets are set with `secrets set`", len(r.Env)))
	}
	r.Commands = append(r.Commands,
		fmt.Sprintf("git remote add prod %s:%s", remote, r.App),
		"git push prod main")

	switch {
	case len(r.Blockers) > 0:
		r.Verdict = "blocked"
	case len(r.Unsupported) > 0 || len(r.Notes) > 0:
		r.Verdict = "ready-with-notes"
	default:
		r.Verdict = "ready"
	}
}

func roleNames(roles []roleSpec) string {
	var n []string
	for _, r := range roles {
		if r.name != "web" {
			n = append(n, r.name)
		}
	}
	return strings.Join(n, ", ")
}

func dedupe(s []string) []string {
	var out []string
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

func quoteAll(s []string) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[i] = yamlStr(v)
	}
	return out
}

// yamlStr quotes a string for YAML only when it has to be.
func yamlStr(s string) string {
	if s != "" && regexp.MustCompile(`^[A-Za-z0-9_./-]+$`).MatchString(s) && !strings.EqualFold(s, "true") && !strings.EqualFold(s, "false") && !strings.EqualFold(s, "null") && !regexp.MustCompile(`^[0-9.]+$`).MatchString(s) {
		return s
	}
	return strconv.Quote(s)
}
