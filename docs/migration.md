# Migration

There are no automatic importers yet. Moving is a few manual steps, listed here for the common
sources, and [leaving appdeploy](#leaving-appdeploy) is one command plus your data.

The model that makes moves cheap: your app is a **Dockerfile**, **environment variables** and
`GET /up`. Everything else (ports, domains, limits) is a few lines of
[`appdeploy.yml`](configuration.md).

## Before you move: check the fit

| If your app... | Then |
|---|---|
| writes files it must keep (uploads, SQLite) | It does not fit as is: containers have **no persistent volumes**. Move that data to Postgres, object storage, or another service first. |
| is built with buildpacks (Heroku, Dokku, Fly) and has no Dockerfile | Write a Dockerfile. appdeploy never guesses a build. |
| needs more than ~10 servers or autoscaling | Not what appdeploy is for. |
| has background workers or cron | Fine: extra roles (`cmd:`). Run schedules inside a worker process; there is no cron feature. |

A safe way to move any app: set it up on appdeploy **next to** the old deployment (a new server,
or another port), deploy, test with a temporary domain, move the data, then switch DNS.

## From Kamal

| Kamal (`config/deploy.yml`) | appdeploy |
|---|---|
| `service:` | the app name (`apps create <name>`) |
| `image:` and `registry:` | not needed: the server builds the image. To keep building in CI, `deploy --image` pulls from your registry (`docker login` on the server once). |
| `servers:` `web:` hosts | servers labeled `web` (one server needs no labels) |
| `servers:` other roles (`job:` with `cmd:`) | `roles.<name>` with `cmd:` and `on: [label]` |
| `proxy: host / hosts, ssl` | `domains:` (HTTPS is automatic) |
| `proxy: app_port` | `roles.web.port` |
| `proxy: healthcheck: path` | `roles.web.healthcheck.path` |
| `env: clear:` | set as secrets (`secrets set`), or `ENV` lines in the Dockerfile |
| `env: secret:` and `.kamal/secrets` | `appdeploy secrets set <app> NAME=value...` |
| `accessories:` postgres / redis | `services: {postgres: true}` (a database per app, backups) or keep your own and set `DATABASE_URL` |
| `accessories:` anything else | run it yourself next to appdeploy, or keep it on Kamal |
| `volumes:` | not supported (see above) |
| `hooks/pre-deploy` (migrations) | `release_command:` |
| `builder:` options | not needed (build happens on the server) |
| `kamal deploy` | `git push prod main` (or `appdeploy deploy`) |
| `kamal rollback <version>` | `appdeploy rollback <app> [--to N]` |
| `kamal app logs`, `kamal app exec` | `appdeploy logs`, `appdeploy exec` |

Steps: install appdeploy on a **new** server, create the app with its domain(s), `secrets set`
the values from `.kamal/secrets`, push, check with a test request (`curl --resolve
myapp.example.com:443:<new-ip> https://myapp.example.com/up`), copy the database
([below](#moving-a-postgres-database-in)), lower the DNS TTL, switch DNS, and keep Kamal running
until traffic has moved. To do it on the **same** server, Kamal's proxy and appdeploy's Caddy both
want ports 80 and 443: expect a short gap while you stop Kamal's proxy and start appdeploy's.

## From Docker Compose

| Compose | appdeploy |
|---|---|
| a service that serves HTTP | the `web` role; `ports:` becomes `domains:` and `roles.web.port` |
| other services running your image with another `command:` | roles with `cmd:` |
| `environment:`, `env_file:` | `secrets set` (or `ENV` in the Dockerfile for non-secret defaults) |
| `build:` | the Dockerfile (and `build.context`, `build.args`) |
| `mem_limit`, `cpus` | `roles.<name>.memory`, `.cpus` |
| `healthcheck:` | `roles.web.healthcheck` (an HTTP path, probed from outside) |
| `postgres`, `redis` services | `services:` (or keep them elsewhere with `DATABASE_URL`) |
| `volumes:` for your app | not supported |
| a reverse proxy service (Caddy, Traefik, nginx) | not needed: Caddy is built in |

Put the repository's Dockerfile at the root, write `appdeploy.yml` from the table, push.

## From Dokku or Heroku

| Dokku / Heroku | appdeploy |
|---|---|
| `git push dokku main` | `git push prod main` |
| buildpack (`heroku/nodejs`, ...) | a Dockerfile (required) |
| `Procfile` entries (`web:`, `worker:`) | roles: `web` runs the image's default `CMD`; others use `cmd:` |
| `release:` in the Procfile | `release_command:` |
| `config:set KEY=value` | `secrets set` |
| `domains:add` | `apps create --domain` / `domains set` |
| `ps:scale web=2` | `roles.web.replicas_per_server: 2` |
| `CHECKS` / health check settings | `roles.web.healthcheck` |
| `postgres:create`, `redis:create` plugins | `services:` |
| `storage:mount` | not supported |
| Let's Encrypt plugin | built in |

## From Fly.io, Render, Railway

| Hosted platform | appdeploy |
|---|---|
| `fly.toml` `[http_service] internal_port` | `roles.web.port` |
| `[[http_service.checks]] path` | `roles.web.healthcheck.path` |
| `[processes]` | roles (`cmd:`) |
| `[vm] memory` | `roles.<name>.memory` |
| `fly secrets set`, dashboard env vars | `secrets set` |
| `[mounts]` volumes | not supported |
| managed Postgres | keep it and set `DATABASE_URL`, or `services: {postgres: true}` and restore a dump |
| `fly deploy` | `git push prod main` |
| certificates | automatic |

## Moving a Postgres database in

Take a plain SQL dump from the old database that does not depend on its users:

```bash
pg_dump --clean --if-exists --no-owner --no-acl "$OLD_DATABASE_URL" | gzip > import.sql.gz
```

With `services: {postgres: true}` deployed once (so the app's database exists), copy the dump into
the app's backup directory on the server and restore it:

```bash
ssh root@my-server mkdir -p /var/lib/appdeploy/backups/myapp
scp import.sql.gz root@my-server:/var/lib/appdeploy/backups/myapp/
appdeploy --host deploy@my-server db restore myapp import.sql.gz
```

`restore` replaces the tables of the app's database with the dump's, so do it once, before you
switch traffic (or put the app in a quiet state first). The dump is a plain SQL file replayed
with `psql`; appdeploy's own backups are exactly that format, which is what has been tested.
Stop writes to the old database and take a final dump right before the cutover.

## Leaving appdeploy

Everything appdeploy knows can be written for another tool, and nothing appdeploy-specific is
inside your image or code.

**1. Generate the target's files**, from the live app:

```bash
appdeploy --host deploy@my-server export myapp --format kamal   --out ./deploy-kamal
appdeploy --host deploy@my-server export myapp --format compose --out ./deploy-compose
appdeploy --host deploy@my-server export myapp --format k8s     --out ./deploy-k8s
appdeploy --host deploy@my-server export myapp --format fly     --out ./deploy-fly
```

You get roles, ports, limits, health path, domains and the **names** of secrets, with notes where
you need to decide something (a registry name, the database). Values are never in these files.

| Format | Files | Maps |
|---|---|---|
| `kamal` | `config/deploy.yml`, `.kamal/secrets` | roles to Kamal roles with your servers by label, domains to `proxy`, Postgres/Redis to accessories |
| `compose` | `compose.yml`, `.env.example`, `Caddyfile` | a service per role with limits, Postgres/Redis services, Caddy for the domains |
| `k8s` | `k8s.yml` | a Secret template, a Deployment per role, a Service and an Ingress |
| `fly` | `fly.toml`, `secrets.sh` | processes, health check, VM size, a script that sets the secrets |

**2. Get the image into a registry.** appdeploy builds images on the servers and does not push
them. The image of release N is `ad/<app>:vN` on the server. Either build from your repository in
the new system, or push what is running:

```bash
ssh root@my-server
docker tag ad/myapp:v7 registry.example.com/myapp:v7
docker push registry.example.com/myapp:v7
```

The exported files use the placeholder `your-registry.example.com/myapp:vN`; replace it.

**3. Take your secrets and your data:**

```bash
appdeploy --host deploy@my-server secrets export myapp > myapp.env      # contains values: protect it
appdeploy --host deploy@my-server db backup myapp                        # then copy it off the server
scp root@my-server:/var/lib/appdeploy/backups/myapp/<name>.sql.gz .
gunzip -c <name>.sql.gz | psql "$NEW_DATABASE_URL"
```

Redis data is not backed up by appdeploy; if you use it for more than a cache, take a dump from
the Redis container yourself.

**4. Take the source:** the repository on the server is an ordinary bare git repository:
`git clone deploy@my-server:myapp` works, and `git remote` on your side is all you need.

**5. Switch DNS** once the new system serves the app, then run `appdeploy apps destroy myapp`
(keeping the database until you are sure) and [uninstall](operations.md#uninstalling).

**What does not carry over:** deploy history and the journal, TLS certificates (the new system
requests its own), and anything your app kept on the server outside Postgres (it could not keep
anything, which is why leaving is easy).
