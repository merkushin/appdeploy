package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtures = "../../internal/migrate/testdata/"

func TestImportReportsAnAppWithNotes(t *testing.T) {
	jsonOut = false
	code, out, errOut := capture(t, func() int {
		return runImport([]string{"kamal", fixtures + "sqlite-app.yml", "--secrets", fixtures + "sqlite-app.secrets"})
	})
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	for _, want := range []string{"READY", "TO DECIDE OR CHECK", "notes_data", "volumes: [", "appdeploy.yml", "apps create notes-app", "Nothing was changed on any server"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report lacks %q:\n%s", want, out)
		}
	}
	// Secret values are never read: only names and where they come from.
	if strings.Contains(out, "sk_") || strings.Contains(out, "hunter") {
		t.Errorf("a secret value in the report")
	}
}

func TestImportJSONAndOut(t *testing.T) {
	jsonOut = true
	defer func() { jsonOut = false }()
	out := filepath.Join(t.TempDir(), "appdeploy.yml")
	code, stdout, _ := capture(t, func() int {
		return runImport([]string{"kamal", fixtures + "sqlite-app.yml", "--out", out})
	})
	var r struct {
		Verdict  string `json:"verdict"`
		Config   string `json:"appdeploy_yml"`
		Commands []string
	}
	if err := json.Unmarshal([]byte(stdout), &r); err != nil || code != 0 || r.Verdict != "ready-with-notes" || len(r.Commands) == 0 {
		t.Fatalf("code=%d err=%v out=%s", code, err, stdout)
	}
	written, err := os.ReadFile(out)
	if err != nil || string(written) != r.Config {
		t.Errorf("--out did not write the generated file: %v", err)
	}
	// It does not overwrite without --force.
	code, stdout, _ = capture(t, func() int { return runImport([]string{"kamal", fixtures + "sqlite-app.yml", "--out", out}) })
	if code != 1 || !strings.Contains(stdout, "--force") {
		t.Errorf("overwrite: code=%d out=%s", code, stdout)
	}
	if code, _, _ := capture(t, func() int { return runImport([]string{"kamal", fixtures + "sqlite-app.yml", "--out", out, "--force"}) }); code != 0 {
		t.Errorf("--force: code=%d", code)
	}
}

func TestImportUsageAndMissingFile(t *testing.T) {
	jsonOut = false
	if code, _, _ := capture(t, func() int { return runImport(nil) }); code != 2 {
		t.Errorf("no source: code=%d", code)
	}
	if code, _, _ := capture(t, func() int { return runImport([]string{"heroku"}) }); code != 2 {
		t.Errorf("an unsupported source: code=%d", code)
	}
	if code, _, errOut := capture(t, func() int { return runImport([]string{"kamal", "/nonexistent/deploy.yml"}) }); code != 1 || errOut == "" {
		t.Errorf("a missing file: code=%d stderr=%q", code, errOut)
	}
}
