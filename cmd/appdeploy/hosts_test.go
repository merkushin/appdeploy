package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func isolate(t *testing.T) (dir string) {
	t.Helper()
	dir = t.TempDir()
	t.Setenv("APPDEPLOY_HOSTS_FILE", filepath.Join(t.TempDir(), "hosts.yml"))
	t.Setenv("APPDEPLOY_HOST", "")
	return dir
}

func writeYML(t *testing.T, dir, s string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "appdeploy.yml"), []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHostComesFromTheFlagThenTheEnvironmentThenTheProject(t *testing.T) {
	dir := isolate(t)
	writeYML(t, dir, "host: deploy@from-file\nhosts:\n  staging: deploy@stg\n")
	if h, src, err := resolveHost("", dir); err != nil || h != "deploy@from-file" || src != "appdeploy.yml" {
		t.Errorf("project: %q %q %v", h, src, err)
	}
	t.Setenv("APPDEPLOY_HOST", "deploy@from-env")
	if h, src, _ := resolveHost("", dir); h != "deploy@from-env" || src != "" {
		t.Errorf("env: %q %q", h, src)
	}
	if h, _, _ := resolveHost("staging", dir); h != "deploy@stg" {
		t.Errorf("a name from appdeploy.yml: %q", h)
	}
	if h, _, _ := resolveHost("root@1.2.3.4", dir); h != "root@1.2.3.4" {
		t.Errorf("an address: %q", h)
	}
}

func TestHostRefusesWhatSSHWouldReadAsAnOption(t *testing.T) {
	dir := isolate(t)
	if _, _, err := resolveHost("-oProxyCommand=x", dir); err == nil {
		t.Error("an option as the host was accepted")
	}
	t.Setenv("APPDEPLOY_HOST", "-oProxyCommand=x")
	if _, _, err := resolveHost("", dir); err == nil {
		t.Error("an option in APPDEPLOY_HOST was accepted")
	}
}

func TestSeveralNamedServersNeedAChoice(t *testing.T) {
	dir := isolate(t)
	writeYML(t, dir, "hosts:\n  prod: deploy@a\n  staging: deploy@b\n")
	if _, _, err := resolveHost("", dir); err == nil || !strings.Contains(err.Error(), "prod, staging") {
		t.Errorf("err=%v", err)
	}
	writeYML(t, dir, "hosts:\n  prod: deploy@a\n")
	if h, _, err := resolveHost("", dir); err != nil || h != "deploy@a" {
		t.Errorf("a single named server is the default: %q %v", h, err)
	}
}

func TestRemoteHost(t *testing.T) {
	for url, want := range map[string]string{
		"deploy@host.example.com:myapp":   "deploy@host.example.com",
		"ssh://deploy@host.example.com/a": "deploy@host.example.com",
		"host:app":                        "host",
		"https://github.com/u/r.git":      "",
		"ssh://deploy@host:2222/app":      "",
		"/srv/git/app":                    "",
	} {
		if got := remoteHost(url); got != want {
			t.Errorf("%s: got %q want %q", url, got, want)
		}
	}
}

func TestHostFromTheGitRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := isolate(t)
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "prod", "deploy@box.example.com:myapp"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if h, src, err := resolveHost("", dir); err != nil || h != "deploy@box.example.com" || src != "the git remote" {
		t.Errorf("%q %q %v", h, src, err)
	}
}

func TestSavedServersAndTheDefault(t *testing.T) {
	dir := isolate(t)
	jsonOut = false
	if code, _, _ := capture(t, func() int { return runHosts([]string{"add", "home", "deploy@1.2.3.4"}) }); code != 0 {
		t.Fatal("add failed")
	}
	if code, _, _ := capture(t, func() int { return runHosts([]string{"add", "work", "deploy@5.6.7.8"}) }); code != 0 {
		t.Fatal("add failed")
	}
	// The first one saved is the default; names work from anywhere.
	if h, src, _ := resolveHost("", dir); h != "deploy@1.2.3.4" || src == "" {
		t.Errorf("default: %q %q", h, src)
	}
	if h, _, _ := resolveHost("work", dir); h != "deploy@5.6.7.8" {
		t.Errorf("by name: %q", h)
	}
	_, out, _ := capture(t, func() int { return runHosts(nil) })
	if !strings.Contains(out, "* home") || !strings.Contains(out, "work") {
		t.Errorf("list:\n%s", out)
	}
	if code, _, _ := capture(t, func() int { return runHosts([]string{"add", "bad", "-oX"}) }); code != 2 {
		t.Errorf("an option as an address: code=%d", code)
	}
	if code, _, _ := capture(t, func() int { return runHosts([]string{"remove", "home"}) }); code != 0 {
		t.Error("remove failed")
	}
	if h, _, _ := resolveHost("", dir); h != "" {
		t.Errorf("no default after removing it: %q", h)
	}
}
