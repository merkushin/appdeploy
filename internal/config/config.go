// Package config parses and validates appdeploy.yml.
//
// The file is optional: Default returns the configuration used when a
// repository has none. Unknown keys are an error.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	pathpkg "path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// FileName is the name of the optional config file at the repository root.
const FileName = "appdeploy.yml"

const (
	DefaultPort        = 8080
	DefaultMemory      = 512 << 20
	DefaultStopTimeout = 30 * time.Second
)

// Duration is a time.Duration that reads "5s", "3m" in YAML and JSON.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return typeError(n, fmt.Sprintf("invalid duration %q", s))
	}
	*d = Duration(v)
	return nil
}

// typeError reports a bad value as a yaml.TypeError, which the decoder collects
// instead of stopping at: a file with several mistakes shows all of them at once.
func typeError(n *yaml.Node, msg string) error {
	return &yaml.TypeError{Errors: []string{fmt.Sprintf("line %d: %s", n.Line, msg)}}
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }

// Size is a number of bytes written as "512M", "1G", "64K" or a plain number.
type Size int64

var sizeRe = regexp.MustCompile(`^(\d+)\s*([KMGT]?)B?$`)

// ParseSize parses sizes such as "512M" (binary multiples).
func ParseSize(s string) (Size, error) {
	m := sizeRe.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(s)))
	if m == nil {
		return 0, fmt.Errorf("invalid size %q (use e.g. 512M or 1G)", s)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	shift := map[string]uint{"": 0, "K": 10, "M": 20, "G": 30, "T": 40}[m[2]]
	return Size(n << shift), nil
}

func (z *Size) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := ParseSize(s)
	if err != nil {
		return typeError(n, err.Error())
	}
	*z = v
	return nil
}

func (z Size) Bytes() int64 { return int64(z) }

type Build struct {
	Dockerfile string            `yaml:"dockerfile" json:"dockerfile"`
	Context    string            `yaml:"context" json:"context"`
	Args       map[string]string `yaml:"args" json:"args,omitempty"`
}

type Healthcheck struct {
	Path        string   `yaml:"path" json:"path"`
	Timeout     Duration `yaml:"timeout" json:"timeout"`
	StartPeriod Duration `yaml:"start_period" json:"start_period"`
}

type Role struct {
	On             []string    `yaml:"on" json:"on"`
	Port           int         `yaml:"port" json:"port"` // 0: use the image's EXPOSE, else DefaultPort
	Cmd            string      `yaml:"cmd" json:"cmd,omitempty"`
	Memory         Size        `yaml:"memory" json:"memory"`
	CPUs           float64     `yaml:"cpus" json:"cpus"`
	ReplicasPerSrv int         `yaml:"replicas_per_server" json:"replicas_per_server"`
	Healthcheck    Healthcheck `yaml:"healthcheck" json:"healthcheck"`
	Smoke          []string    `yaml:"smoke" json:"smoke,omitempty"`
	StopTimeout    Duration    `yaml:"stop_timeout" json:"stop_timeout"`
	Volumes        []string    `yaml:"volumes" json:"volumes,omitempty"` // "name:/path[:ro]", named volumes only
}

type Services struct {
	Postgres bool `yaml:"postgres" json:"postgres"`
	Redis    bool `yaml:"redis" json:"redis"`
}

type Watch struct {
	Window      Duration `yaml:"window" json:"window"`
	ErrorRate   string   `yaml:"error_rate" json:"error_rate"`
	MinRequests int      `yaml:"min_requests" json:"min_requests"`
	MaxRestarts int      `yaml:"max_restarts" json:"max_restarts"`
}

type App struct {
	App            string            `yaml:"app" json:"app"`
	Branch         string            `yaml:"branch" json:"branch"`
	Build          Build             `yaml:"build" json:"build"`
	Domains        []string          `yaml:"domains" json:"domains,omitempty"`
	Roles          map[string]*Role  `yaml:"roles" json:"roles"`
	Env            map[string]string `yaml:"env" json:"env,omitempty"` // not secret: kept in git and in the release
	ReleaseCommand string            `yaml:"release_command" json:"release_command,omitempty"`
	Services       Services          `yaml:"services" json:"services"`
	Watch          Watch             `yaml:"watch" json:"watch"`
	AllowPartial   bool              `yaml:"allow_partial" json:"allow_partial,omitempty"`
	KeepReleases   int               `yaml:"keep_releases" json:"keep_releases"`
	Notify         []string          `yaml:"notify" json:"notify,omitempty"`
	Host           string            `yaml:"host" json:"host,omitempty"`   // used by the client only: where to run commands
	Hosts          map[string]string `yaml:"hosts" json:"hosts,omitempty"` // named servers for --host NAME
}

