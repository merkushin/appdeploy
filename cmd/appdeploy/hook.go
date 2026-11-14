package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"appdeploy/internal/api"
	"appdeploy/internal/config"
)

const zeroSHA = "0000000000000000000000000000000000000000"

// runHook implements git's post-receive hook: it asks the agent to deploy and
// streams the journal back, which git shows to the pusher as "remote:" lines.
// The deploy runs in the agent, so killing git push does not stop it.
func runHook(args []string) int {
	if len(args) == 0 || args[0] != "post-receive" {
		return die("usage: appdeploy hook post-receive --app NAME [--socket PATH]")
	}
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	app := fs.String("app", "", "app name")
	socket := fs.String("socket", envOr("APPDEPLOY_SOCKET", defaultSocket), "agent socket")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	c := api.NewClient(*socket)
	ctx := context.Background()
	status := 0
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 3 || f[1] == zeroSHA {
			continue // branch deletion or malformed line
		}
		resp, err := c.Push(ctx, *app, f[2], f[1])
		if err != nil {
			fmt.Fprintf(os.Stdout, "appdeploy: %v\n", err)
			status = 1
			continue
		}
		if resp.DeployID == "" {
			fmt.Println(resp.Message)
			continue
		}
		state, errMsg, err := c.StreamEvents(ctx, resp.DeployID, os.Stdout)
		if err != nil {
			fmt.Printf("appdeploy: lost the event stream: %v (the deploy continues on the server)\n", err)
			status = 1
			continue
		}
		if state != "succeeded" {
			fmt.Printf("appdeploy: deploy %s: %s\n", state, errMsg)
			status = 1
		}
	}
	return status
}

// runShell is the SSH forced command. It allows git pushes/fetches of app
// repositories and appdeploy client commands, and nothing else.
func runShell(args []string) int {
	fs := flag.NewFlagSet("shell", flag.ContinueOnError)
	role := fs.String("role", roleAdmin, "what this SSH key may do: viewer, deployer or admin")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !validRole(*role) {
		return die("unknown role %q (use viewer, deployer or admin)", *role)
	}
	orig := os.Getenv("SSH_ORIGINAL_COMMAND")
	if orig == "" {
		fmt.Fprintln(os.Stderr, "appdeploy: interactive shells are not available; use git push or an appdeploy command")
		return 1
	}
	words, err := splitCommand(orig)
	if err != nil || len(words) == 0 {
		return die("cannot parse command")
	}
	words = extractJSON(words)
	if len(words) == 0 {
		return die("cannot parse command")
	}
	if err := allowed(*role, words); err != nil {
		return die("%v", err)
	}
	dataDir := envOr("APPDEPLOY_DATA_DIR", defaultDataDir)
	switch words[0] {
	case "git-receive-pack", "git-upload-pack":
		if len(words) != 2 {
			return die("usage: %s <app>", words[0])
		}
		app := strings.Trim(strings.TrimPrefix(words[1], "/"), "/")
		app = strings.TrimSuffix(app, ".git")
		if !config.ValidAppName(app) {
			return die("invalid app name")
		}
		repo := filepath.Join(dataDir, "apps", app, "repo.git")
		if _, err := os.Stat(repo); err != nil {
			return die("no such app %q (create it with: appdeploy apps create %s)", app, app)
		}
		return execReplace(words[0], repo)
	}
	// Anything else is an appdeploy client command, run against the local
	// agent. Global flags (--host, --socket) are not accepted from SSH users:
	// they must not be able to redirect the command elsewhere.
	if strings.HasPrefix(words[0], "-") {
		return die("unexpected option %q", words[0])
	}
	return clientRun(envOr("APPDEPLOY_SOCKET", defaultSocket), words)
}

// execReplace replaces this process with the command.
func execReplace(name string, args ...string) int {
	path, err := exec.LookPath(name)
	if err != nil {
		return die("%v", err)
	}
	if err := syscall.Exec(path, append([]string{name}, args...), os.Environ()); err != nil {
		return die("%v", err)
	}
	return 0
}
