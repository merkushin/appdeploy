package main

import (
	"strings"
	"testing"
)

func TestRolePermissions(t *testing.T) {
	cases := []struct {
		cmd                     string
		viewer, deployer, admin bool
	}{
		{"status shop", true, true, true},
		{"status", true, true, true},
		{"releases shop", true, true, true},
		{"apps list", true, true, true},
		{"group servers", true, true, true},
		{"db list shop", true, true, true},
		{"ping", true, true, true},
		{"logs shop -f", false, true, true},
		{"deploy shop", false, true, true},
		{"redeploy shop", false, true, true},
		{"rollback shop", false, true, true},
		{"secrets list shop", false, true, true},
		{"git-receive-pack shop", false, true, true},
		{"git-upload-pack shop", false, true, true},
		{"secrets set shop A=1", false, false, true},
		{"secrets unset shop A", false, false, true},
		{"secrets export shop", false, false, true},
		{"exec shop -- sh", false, false, true},
		{"apps create shop", false, false, true},
		{"apps destroy shop", false, false, true},
		{"domains set shop a.test", false, false, true},
		{"db backup shop", false, false, true},
		{"db restore shop x", false, false, true},
		{"export shop --format fly", false, false, true},
		{"group invite", false, false, true},
		{"group remove b", false, false, true},
		{"agent", false, false, true}, // refused elsewhere: the shell only runs client commands
	}
	for _, c := range cases {
		words := strings.Fields(c.cmd)
		for role, want := range map[string]bool{roleViewer: c.viewer, roleDeployer: c.deployer, roleAdmin: c.admin} {
			if got := allowed(role, words) == nil; got != want {
				t.Errorf("%s may run %q: got %v, want %v", role, c.cmd, got, want)
			}
		}
	}
	if allowed(roleAdmin, nil) == nil {
		t.Errorf("an empty command was allowed")
	}
	if !validRole("viewer") || validRole("root") || validRole("") {
		t.Errorf("validRole is wrong")
	}
}
