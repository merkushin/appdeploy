package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"appdeploy/internal/migrate"
)

// runImport implements `appdeploy import kamal`: it reads a Kamal setup on this
// machine and reports what it becomes on appdeploy. It changes nothing on any
// server and prints no secret values (it does not read them).
func runImport(args []string) int {
	if len(args) == 0 || args[0] != "kamal" {
		reportError("usage: appdeploy import kamal [config/deploy.yml] [--secrets .kamal/secrets] [--out appdeploy.yml] [--json]")
		return 2
	}
	fs := flag.NewFlagSet("import kamal", flag.ContinueOnError)
	secretsPath := fs.String("secrets", "", "Kamal's secrets file (default: .kamal/secrets next to the config's directory, if present)")
	out := fs.String("out", "", "write the generated appdeploy.yml to this file instead of printing it")
	force := fs.Bool("force", false, "overwrite --out if it exists")
	pos, err := parseFlags(fs, args[1:])
	if err != nil || len(pos) > 1 {
		reportError("usage: appdeploy import kamal [config/deploy.yml] [--secrets .kamal/secrets] [--out appdeploy.yml] [--json]")
		return 2
	}
	file := "config/deploy.yml"
	if len(pos) == 1 {
		file = pos[0]
	}
	deploy, err := os.ReadFile(file)
	if err != nil {
		reportError(err.Error())
		return 1
	}
	var secrets []byte
	candidates := []string{*secretsPath}
	if *secretsPath == "" {
		candidates = []string{filepath.Join(filepath.Dir(file), "..", ".kamal", "secrets"), filepath.Join(".kamal", "secrets")}
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if b, err := os.ReadFile(c); err == nil {
			secrets = b
			break
		} else if *secretsPath != "" {
			reportError(err.Error())
			return 1
		}
	}

	res, err := migrate.FromKamal(deploy, secrets)
	if err != nil {
		reportError(err.Error())
		return 1
	}
	wrote := ""
	if *out != "" {
		if _, err := os.Stat(*out); err == nil && !*force {
			reportError(fmt.Sprintf("%s exists; use --force to overwrite it", *out))
			return 1
		}
		if err := os.WriteFile(*out, []byte(res.Config), 0o644); err != nil {
			reportError(err.Error())
			return 1
		}
		wrote = *out
	}
	if jsonOut {
		emit(res)
	} else {
		printImport(res, file, wrote)
	}
	if res.Verdict == "blocked" {
		return 1
	}
	return 0
}

func printImport(r *migrate.Result, file, wrote string) {
	verdict := map[string]string{
		"ready":            "READY: nothing stands in the way",
		"ready-with-notes": "READY, with things to read below",
		"blocked":          "BLOCKED: it cannot run on appdeploy as it is",
	}[r.Verdict]
	fmt.Printf("%s: app %q on %s\n%s\n\n", file, r.App, strings.Join(r.Hosts, ", "), verdict)
	if wrote != "" {
		fmt.Printf("appdeploy.yml written to %s:\n\n", wrote)
	} else {
		fmt.Print("appdeploy.yml (save it in your repository):\n\n")
	}
	for _, l := range strings.Split(strings.TrimRight(r.Config, "\n"), "\n") {
		fmt.Println("    " + l)
	}
	section := func(title string, items []migrate.Item) {
		if len(items) == 0 {
			return
		}
		fmt.Printf("\n%s\n", title)
		for _, it := range items {
			fmt.Printf("  - %s: %s\n", it.Key, it.Message)
		}
	}
	section("BLOCKERS: the app does not work on appdeploy until these are solved", r.Blockers)
	section("NOT CARRIED OVER: no equivalent in appdeploy", r.Unsupported)
	section("TO DECIDE OR CHECK", r.Notes)
	fmt.Println("\nCOMMANDS (run them in this order; the values of secrets come from your environment):")
	for i, c := range r.Commands {
		fmt.Printf("  %d. %s\n", i+1, c)
	}
	fmt.Println("\nNothing was changed on any server. Details: docs/migration.md")
}
