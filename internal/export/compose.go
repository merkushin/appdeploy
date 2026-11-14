package export

import (
	"bytes"
	"fmt"
	"strings"
)

func compose(s Spec) map[string]string {
	c := s.Config
	var b bytes.Buffer
	header(&b, "Docker Compose", s)
	b.WriteString("name: " + q(s.App) + "\nservices:\n")
	for _, name := range sortedRoles(c) {
		r := c.Roles[name]
		fmt.Fprintf(&b, "  %s:\n    image: %s\n    restart: unless-stopped\n    env_file: .env\n", name, q(image(s)))
		if name == "web" {
			fmt.Fprintf(&b, "    environment:\n      PORT: %q\n", fmt.Sprint(webPort(c)))
		}
		if r.Cmd != "" {
			b.WriteString("    command:\n")
			for _, a := range args(r.Cmd) {
				fmt.Fprintf(&b, "      - %s\n", q(a))
			}
		}
		fmt.Fprintf(&b, "    mem_limit: %s\n    cpus: %v\n", strings.ToLower(mem(r)), r.CPUs)
		if name == "web" {
			if len(s.Domains) > 0 {
				fmt.Fprintf(&b, "    expose:\n      - %q\n", fmt.Sprint(webPort(c)))
			} else {
				fmt.Fprintf(&b, "    ports:\n      - %q\n", fmt.Sprintf("80:%d", webPort(c)))
			}
		}
		var deps []string
		if c.Services.Postgres {
			deps = append(deps, "postgres")
		}
		if c.Services.Redis {
			deps = append(deps, "redis")
		}
		if len(deps) > 0 {
			b.WriteString("    depends_on:\n")
			for _, d := range deps {
				fmt.Fprintf(&b, "      - %s\n", d)
			}
		}
	}
	if c.Services.Postgres {
		b.WriteString("  postgres:\n    image: postgres:17-alpine\n    restart: unless-stopped\n    environment:\n      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:?set POSTGRES_PASSWORD in .env}\n      POSTGRES_USER: app\n      POSTGRES_DB: app\n    volumes:\n      - postgres-data:/var/lib/postgresql/data\n")
	}
	if c.Services.Redis {
		b.WriteString("  redis:\n    image: redis:7-alpine\n    restart: unless-stopped\n    volumes:\n      - redis-data:/data\n")
	}
	if len(s.Domains) > 0 {
		b.WriteString("  caddy:\n    image: caddy:2\n    restart: unless-stopped\n    ports:\n      - \"80:80\"\n      - \"443:443\"\n    volumes:\n      - ./Caddyfile:/etc/caddy/Caddyfile:ro\n      - caddy-data:/data\n    depends_on:\n      - web\n")
	}
	var vols []string
	if c.Services.Postgres {
		vols = append(vols, "postgres-data")
	}
	if c.Services.Redis {
		vols = append(vols, "redis-data")
	}
	if len(s.Domains) > 0 {
		vols = append(vols, "caddy-data")
	}
	if len(vols) > 0 {
		b.WriteString("volumes:\n")
		for _, v := range vols {
			fmt.Fprintf(&b, "  %s: {}\n", v)
		}
	}

	var env bytes.Buffer
	env.WriteString("# Fill in the values. Do not commit this file.\n")
	for _, n := range allEnvNames(s) {
		fmt.Fprintf(&env, "%s=\n", n)
	}
	if c.Services.Postgres {
		env.WriteString("POSTGRES_PASSWORD=\n# DATABASE_URL=postgres://app:<POSTGRES_PASSWORD>@postgres:5432/app\n")
	}
	if c.Services.Redis {
		env.WriteString("# REDIS_URL=redis://redis:6379/0\n")
	}
	files := map[string]string{"compose.yml": b.String(), ".env.example": env.String()}
	if len(s.Domains) > 0 {
		var cf bytes.Buffer
		fmt.Fprintf(&cf, "%s {\n\treverse_proxy web:%d\n}\n", strings.Join(s.Domains, ", "), webPort(c))
		files["Caddyfile"] = cf.String()
	}
	return files
}
