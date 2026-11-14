package main

import "fmt"

// Roles of an SSH key, set in authorized_keys: command="appdeploy shell --role deployer".
//
//	viewer   see what runs: status, releases, app and server lists
//	deployer viewer + git push, deploy, redeploy, rollback, logs, secret names
//	admin    everything (the default when no role is given)
const (
	roleViewer   = "viewer"
	roleDeployer = "deployer"
	roleAdmin    = "admin"
)

// validRole reports whether name is a known role.
func validRole(name string) bool {
	return name == roleViewer || name == roleDeployer || name == roleAdmin
}

// viewerCommands and deployerCommands list what each role may run, as
// "command" or "command sub". Anything not listed needs the admin role.
var viewerCommands = map[string]bool{
	"ping": true, "status": true, "releases": true,
	"apps list": true, "group servers": true, "db list": true,
}

var deployerCommands = map[string]bool{
	"deploy": true, "redeploy": true, "rollback": true, "logs": true, "secrets list": true,
	"git-receive-pack": true, "git-upload-pack": true,
}

// allowed reports whether role may run the command given as words (the part of
// the SSH command line after the program name, or a git service).
func allowed(role string, words []string) error {
	if len(words) == 0 {
		return fmt.Errorf("nothing to run")
	}
	if role == roleAdmin {
		return nil
	}
	key := words[0]
	if len(words) > 1 {
		key += " " + words[1]
	}
	ok := viewerCommands[key] || viewerCommands[words[0]]
	if role == roleDeployer {
		ok = ok || deployerCommands[key] || deployerCommands[words[0]]
	}
	if !ok {
		return fmt.Errorf("%q is not allowed for the %s role", key, role)
	}
	return nil
}
