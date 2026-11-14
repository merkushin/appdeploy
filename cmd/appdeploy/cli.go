package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"appdeploy/internal/api"
)

// parseFlags parses flags that may appear before, between or after
// positional arguments, and returns the positionals.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		// "--" ends flag parsing: everything after it is positional.
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
		if len(args) > 0 && args[0] == "--" {
			return append(pos, args[1:]...), nil
		}
	}
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func runClient(args []string) int {
	// Global flags come first.
	host, socket := "", envOr("APPDEPLOY_SOCKET", defaultSocket)
	for len(args) > 0 {
		switch {
		case args[0] == "--host" && len(args) > 1:
			host, args = args[1], args[2:]
			continue
		case args[0] == "--socket" && len(args) > 1:
			socket, args = args[1], args[2:]
			continue
		case strings.HasPrefix(args[0], "--host="):
			host, args = strings.TrimPrefix(args[0], "--host="), args[1:]
			continue
		case strings.HasPrefix(args[0], "--socket="):
			socket, args = strings.TrimPrefix(args[0], "--socket="), args[1:]
			continue
		}
		break
	}
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	if args[0] != "ping" || host != "" {
		wd, _ := os.Getwd()
		h, source, err := resolveHost(host, wd)
		if err != nil {
			return die("%v", err)
		}
		if source != "" && !jsonOut {
			fmt.Fprintf(os.Stderr, "appdeploy: using the server %s (from %s)\n", h, source)
		}
		host = h
	}
	if host != "" {
		if jsonOut {
			args = append([]string{"--json"}, args...)
		}
		return execSSH(host, args)
	}
	return clientRun(socket, args)
}

