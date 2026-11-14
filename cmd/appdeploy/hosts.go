package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"appdeploy/internal/config"
)

// userHosts is ~/.config/appdeploy/hosts.yml: servers a person manages from this machine.
type userHosts struct {
	Default string            `yaml:"default"`
	Hosts   map[string]string `yaml:"hosts"`
}

func userHostsPath() string {
	if p := os.Getenv("APPDEPLOY_HOSTS_FILE"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "appdeploy", "hosts.yml")
}

func loadUserHosts() userHosts {
	var u userHosts
	if p := userHostsPath(); p != "" {
		if data, err := os.ReadFile(p); err == nil {
			_ = yaml.Unmarshal(data, &u)
		}
	}
	return u
}

// projectHosts reads only host and hosts from appdeploy.yml in dir. It is lenient:
// a mistake elsewhere in the file is `appdeploy check`'s business, not a reason to
// refuse a command.
func projectHosts(dir string) (host string, hosts map[string]string) {
	data, err := os.ReadFile(filepath.Join(dir, config.FileName))
	if err != nil {
		return "", nil
	}
	var v struct {
		Host  string            `yaml:"host"`
		Hosts map[string]string `yaml:"hosts"`
	}
	if yaml.Unmarshal(data, &v) != nil {
		return "", nil
	}
	return v.Host, v.Hosts
}

var sshRemote = regexp.MustCompile(`^ssh://([A-Za-z0-9._-]+@)?([A-Za-z0-9._-]+)/.+$`)

// remoteHost extracts the ssh destination from a git remote URL such as
// deploy@host:app or ssh://deploy@host/app. Other forms (a port, https) give "".
func remoteHost(url string) string {
	if m := sshRemote.FindStringSubmatch(url); m != nil {
		return m[1] + m[2]
	}
	if strings.Contains(url, "://") {
		return ""
	}
	if i := strings.Index(url, ":"); i > 0 && !strings.Contains(url[:i], "/") {
		h := url[:i]
		if config.ValidHost(h) {
			return h
		}
	}
	return ""
}

// gitRemoteHost looks at the remotes `appdeploy` and `prod` of the repository in dir.
func gitRemoteHost(dir string) string {
	for _, name := range []string{"appdeploy", "prod"} {
		cmd := exec.Command("git", "-C", dir, "remote", "get-url", name)
		out, err := cmd.Output()
		if err != nil {
			continue
		}
		if h := remoteHost(strings.TrimSpace(string(out))); h != "" {
			return h
		}
	}
	return ""
}

// resolveHost decides where a client command runs. flag is the value of --host
// (an address or the name of a server in appdeploy.yml or hosts.yml). It returns the
// ssh destination, "" for the local agent, and where the answer came from (for
// anything but the flag and the environment, which are explicit).
func resolveHost(flag, dir string) (host, source string, err error) {
	projHost, projHosts := projectHosts(dir)
	user := loadUserHosts()
	named := func(name string) (string, bool) {
		if h, ok := projHosts[name]; ok {
			return h, true
		}
		h, ok := user.Hosts[name]
		return h, ok
	}
	check := func(h, from string) (string, string, error) {
		if !config.ValidHost(h) {
			return "", "", fmt.Errorf("the host %q (from %s) is not a user@host or host address", h, from)
		}
		return h, from, nil
	}
	if flag != "" {
		if h, ok := named(flag); ok {
			return check(h, "")
		}
		if config.ValidHost(flag) {
			return flag, "", nil
		}
		return "", "", fmt.Errorf("--host %q is neither a user@host address nor a server named in %s or %s", flag, config.FileName, userHostsPath())
	}
	if h := os.Getenv("APPDEPLOY_HOST"); h != "" {
		if n, ok := named(h); ok {
			h = n
		}
		return check(h, "")
	}
	if projHost != "" {
		if n, ok := named(projHost); ok {
			projHost = n
		}
		return check(projHost, config.FileName)
	}
	if len(projHosts) == 1 {
		for _, h := range projHosts {
			return check(h, config.FileName)
		}
	}
	if len(projHosts) > 1 {
		names := make([]string, 0, len(projHosts))
		for n := range projHosts {
			names = append(names, n)
		}
		sort.Strings(names)
		return "", "", fmt.Errorf("%s names several servers (%s): choose one with --host NAME", config.FileName, strings.Join(names, ", "))
	}
	if h := gitRemoteHost(dir); h != "" {
		return h, "the git remote", nil
	}
	if user.Default != "" {
		h, ok := user.Hosts[user.Default]
		if !ok {
			h = user.Default
		}
		return check(h, userHostsPath())
	}
	return "", "", nil
}

// runHosts is `appdeploy hosts [list | add NAME ADDRESS [--default] | remove NAME]`:
// the file of servers kept on this machine.
func runHosts(args []string) int {
	path := userHostsPath()
	if path == "" {
		return die("cannot find the user configuration directory")
	}
	u := loadUserHosts()
	if len(args) == 0 || args[0] == "list" {
		names := make([]string, 0, len(u.Hosts))
		for n := range u.Hosts {
			names = append(names, n)
		}
		sort.Strings(names)
		if jsonOut {
			return emitCode(map[string]any{"default": u.Default, "hosts": u.Hosts, "file": path})
		}
		for _, n := range names {
			mark := " "
			if n == u.Default {
				mark = "*"
			}
			fmt.Printf("%s %-16s %s\n", mark, n, u.Hosts[n])
		}
		if len(names) == 0 {
			fmt.Printf("no servers saved (%s); add one: appdeploy hosts add NAME user@host [--default]\n", path)
		}
		return 0
	}
	switch args[0] {
	case "add":
		var pos []string
		makeDefault := false
		for _, a := range args[1:] {
			if a == "--default" {
				makeDefault = true
			} else {
				pos = append(pos, a)
			}
		}
		if len(pos) != 2 || !config.ValidAppName(pos[0]) || !config.ValidHost(pos[1]) {
			reportError("usage: appdeploy hosts add NAME user@host [--default] (NAME: lowercase letters, digits, dashes)")
			return 2
		}
		if u.Hosts == nil {
			u.Hosts = map[string]string{}
		}
		u.Hosts[pos[0]] = pos[1]
		if makeDefault || u.Default == "" {
			u.Default = pos[0]
		}
	case "remove":
		if len(args) != 2 {
			reportError("usage: appdeploy hosts remove NAME")
			return 2
		}
		if _, ok := u.Hosts[args[1]]; !ok {
			return die("no server named %q", args[1])
		}
		delete(u.Hosts, args[1])
		if u.Default == args[1] {
			u.Default = ""
		}
	default:
		reportError("usage: appdeploy hosts [list | add NAME user@host [--default] | remove NAME]")
		return 2
	}
	data, err := yaml.Marshal(u)
	if err == nil {
		if err = os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
			err = os.WriteFile(path, data, 0o600)
		}
	}
	if err != nil {
		return die("%v", err)
	}
	if jsonOut {
		return emitCode(map[string]any{"ok": true, "file": path})
	}
	fmt.Println("saved in", path)
	return 0
}

func emitCode(v any) int {
	code, _ := emit(v)
	return code
}
