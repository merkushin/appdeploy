package export

import (
	"bytes"
	"fmt"
	"strings"
)

func kamal(s Spec) map[string]string {
	c := s.Config
	var b bytes.Buffer
	header(&b, "Kamal", s)
	fmt.Fprintf(&b, "service: %s\nimage: %s\n", q(s.App), q("your-registry-user/"+s.App))
	b.WriteString("\nregistry:\n  # server: ghcr.io   # omit for Docker Hub\n  username: your-registry-user\n  password:\n    - KAMAL_REGISTRY_PASSWORD\n")
	b.WriteString("\nservers:\n")
	for _, name := range sortedRoles(c) {
		r := c.Roles[name]
		fmt.Fprintf(&b, "  %s:\n    hosts:\n", name)
		for _, h := range hosts(s, r) {
			fmt.Fprintf(&b, "      - %s\n", h)
		}
		if name != "web" && r.Cmd != "" {
			fmt.Fprintf(&b, "    cmd: %s\n", q(r.Cmd))
		}
	}
	if web := c.Roles["web"]; web != nil && len(s.Domains) > 0 {
		b.WriteString("\nproxy:\n  ssl: true\n")
		if len(s.Domains) == 1 {
			fmt.Fprintf(&b, "  host: %s\n", q(s.Domains[0]))
		} else {
			fmt.Fprintf(&b, "  hosts:\n")
			for _, d := range s.Domains {
				fmt.Fprintf(&b, "    - %s\n", q(d))
			}
		}
		fmt.Fprintf(&b, "  app_port: %d\n  healthcheck:\n    path: %s\n", webPort(c), q(web.Healthcheck.Path))
	}
	if names := allEnvNames(s); len(names) > 0 {
		b.WriteString("\nenv:\n  secret:\n")
		for _, n := range names {
			fmt.Fprintf(&b, "    - %s\n", n)
		}
	}
	if c.Services.Postgres || c.Services.Redis {
		b.WriteString("\n# appdeploy ran these for you; Kamal runs them as accessories. Set DATABASE_URL / REDIS_URL\n# to point at them (host: <service>-accessory name) in .kamal/secrets.\naccessories:\n")
		if c.Services.Postgres {
			fmt.Fprintf(&b, "  postgres:\n    image: postgres:17-alpine\n    host: %s\n    port: \"127.0.0.1:5432:5432\"\n    env:\n      secret:\n        - POSTGRES_PASSWORD\n    directories:\n      - data:/var/lib/postgresql/data\n", hosts(s, c.Roles[sortedRoles(c)[0]])[0])
		}
		if c.Services.Redis {
			fmt.Fprintf(&b, "  redis:\n    image: redis:7-alpine\n    host: %s\n    port: \"127.0.0.1:6379:6379\"\n    directories:\n      - data:/data\n", hosts(s, c.Roles[sortedRoles(c)[0]])[0])
		}
	}
	var sec bytes.Buffer
	sec.WriteString("# Kamal reads these from your shell environment or a password manager; no values here.\nKAMAL_REGISTRY_PASSWORD=$KAMAL_REGISTRY_PASSWORD\n")
	for _, n := range allEnvNames(s) {
		fmt.Fprintf(&sec, "%s=$%s\n", n, n)
	}
	if c.Services.Postgres {
		sec.WriteString("POSTGRES_PASSWORD=$POSTGRES_PASSWORD\n")
	}
	return map[string]string{"config/deploy.yml": b.String(), ".kamal/secrets": sec.String()}
}

var _ = strings.TrimSpace
