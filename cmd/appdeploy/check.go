package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"appdeploy/internal/config"
)

// runCheck validates an appdeploy.yml without a server: the parser the agent
// uses decides, so what passes here passes in the validating step of a deploy
// (apart from the server's own limits, such as free memory).
func runCheck(args []string) int {
	file := config.FileName
	explicit := false
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			reportError(fmt.Sprintf("unknown option %q (usage: appdeploy check [file] [--json])", a))
			return 2
		}
		file, explicit = a, true
	}
	data, err := os.ReadFile(file)
	if os.IsNotExist(err) && !explicit {
		if jsonOut {
			emit(map[string]any{"valid": true, "file": file, "present": false, "note": "no file: the defaults apply"})
		} else {
			fmt.Printf("no %s here: the defaults apply, which is fine\n", file)
		}
		return 0
	}
	if err != nil {
		reportError(err.Error())
		return 1
	}
	cfg, err := config.Parse(data)
	if err != nil {
		if jsonOut {
			emit(map[string]any{"valid": false, "file": file, "error": err.Error()})
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
		return 1
	}
	if jsonOut {
		var resolved any
		_ = json.Unmarshal(cfg.JSON(), &resolved)
		warnings := cfg.Warnings()
		if warnings == nil {
			warnings = []string{}
		}
		emit(map[string]any{"valid": true, "file": file, "present": true, "config": resolved, "warnings": warnings})
		return 0
	}
	fmt.Printf("%s is valid.\n", file)
	for _, w := range cfg.Warnings() {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
	names := make([]string, 0, len(cfg.Roles))
	for n := range cfg.Roles {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		r := cfg.Roles[n]
		line := fmt.Sprintf("  %-8s %dM, %v CPU, on %v", n, r.Memory.Bytes()>>20, r.CPUs, r.On)
		if n == "web" {
			port := "the port the image EXPOSEs, else 8080"
			if r.Port > 0 {
				port = fmt.Sprint(r.Port)
			}
			line += fmt.Sprintf(", port %s, health GET %s (up to %s to start)", port, r.Healthcheck.Path, r.Healthcheck.StartPeriod.D())
		}
		if r.Cmd != "" {
			line += fmt.Sprintf(", cmd %q", r.Cmd)
		}
		if len(r.Volumes) > 0 {
			line += fmt.Sprintf(", volumes %s", strings.Join(r.Volumes, " "))
		}
		fmt.Println(line)
	}
	if cfg.ReleaseCommand != "" {
		fmt.Printf("  release command: %s\n", cfg.ReleaseCommand)
	}
	if len(cfg.Env) > 0 {
		keys := make([]string, 0, len(cfg.Env))
		for k := range cfg.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Printf("  env: %s\n", strings.Join(keys, ", "))
	}
	var sv []string
	if cfg.Services.Postgres {
		sv = append(sv, "postgres")
	}
	if cfg.Services.Redis {
		sv = append(sv, "redis")
	}
	if len(sv) > 0 {
		fmt.Printf("  services: %s\n", strings.Join(sv, ", "))
	}
	fmt.Printf("  watch: %s, roll back above %s 5xx (at least %d requests) or after %d restarts\n",
		cfg.Watch.Window.D(), cfg.Watch.ErrorRate, cfg.Watch.MinRequests, cfg.Watch.MaxRestarts)
	return 0
}
