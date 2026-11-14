package export

import (
	"bytes"
	"fmt"
	"strings"
)

func fly(s Spec) map[string]string {
	c := s.Config
	var b bytes.Buffer
	header(&b, "Fly.io", s)
	fmt.Fprintf(&b, "app = %s\n# primary_region = \"ams\"\n\n[build]\n  image = %s\n\n[env]\n  PORT = %q\n", q(s.App), q(image(s)), fmt.Sprint(webPort(c)))
	roles := sortedRoles(c)
	if len(roles) > 1 {
		b.WriteString("\n[processes]\n")
		for _, name := range roles {
			fmt.Fprintf(&b, "  %s = %s\n", name, q(c.Roles[name].Cmd))
		}
	}
	if web := c.Roles["web"]; web != nil {
		b.WriteString("\n[http_service]\n")
		fmt.Fprintf(&b, "  internal_port = %d\n  force_https = true\n  auto_stop_machines = \"off\"\n  min_machines_running = %d\n", webPort(c), web.ReplicasPerSrv*len(hosts(s, web)))
		if len(roles) > 1 {
			b.WriteString("  processes = [\"web\"]\n")
		}
		fmt.Fprintf(&b, "\n  [[http_service.checks]]\n    method = \"GET\"\n    path = %s\n    interval = \"15s\"\n    timeout = %q\n    grace_period = %q\n",
			q(web.Healthcheck.Path), web.Healthcheck.Timeout.D().String(), web.Healthcheck.StartPeriod.D().String())
	}
	for _, name := range roles {
		r := c.Roles[name]
		fmt.Fprintf(&b, "\n[[vm]]\n  memory = %s\n  cpus = %d\n", q(strings.ToLower(mem(r))+"b"), max(1, int(r.CPUs)))
		if len(roles) > 1 {
			fmt.Fprintf(&b, "  processes = [%s]\n", q(name))
		}
	}
	if len(s.Domains) > 0 {
		b.WriteString("\n# Add your domains after the first deploy:\n")
		for _, d := range s.Domains {
			fmt.Fprintf(&b, "#   fly certs add %s\n", d)
		}
	}
	var sec bytes.Buffer
	sec.WriteString("#!/bin/sh\n# Sets the secrets of the app on Fly. Fill the values in first; do not commit them.\nset -eu\nfly secrets set --app " + s.App + " \\\n")
	for _, n := range allEnvNames(s) {
		fmt.Fprintf(&sec, "  %s=\"${%s:?set %s}\" \\\n", n, n, n)
	}
	sec.WriteString("  --stage\n")
	if c.Services.Postgres {
		sec.WriteString("# This app used appdeploy's Postgres: use Fly Postgres or another database for DATABASE_URL.\n")
	}
	return map[string]string{"fly.toml": b.String(), "secrets.sh": sec.String()}
}
