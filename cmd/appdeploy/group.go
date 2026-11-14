package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"appdeploy/internal/api"
	"appdeploy/internal/group"
)

// runGroupOffline handles `group init` and `group join`, which prepare a
// server's identity before its agent starts.
func runGroupOffline(args []string) int {
	fs := flag.NewFlagSet("group "+args[0], flag.ContinueOnError)
	dataDir := fs.String("data-dir", envOr("APPDEPLOY_DATA_DIR", defaultDataDir), "state directory of the agent")
	name := fs.String("server", "", "name of this server (unique in the group)")
	advertise := fs.String("advertise", "", "host:port other servers use to reach this server's peer API")
	labels := fs.String("labels", "web", "comma-separated labels of this server")
	groupName := fs.String("group", "default", "name of the group (init only)")
	tok := fs.String("token", "", "invite token (join only)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *name == "" || *advertise == "" {
		return die("--server and --advertise are required")
	}
	var ls []string
	for _, l := range strings.Split(*labels, ",") {
		if l = strings.TrimSpace(l); l != "" {
			ls = append(ls, l)
		}
	}
	dir := filepath.Join(*dataDir, "group")
	switch args[0] {
	case "init":
		if _, err := group.Init(dir, *groupName, *name, *advertise, ls); err != nil {
			return die("%v", err)
		}
		fmt.Printf("group %q created with %s as its founder.\nRestart the agent, then invite servers with: appdeploy group invite\n", *groupName, *name)
	case "join":
		if *tok == "" {
			return die("--token is required")
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		g, err := group.Join(ctx, dir, *tok, *name, *advertise, ls)
		if err != nil {
			return die("%v", err)
		}
		fmt.Printf("%s joined group %q (%d servers).\nStart (or restart) the agent to take part.\n", *name, g.State().Name, len(g.State().Servers))
	}
	return 0
}

func cmdGroup(ctx context.Context, c *api.Client, args []string) (int, error) {
	if len(args) == 0 {
		return 2, fmt.Errorf("usage: group invite|servers|remove <name>")
	}
	switch args[0] {
	case "invite":
		tok, err := c.GroupInvite(ctx)
		if err != nil {
			return 1, err
		}
		fmt.Println("On the new server, run (valid for one hour, once):")
		fmt.Printf("  appdeploy group join --server <name> --advertise <host:7070> --labels web --token %s\n", tok)
	case "servers":
		st, err := c.Group(ctx)
		if err != nil {
			return 1, err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(tw, "SERVER\tADDRESS\tLABELS\n")
		for _, s := range st.Servers {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", s.Name, s.Addr, strings.Join(s.Labels, ","))
		}
		tw.Flush()
	case "remove":
		if len(args) != 2 {
			return 2, fmt.Errorf("usage: group remove <name>")
		}
		if err := c.GroupRemove(ctx, args[1]); err != nil {
			return 1, err
		}
		fmt.Println("removed", args[1])
	default:
		return 2, fmt.Errorf("unknown group command %q", args[0])
	}
	return 0, nil
}
