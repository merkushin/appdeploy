// Command appdeploy is both the server agent and the command line client.
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"appdeploy/internal/docker"
	"appdeploy/internal/update"
)

// Set at build time by the release script: -ldflags "-X main.version=1.2.3 ...".
var (
	version         = "dev"
	updateRepo      = "merkushin/appdeploy" // GitHub repository that publishes releases
	updatePublicKey = ""                    // base64 ed25519 key the releases are signed with
)

const defaultSocket = "/run/appdeploy/agent.sock"
const defaultDataDir = "/var/lib/appdeploy"

const usage = `appdeploy - deploy web apps from git push

Server side:
  appdeploy agent [flags]          run the agent (normally under systemd)

Client side (talks to the agent; --host runs it on a remote server over SSH):
  appdeploy [--host user@host] [--socket path] [--json] <command>

  --json prints JSON instead of text: lists and objects as one line, deploys as one JSON
      line per journal entry ending with {"done":true,...}, errors as {"error":"..."}.

  ping                             check that the agent answers (no output, exit status 0)
  check [file] [--json]            validate an appdeploy.yml on this machine, without a server
  import kamal [deploy.yml] [--secrets file] [--out file] [--force]   turn a Kamal setup into appdeploy.yml and commands (on this machine)
  apps create <app> [--domain d]... [--branch main]
  apps list
  apps destroy <app> [--delete-volumes]
  status [<app>]
  deploy <app> [--ref sha | --image ref [--config appdeploy.yml|-]] [--detach]
  redeploy <app> [--detach]        redeploy the current image with current secrets
  rollback <app> [--to N] [--detach]
  releases <app>
  logs <app> [--role web] [-f] [--tail N]
  exec <app> [--role web] -- <cmd...>
  secrets set <app> KEY=value... [--no-deploy]
  secrets unset <app> KEY... [--no-deploy]
  secrets list <app>
  secrets export <app>
  export <app> --format kamal|compose|k8s|fly [--out dir]   the app's setup for another tool
  db backup|list <app>             Postgres backups (daily, kept 7 days + 4 weeks)
  db restore <app> <name>          replace the app's database with a backup
  domains set <app> <domain>...
  group init|join ...              create a group of servers / join one (before starting the agent)
  group invite|servers             invite a server / list members with their agent versions
  group set-version <vX.Y.Z|none>  have every server install that release, one at a time
  group remove <name>
  self-update [--check] [--force] [--repo owner/name]   install the latest signed release (admin)
  version [--verbose]              --verbose also shows the key this build trusts for updates
`

func main() {
	args := extractJSON(os.Args[1:])
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch args[0] {
	case "agent":
		os.Exit(runAgent(args[1:]))
	case "hook":
		os.Exit(runHook(args[1:]))
	case "shell":
		os.Exit(runShell(args[1:]))
	case "check":
		os.Exit(runCheck(args[1:]))
	case "import":
		os.Exit(runImport(args[1:]))
	case "recover":
		os.Exit(runRecover(args[1:]))
	case "version", "--version":
		if jsonOut {
			emitVersion()
			return
		}
		fmt.Println("appdeploy", version)
		if len(args) > 1 && (args[1] == "--verbose" || args[1] == "-v") {
			printTrust()
		}
		return
	case "help", "-h", "--help":
		fmt.Print(usage)
		return
	}
	if args[0] == "group" && len(args) > 1 && (args[1] == "init" || args[1] == "join") {
		os.Exit(runGroupOffline(args[1:]))
	}
	os.Exit(runClient(args))
}

// execSSH runs the command on a remote server through its forced command.
func execSSH(host string, args []string) int {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shQuote(a)
	}
	cmd := exec.Command("ssh", "-T", host, strings.Join(quoted, " "))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
				return ws.ExitStatus()
			}
		}
		fmt.Fprintln(os.Stderr, "appdeploy:", err)
		return 1
	}
	return 0
}

func shQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`!*?[]{}()<>|&;~#") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func splitCommand(s string) ([]string, error) { return docker.SplitCommand(s) }

func die(format string, a ...any) int {
	reportError(fmt.Sprintf(format, a...))
	return 1
}

// emitVersion is `version --json`.
func emitVersion() {
	out := map[string]any{"version": version, "update_repository": updateRepo}
	if pub, err := base64.StdEncoding.DecodeString(updatePublicKey); err == nil && len(pub) == ed25519.PublicKeySize {
		out["release_key"] = updatePublicKey
		out["release_key_fingerprint"] = update.Fingerprint(ed25519.PublicKey(pub))
	}
	emit(out)
}

// printTrust shows what this binary trusts: the repository its updates come from
// and the key they must be signed with, with a fingerprint to compare out of band.
func printTrust() {
	fmt.Println("update repository:", updateRepo)
	pub, err := base64.StdEncoding.DecodeString(updatePublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		fmt.Println("release key: none (this build cannot verify updates; it was not made by scripts/release.sh)")
		return
	}
	fmt.Println("release key (ed25519, base64):", updatePublicKey)
	fmt.Println("release key fingerprint (sha256):", update.Fingerprint(ed25519.PublicKey(pub)))
}
