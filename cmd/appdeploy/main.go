// Command appdeploy is both the server agent and the command line client.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"appdeploy/internal/docker"
)

const version = "0.1.0-m1"

const defaultSocket = "/run/appdeploy/agent.sock"
const defaultDataDir = "/var/lib/appdeploy"

const usage = `appdeploy - deploy web apps from git push

Server side:
  appdeploy agent [flags]          run the agent (normally under systemd)

Client side (talks to the agent; --host runs it on a remote server over SSH):
  appdeploy [--host user@host] [--socket path] <command>

  apps create <app> [--domain d]... [--branch main]
  apps list
  apps destroy <app>
  status [<app>]
  deploy <app> [--ref sha] [--detach]
  redeploy <app> [--detach]        redeploy the current image with current secrets
  rollback <app> [--to N] [--detach]
  releases <app>
  logs <app> [--role web] [-f] [--tail N]
  exec <app> [--role web] -- <cmd...>
  secrets set <app> KEY=value... [--no-deploy]
  secrets unset <app> KEY... [--no-deploy]
  secrets list <app>
  secrets export <app>
  db backup|list <app>             Postgres backups (daily, kept 7 days + 4 weeks)
  db restore <app> <name>          replace the app's database with a backup
  domains set <app> <domain>...
  group init|join ...              create a group of servers / join one (before starting the agent)
  group invite|servers             invite a server / list members
  group remove <name>
  version
`

func main() {
	args := os.Args[1:]
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
	case "version", "--version":
		fmt.Println("appdeploy", version)
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
	fmt.Fprintf(os.Stderr, "appdeploy: "+format+"\n", a...)
	return 1
}