// clientRun runs a client command against the local agent socket.
func clientRun(socket string, args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	c := api.NewClient(socket)
	cmd, rest := args[0], args[1:]
	var err error
	code := 0
	switch cmd {
	case "apps":
		code, err = cmdApps(ctx, c, rest)
	case "status":
		code, err = cmdStatus(ctx, c, rest)
	case "deploy":
		code, err = cmdDeploy(ctx, c, "deploy", rest)
	case "redeploy":
		code, err = cmdDeploy(ctx, c, "redeploy", rest)
	case "rollback":
		code, err = cmdDeploy(ctx, c, "rollback", rest)
	case "releases":
		code, err = cmdReleases(ctx, c, rest)
	case "logs":
		code, err = cmdLogs(ctx, c, rest)
	case "exec":
		code, err = cmdExec(ctx, c, rest)
	case "secrets":
		code, err = cmdSecrets(ctx, c, rest)
	case "group":
		code, err = cmdGroup(ctx, c, rest)
	case "self-update":
		code, err = cmdSelfUpdate(ctx, c, rest)
	case "export":
		code, err = cmdExport(ctx, c, rest)
	case "db":
		code, err = cmdDB(ctx, c, rest)
	case "domains":
		code, err = cmdDomains(ctx, c, rest)
	case "ping":
		if err = c.Ping(ctx); err == nil && jsonOut {
			code, err = emit(map[string]any{"ok": true})
		}
	default:
		fmt.Fprintf(os.Stderr, "appdeploy: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	if err != nil {
		if err != errReported {
			die("%v", err)
		}
		if code == 2 {
			return 2 // wrong usage
		}
		return 1
	}
	return code
}

func cmdApps(ctx context.Context, c *api.Client, args []string) (int, error) {
	if len(args) == 0 {
		return 2, fmt.Errorf("usage: apps create|list|destroy")
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("apps create", flag.ContinueOnError)
		var domains stringList
		fs.Var(&domains, "domain", "domain served by the app (repeatable)")
		branch := fs.String("branch", "main", "branch that deploys")
		pos, err := parseFlags(fs, args[1:])
		if err != nil || len(pos) != 1 {
			return 2, fmt.Errorf("usage: apps create <app> [--domain d]... [--branch main]")
		}
		a, err := c.CreateApp(ctx, api.CreateAppRequest{Name: pos[0], Domains: domains, Branch: *branch})
		if err != nil {
			return 1, err
		}
		if jsonOut {
			return emit(a)
		}
		fmt.Printf("created app %s (deploy branch %s)\n", a.Name, a.Branch)
		fmt.Printf("  git remote add prod <user>@<server>:%s\n  git push prod %s\n", a.Name, a.Branch)
	case "list":
		apps, err := c.ListApps(ctx)
		if err != nil {
			return 1, err
		}
		if jsonOut {
			if apps == nil {
				apps = []api.AppInfo{}
			}
			return emit(apps)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "APP\tRELEASE\tDOMAINS")
		for _, a := range apps {
			rel := "-"
			if a.CurrentRelease > 0 {
				rel = "v" + strconv.Itoa(a.CurrentRelease)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", a.Name, rel, strings.Join(a.Domains, ","))
		}
		tw.Flush()
	case "destroy":
		fs := flag.NewFlagSet("apps destroy", flag.ContinueOnError)
		del := fs.Bool("delete-volumes", false, "also delete the app's volumes: its data is gone for good")
		pos, err := parseFlags(fs, args[1:])
		if err != nil || len(pos) != 1 {
			return 2, fmt.Errorf("usage: apps destroy <app> [--delete-volumes]")
		}
		kept, err := c.DeleteApp(ctx, pos[0], *del)
		if err != nil {
			return 1, err
		}
		if jsonOut {
			return emit(map[string]any{"ok": true, "kept_volumes": kept})
		}
		fmt.Printf("destroyed app %s\n", pos[0])
		if len(kept) > 0 {
			fmt.Printf("its data is kept in the volume(s): %s\n(delete them with `docker volume rm` on the server, or destroy with --delete-volumes next time)\n", strings.Join(kept, ", "))
		}
	default:
		return 2, fmt.Errorf("unknown apps command %q", args[0])
	}
	return 0, nil
}

func cmdStatus(ctx context.Context, c *api.Client, args []string) (int, error) {
	if len(args) == 0 {
		return cmdApps(ctx, c, []string{"list"})
	}
	s, err := c.Status(ctx, args[0])
	if err != nil {
		return 1, err
	}
	if jsonOut {
		return emit(s)
	}
	rel := "none"
	if s.App.CurrentRelease > 0 {
		rel = "v" + strconv.Itoa(s.App.CurrentRelease)
	}
	fmt.Printf("%s  release %s  domains: %s\n\n", s.App.Name, rel, strings.Join(s.App.Domains, ", "))
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "CONTAINER\tROLE\tRELEASE\tSTATE")
	for _, ct := range s.Containers {
		state := "stopped"
		if ct.Running {
			state = "running"
		}
		if ct.Restarts > 0 {
			state += fmt.Sprintf(" (%d restarts)", ct.Restarts)
		}
		fmt.Fprintf(tw, "%s\t%s\tv%d\t%s\n", ct.Name, ct.Role, ct.Release, state)
	}
	tw.Flush()
	fmt.Println("\nRecent deploys:")
	tw = tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	for _, d := range s.Deploys {
		line := fmt.Sprintf("%s\tv%d\t%s\t%s", d.StartedAt.Format("2006-01-02 15:04:05"), d.Release, d.Source, d.State)
		if d.Error != "" {
			line += "\t" + d.Error
		}
		fmt.Fprintln(tw, line)
	}
	tw.Flush()
	return 0, nil
}

func cmdReleases(ctx context.Context, c *api.Client, args []string) (int, error) {
	if len(args) != 1 {
		return 2, fmt.Errorf("usage: releases <app>")
	}
	rels, err := c.Releases(ctx, args[0])
	if err != nil {
		return 1, err
	}
	if jsonOut {
		if rels == nil {
			rels = []api.ReleaseInfo{}
		}
		return emit(rels)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "RELEASE\tCOMMIT\tCREATED\t")
	for _, r := range rels {
		cur := ""
		if r.Current {
			cur = "<- current"
		}
		commit := r.Commit
		if len(commit) > 8 {
			commit = commit[:8]
		}
		fmt.Fprintf(tw, "v%d\t%s\t%s\t%s\n", r.Number, commit, r.CreatedAt.Format(time.DateTime), cur)
	}
	tw.Flush()
	return 0, nil
}

// cmdDeploy handles deploy, redeploy and rollback, which all return a deploy to follow.
func cmdDeploy(ctx context.Context, c *api.Client, kind string, args []string) (int, error) {
	fs := flag.NewFlagSet(kind, flag.ContinueOnError)
	ref := fs.String("ref", "", "git ref or commit (deploy only)")
	to := fs.Int("to", 0, "release number (rollback only; default: the previous one)")
	image := fs.String("image", "", "deploy this prebuilt image instead of building (deploy only)")
	cfgFile := fs.String("config", "", "appdeploy.yml to use with --image, or - to read it from standard input (default: the live release's config)")
	detach := fs.Bool("detach", false, "do not wait for the deploy to finish")
	pos, err := parseFlags(fs, args)
	if err != nil || len(pos) != 1 {
		return 2, fmt.Errorf("usage: %s <app> [--detach]", kind)
	}
	var resp api.DeployResponse
	switch kind {
	case "deploy":
		if *image != "" {
			yaml := ""
			if *cfgFile != "" {
				var b []byte
				var rerr error
				if *cfgFile == "-" { // from standard input: the way to send a file from CI with --host
					b, rerr = io.ReadAll(os.Stdin)
				} else {
					b, rerr = os.ReadFile(*cfgFile)
				}
				if rerr != nil {
					return 1, rerr
				}
				yaml = string(b)
			}
			resp, err = c.DeployImage(ctx, pos[0], *image, yaml)
		} else {
			resp, err = c.Deploy(ctx, pos[0], *ref)
		}
	case "redeploy":
		resp, err = c.Redeploy(ctx, pos[0])
	case "rollback":
		resp, err = c.Rollback(ctx, pos[0], *to)
	}
	if err != nil {
		return 1, err
	}
	return follow(ctx, c, resp.DeployID, *detach)
}

func follow(ctx context.Context, c *api.Client, id string, detach bool) (int, error) {
	if detach {
		if jsonOut {
			return emit(map[string]any{"deploy_id": id})
		}
		fmt.Println(id)
		return 0, nil
	}
	var state, errMsg string
	var err error
	if jsonOut {
		state, errMsg, err = c.StreamEventsJSON(ctx, id, os.Stdout)
	} else {
		state, errMsg, err = c.StreamEvents(ctx, id, os.Stdout)
	}
	if err != nil {
		return 1, fmt.Errorf("%v (the deploy continues on the server: deploy id %s)", err, id)
	}
	if state != "succeeded" {
		if jsonOut {
			return 1, errReported // the final line of the stream already says so
		}
		return 1, fmt.Errorf("deploy %s: %s", state, errMsg)
	}
	return 0, nil
}

func cmdLogs(ctx context.Context, c *api.Client, args []string) (int, error) {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	role := fs.String("role", "", "only this role")
	f := fs.Bool("f", false, "follow")
	tail := fs.Int("tail", 100, "number of lines per container")
	pos, err := parseFlags(fs, args)
	if err != nil || len(pos) != 1 {
		return 2, fmt.Errorf("usage: logs <app> [--role web] [-f] [--tail N]")
	}
	return 0, c.Logs(ctx, pos[0], *role, *f, *tail, os.Stdout)
}

func cmdExec(ctx context.Context, c *api.Client, args []string) (int, error) {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	role := fs.String("role", "web", "role whose container runs the command")
	pos, err := parseFlags(fs, args)
	if err != nil || len(pos) < 2 {
		return 2, fmt.Errorf("usage: exec <app> [--role web] -- <cmd...>")
	}
	return c.Exec(ctx, pos[0], *role, pos[1:], os.Stdout)
}

func cmdSecrets(ctx context.Context, c *api.Client, args []string) (int, error) {
	if len(args) < 2 {
		return 2, fmt.Errorf("usage: secrets set|unset|list|export <app> ...")
	}
	sub := args[0]
	fs := flag.NewFlagSet("secrets "+sub, flag.ContinueOnError)
	noDeploy := fs.Bool("no-deploy", false, "store the change without rolling it out")
	pos, err := parseFlags(fs, args[1:])
	if err != nil || len(pos) == 0 {
		return 2, fmt.Errorf("usage: secrets %s <app> ...", sub)
	}
	app, items := pos[0], pos[1:]
	switch sub {
	case "set", "unset":
		req := api.SetSecretsRequest{Deploy: !*noDeploy}
		if sub == "set" {
			req.Set = map[string]string{}
			for _, kv := range items {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					return 2, fmt.Errorf("%q is not KEY=value", kv)
				}
				req.Set[k] = v
			}
		} else {
			req.Unset = items
		}
		if len(items) == 0 {
			return 2, fmt.Errorf("nothing to %s", sub)
		}
		resp, err := c.SetSecrets(ctx, app, req)
		if err != nil {
			return 1, err
		}
		if resp.DeployID == "" {
			if jsonOut {
				return emit(map[string]any{"ok": true, "deployed": false})
			}
			fmt.Println("secrets updated (not deployed yet: they apply to the next release)")
			return 0, nil
		}
		return follow(ctx, c, resp.DeployID, false)
	case "list":
		names, err := c.SecretNames(ctx, app)
		if err != nil {
			return 1, err
		}
		if jsonOut {
			if names == nil {
				names = []string{}
			}
			return emit(map[string]any{"names": names})
		}
		for _, n := range names {
			fmt.Println(n)
		}
	case "export":
		vals, err := c.SecretValues(ctx, app)
		if err != nil {
			return 1, err
		}
		keys := make([]string, 0, len(vals))
		for k := range vals {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("%s=%s\n", k, vals[k])
		}
	default:
		return 2, fmt.Errorf("unknown secrets command %q", sub)
	}
	return 0, nil
}

