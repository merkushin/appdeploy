package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// capture runs f and returns what it wrote to stdout and stderr.
func capture(t *testing.T, f func() int) (code int, stdout, stderr string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	ro, wo, _ := os.Pipe()
	re, we, _ := os.Pipe()
	os.Stdout, os.Stderr = wo, we
	defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()
	code = f()
	wo.Close()
	we.Close()
	o, _ := io.ReadAll(ro)
	e, _ := io.ReadAll(re)
	return code, string(o), string(e)
}

func write(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheckAcceptsAGoodFileAndExplainsIt(t *testing.T) {
	jsonOut = false
	p := write(t, "appdeploy.yml", "roles:\n  web:\n    port: 3000\n    healthcheck:\n      path: /healthz\n  worker:\n    cmd: worker\nrelease_command: migrate\nservices:\n  postgres: true\n")
	code, out, errOut := capture(t, func() int { return runCheck([]string{p}) })
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	for _, want := range []string{"is valid", "web", "port 3000", "health GET /healthz", `cmd "worker"`, "release command: migrate", "services: postgres", "watch: 3m0s"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestCheckRejectsABadFileWithTheParsersMessage(t *testing.T) {
	jsonOut = false
	p := write(t, "appdeploy.yml", "roles:\n  web:\n    memroy: 1G\n    memory: lots\n")
	code, _, errOut := capture(t, func() int { return runCheck([]string{p}) })
	if code != 1 || !strings.Contains(errOut, "memroy") {
		t.Errorf("code=%d stderr=%q: the typo should be reported", code, errOut)
	}
}

func TestCheckJSON(t *testing.T) {
	jsonOut = true
	defer func() { jsonOut = false }()
	good := write(t, "good.yml", "roles:\n  web: {}\n")
	code, out, _ := capture(t, func() int { return runCheck([]string{good}) })
	var v struct {
		Valid  bool           `json:"valid"`
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil || code != 0 || !v.Valid || v.Config["keep_releases"] != float64(5) {
		t.Errorf("good: code=%d out=%s err=%v", code, out, err)
	}
	bad := write(t, "bad.yml", "nope: 1\n")
	code, out, _ = capture(t, func() int { return runCheck([]string{bad}) })
	var e struct {
		Valid bool   `json:"valid"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &e); err != nil || code != 1 || e.Valid || !strings.Contains(e.Error, "nope") {
		t.Errorf("bad: code=%d out=%s err=%v", code, out, err)
	}
}

func TestCheckWithoutAFileMeansDefaults(t *testing.T) {
	jsonOut = false
	dir := t.TempDir()
	old, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(old)
	code, out, _ := capture(t, func() int { return runCheck(nil) })
	if code != 0 || !strings.Contains(out, "defaults apply") {
		t.Errorf("no file: code=%d out=%q", code, out)
	}
	// An explicitly named file that does not exist is an error.
	if code, _, _ := capture(t, func() int { return runCheck([]string{"nope.yml"}) }); code != 1 {
		t.Errorf("a missing named file: code=%d", code)
	}
	if code, _, _ := capture(t, func() int { return runCheck([]string{"--wat"}) }); code != 2 {
		t.Errorf("an unknown option: code=%d", code)
	}
}

func TestExtractJSON(t *testing.T) {
	defer func() { jsonOut = false }()
	cases := []struct {
		in, out []string
		json    bool
	}{
		{[]string{"status", "shop", "--json"}, []string{"status", "shop"}, true},
		{[]string{"--json", "status"}, []string{"status"}, true},
		{[]string{"status"}, []string{"status"}, false},
		// After "--" the words belong to the command run in the container.
		{[]string{"exec", "shop", "--", "prog", "--json"}, []string{"exec", "shop", "--", "prog", "--json"}, false},
	}
	for _, c := range cases {
		jsonOut = false
		got := extractJSON(c.in)
		if strings.Join(got, " ") != strings.Join(c.out, " ") || jsonOut != c.json {
			t.Errorf("extractJSON(%v) = %v json=%v, want %v json=%v", c.in, got, jsonOut, c.out, c.json)
		}
	}
}