// Default returns the configuration of a repository that has no appdeploy.yml.
func Default() *App {
	a := &App{}
	a.applyDefaults()
	return a
}

// Defaults are server-wide defaults for values the config file leaves out.
type Defaults struct {
	WatchWindow time.Duration // 0 = the built-in 3 minutes
}

// Parse reads and validates a config file's content. Empty content gives the defaults.
func Parse(data []byte) (*App, error) { return ParseWith(data, Defaults{}) }

// ParseWith is Parse with server-wide defaults.
func ParseWith(data []byte, d Defaults) (*App, error) {
	a := &App{}
	if d.WatchWindow > 0 {
		a.Watch.Window = Duration(d.WatchWindow)
	}
	if len(bytes.TrimSpace(data)) > 0 {
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(a); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s: %w", FileName, err)
		}
	}
	a.applyDefaults()
	if err := a.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	return a, nil
}

// FromJSON restores a config stored with a release.
func FromJSON(data []byte) (*App, error) {
	a := &App{}
	if err := json.Unmarshal(data, a); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *App) JSON() []byte {
	b, _ := json.Marshal(a)
	return b
}

func (a *App) applyDefaults() {
	if a.Branch == "" {
		a.Branch = "main"
	}
	if a.Build.Dockerfile == "" {
		a.Build.Dockerfile = "Dockerfile"
	}
	if a.Build.Context == "" {
		a.Build.Context = "."
	}
	if a.KeepReleases == 0 {
		a.KeepReleases = 5
	}
	if a.Roles == nil {
		a.Roles = map[string]*Role{}
	}
	if _, ok := a.Roles["web"]; !ok && len(a.Roles) == 0 {
		a.Roles["web"] = &Role{}
	}
	for name, r := range a.Roles {
		if r == nil {
			r = &Role{}
			a.Roles[name] = r
		}
		if len(r.On) == 0 {
			r.On = []string{"web"}
		}
		if r.Memory == 0 {
			r.Memory = DefaultMemory
		}
		if r.CPUs == 0 {
			r.CPUs = 1
		}
		if r.ReplicasPerSrv == 0 {
			r.ReplicasPerSrv = 1
		}
		if r.StopTimeout == 0 {
			r.StopTimeout = Duration(DefaultStopTimeout)
		}
		if name == "web" {
			if r.Healthcheck.Path == "" {
				r.Healthcheck.Path = "/up"
			}
			if r.Healthcheck.Timeout == 0 {
				r.Healthcheck.Timeout = Duration(5 * time.Second)
			}
			if r.Healthcheck.StartPeriod == 0 {
				r.Healthcheck.StartPeriod = Duration(60 * time.Second)
			}
		}
	}
	if a.Watch.Window == 0 {
		a.Watch.Window = Duration(3 * time.Minute)
	}
	if a.Watch.ErrorRate == "" {
		a.Watch.ErrorRate = "2%"
	}
	if a.Watch.MinRequests == 0 {
		a.Watch.MinRequests = 20
	}
	if a.Watch.MaxRestarts == 0 {
		a.Watch.MaxRestarts = 2
	}
}

var roleNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
var appNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// ValidAppName reports whether name can be used as an app name.
func ValidAppName(name string) bool { return appNameRe.MatchString(name) }