func cmdDomains(ctx context.Context, c *api.Client, args []string) (int, error) {
	if len(args) < 2 || args[0] != "set" {
		return 2, fmt.Errorf("usage: domains set <app> <domain>...")
	}
	if err := c.SetDomains(ctx, args[1], args[2:]); err != nil {
		return 1, err
	}
	if jsonOut {
		return emit(map[string]any{"ok": true, "domains": args[2:]})
	}
	fmt.Printf("domains of %s: %s\n", args[1], strings.Join(args[2:], ", "))
	return 0, nil
}

func cmdDB(ctx context.Context, c *api.Client, args []string) (int, error) {
	if len(args) < 2 {
		return 2, fmt.Errorf("usage: db backup|list|restore <app> [file]")
	}
	app := args[1]
	switch args[0] {
	case "backup":
		path, err := c.DBBackup(ctx, app)
		if err != nil {
			return 1, err
		}
		if jsonOut {
			return emit(map[string]any{"path": path})
		}
		fmt.Println("backup written:", path)
	case "list":
		names, err := c.DBBackups(ctx, app)
		if err != nil {
			return 1, err
		}
		if jsonOut {
			if names == nil {
				names = []string{}
			}
			return emit(names)
		}
		for _, n := range names {
			fmt.Println(n)
		}
	case "restore":
		if len(args) != 3 {
			return 2, fmt.Errorf("usage: db restore <app> <backup name from `db list`>")
		}
		if err := c.DBRestore(ctx, app, args[2]); err != nil {
			return 1, err
		}
		if jsonOut {
			return emit(map[string]any{"ok": true})
		}
		fmt.Println("restored", args[2])
	default:
		return 2, fmt.Errorf("unknown db command %q", args[0])
	}
	return 0, nil
}

func cmdExport(ctx context.Context, c *api.Client, args []string) (int, error) {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	format := fs.String("format", "", "kamal, compose, k8s or fly")
	out := fs.String("out", "", "write the files into this directory instead of printing them")
	pos, err := parseFlags(fs, args)
	if err != nil || len(pos) != 1 || *format == "" {
		return 2, fmt.Errorf("usage: export <app> --format kamal|compose|k8s|fly [--out dir]")
	}
	files, err := c.Export(ctx, pos[0], *format)
	if err != nil {
		return 1, err
	}
	if jsonOut && *out == "" {
		return emit(map[string]any{"files": files})
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if *out == "" {
			fmt.Printf("==> %s <==\n%s\n", n, files[n])
			continue
		}
		path := filepath.Join(*out, n)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return 1, err
		}
		mode := os.FileMode(0o644)
		switch {
		case strings.HasSuffix(n, ".sh"):
			mode = 0o755
		case strings.Contains(n, "secrets") || strings.HasSuffix(n, ".env.example"):
			mode = 0o600
		}
		if err := os.WriteFile(path, []byte(files[n]), mode); err != nil {
			return 1, err
		}
		fmt.Println("wrote", path)
	}
	return 0, nil
}

func cmdSelfUpdate(ctx context.Context, c *api.Client, args []string) (int, error) {
	fs := flag.NewFlagSet("self-update", flag.ContinueOnError)
	check := fs.Bool("check", false, "only report whether a newer release exists")
	force := fs.Bool("force", false, "install even if it is not newer, and even while deploys are running")
	repo := fs.String("repo", "", "owner/name of the GitHub repository (default: the one this build was made for)")
	if _, err := parseFlags(fs, args); err != nil {
		return 2, err
	}
	res, err := c.SelfUpdate(ctx, *check, *force, *repo)
	if jsonOut {
		if err != nil {
			res.Error = err.Error()
			emit(res)
			return 1, errReported
		}
		return emit(res)
	}
	for _, l := range res.Log {
		fmt.Println(l)
	}
	if err != nil {
		return 1, err
	}
	if res.Updated {
		fmt.Printf("updated %s -> %s; the agent restarts now (deploys in progress resume)\n", res.From, res.To)
	}
	return 0, nil
}