// Validate checks values and rejects features this version does not implement yet.
func (a *App) Validate() error {
	if a.App != "" && !ValidAppName(a.App) {
		return fmt.Errorf("app: %q must be lowercase letters, digits and dashes, starting with a letter", a.App)
	}
	if a.KeepReleases < 1 {
		return errors.New("keep_releases must be at least 1")
	}
	if a.Host != "" && !ValidHost(a.Host) {
		return fmt.Errorf("host: %q is not a user@host or host address", a.Host)
	}
	for name, h := range a.Hosts {
		if !ValidAppName(name) {
			return fmt.Errorf("hosts: the name %q must be lowercase letters, digits and dashes, starting with a letter", name)
		}
		if !ValidHost(h) {
			return fmt.Errorf("hosts.%s: %q is not a user@host or host address", name, h)
		}
	}
	if len(a.Roles) == 0 {
		return errors.New("roles: at least one role is required")
	}
	for k, v := range a.Env {
		switch {
		case !envKeyRe.MatchString(k):
			return fmt.Errorf("env.%s: not a valid variable name (letters, digits and underscores, not starting with a digit)", k)
		case k == "PORT":
			return errors.New("env.PORT: the port is set by roles.web.port; appdeploy passes it as PORT")
		case strings.HasPrefix(k, "APPDEPLOY_"):
			return fmt.Errorf("env.%s: names starting with APPDEPLOY_ are set by appdeploy", k)
		case strings.ContainsAny(v, "\r\n"):
			return fmt.Errorf("env.%s: a value must be a single line", k)
		}
	}
	for name, r := range a.Roles {
		if !roleNameRe.MatchString(name) {
			return fmt.Errorf("roles.%s: invalid role name", name)
		}
		if r.Port < 0 || r.Port > 65535 {
			return fmt.Errorf("roles.%s.port: %d out of range", name, r.Port)
		}
		if r.Memory < 4<<20 {
			return fmt.Errorf("roles.%s.memory: at least 4M is required", name)
		}
		if r.CPUs < 0 {
			return fmt.Errorf("roles.%s.cpus: must be positive", name)
		}
		if r.ReplicasPerSrv < 1 || r.ReplicasPerSrv > 32 {
			return fmt.Errorf("roles.%s.replicas_per_server: must be between 1 and 32", name)
		}
		if name == "web" && !strings.HasPrefix(r.Healthcheck.Path, "/") {
			return fmt.Errorf("roles.web.healthcheck.path: must start with /")
		}
		for _, u := range r.Smoke {
			if !strings.HasPrefix(u, "/") {
				return fmt.Errorf("roles.%s.smoke: %q must be a path starting with /", name, u)
			}
		}
		paths := map[string]bool{}
		for _, v := range r.Volumes {
			_, path, _, err := ParseVolume(v)
			if err != nil {
				return fmt.Errorf("roles.%s.volumes: %w", name, err)
			}
			if paths[path] {
				return fmt.Errorf("roles.%s.volumes: two volumes are mounted at %s", name, path)
			}
			paths[path] = true
		}
	}
	return nil
}

// WebRole returns the HTTP role, or nil if the app has none.
func (a *App) WebRole() *Role { return a.Roles["web"] }

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var volumeNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// ParseVolume reads "name:/path" or "name:/path:ro". Only named volumes are
// accepted: a host path would let a config reach files of the server.
func ParseVolume(s string) (name, path string, readOnly bool, err error) {
	parts := strings.Split(s, ":")
	if len(parts) == 3 && parts[2] == "ro" {
		readOnly = true
		parts = parts[:2]
	}
	if len(parts) != 2 {
		return "", "", false, fmt.Errorf("%q: use name:/path or name:/path:ro", s)
	}
	name, path = parts[0], parts[1]
	if !volumeNameRe.MatchString(name) {
		return "", "", false, fmt.Errorf("%q: the volume name must be lowercase letters, digits, - and _ (host paths are not allowed: use a named volume)", s)
	}
	clean := pathpkg.Clean(path)
	if !strings.HasPrefix(path, "/") || clean != path || path == "/" {
		return "", "", false, fmt.Errorf("%q: the mount path must be an absolute, clean path other than /", s)
	}
	for _, bad := range []string{"/proc", "/sys", "/dev"} {
		if path == bad || strings.HasPrefix(path, bad+"/") {
			return "", "", false, fmt.Errorf("%q: %s cannot be mounted over", s, bad)
		}
	}
	return name, path, readOnly, nil
}

var secretish = regexp.MustCompile(`(?i)(PASSWORD|PASSWD|SECRET|TOKEN|PRIVATE|CREDENTIAL|API_?KEY|ACCESS_?KEY|(^|_)KEY$)`)

// Warnings lists things that are valid but probably not what was meant.
func (a *App) Warnings() []string {
	var w []string
	keys := make([]string, 0, len(a.Env))
	for k := range a.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if secretish.MatchString(k) {
			w = append(w, fmt.Sprintf("env.%s looks like a secret: values in appdeploy.yml are kept in git. Use `appdeploy secrets set` for secrets", k))
		}
	}
	names := make([]string, 0, len(a.Roles))
	for n := range a.Roles {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		r := a.Roles[n]
		for _, v := range r.Volumes {
			if _, _, ro, _ := ParseVolume(v); !ro && r.ReplicasPerSrv > 1 {
				w = append(w, fmt.Sprintf("roles.%s: %d replicas per server share the volume in %q; that is only safe for data that tolerates several writers (not SQLite or similar)", n, r.ReplicasPerSrv, v))
			}
		}
	}
	return w
}

var hostRe = regexp.MustCompile(`^([A-Za-z0-9._-]+@)?[A-Za-z0-9._:\[\]-]+$`)

// ValidHost reports whether s can be given to ssh as a destination: no spaces, and
// no leading dash, which ssh would read as an option.
func ValidHost(s string) bool { return !strings.HasPrefix(s, "-") && hostRe.MatchString(s) }
