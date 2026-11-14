# Migration

There is an importer for Kamal (`appdeploy import kamal`); for other sources moving is a few
manual steps, listed here, and [leaving appdeploy](#leaving-appdeploy) is one command plus your data.

The model that makes moves cheap: your app is a **Dockerfile**, **environment variables** and
`GET /up`. Everything else (ports, domains, limits) is a few lines of
[`appdeploy.yml`](configuration.md).

## Before you move: check the fit

| If your app... | Then |
|---|---|
| writes files it must keep (uploads, SQLite) | Mount a named volume (`roles.<name>.volumes`) and copy the data in ([moving a volume](#moving-a-volume)). Volumes are not backed up by appdeploy; for important data prefer Postgres or object storage. |
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
| `env: clear:` | the `env:` section of `appdeploy.yml` |
| `env: secret:` and `.kamal/secrets` | `appdeploy secrets set <app> NAME=value...` |
| `accessories:` postgres / redis | `services: {postgres: true}` (a database per app, backups) or keep your own and set `DATABASE_URL` |
| `accessories:` anything else | run it yourself next to appdeploy, or keep it on Kamal |
| `volumes:` (named) | `roles.<name>.volumes` (see [moving a volume](#moving-a-volume)); a host directory is not supported |
| `hooks/pre-deploy` (migrations) | `release_command:` |
| `builder:` options | not needed (build happens on the server) |
| `kamal deploy` | `git push prod main` (or `appdeploy deploy`) |
| `kamal rollback <version>` | `appdeploy rollback <app> [--to N]` |
| `kamal app logs`, `kamal app exec` | `appdeploy logs`, `appdeploy exec` |

**Let the tool do the reading first.** In your project directory, on your computer:

```bash
appdeploy import kamal                 # reads config/deploy.yml and .kamal/secrets
appdeploy import kamal --out appdeploy.yml
```

It prints the `appdeploy.yml` for the app, the commands to create it and set its secrets, and
three lists: **blockers** (the app cannot run on appdeploy as it is: host directory mounts, accessories other
than Postgres and Redis), **not carried over**, and **things to decide**. It changes nothing on any
server and does not read secret values. Solve the blockers before you go on; the tables below explain
each mapping, and `appdeploy check appdeploy.yml` validates the result.

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
| `environment:`, `env_file:` | `env:` in `appdeploy.yml` for plain values, `secrets set` for secrets |
| `build:` | the Dockerfile (and `build.context`, `build.args`) |
| `mem_limit`, `cpus` | `roles.<name>.memory`, `.cpus` |
| `healthcheck:` | `roles.web.healthcheck` (an HTTP path, probed from outside) |
| `postgres`, `redis` services | `services:` (or keep them elsewhere with `DATABASE_URL`) |
| named `volumes:` for your app | `roles.<name>.volumes`; bind mounts are not supported |
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
| `[mounts]` volumes | `roles.<name>.volumes` (copy the data in: [moving a volume](#moving-a-volume)) |
| managed Postgres | keep it and set `DATABASE_URL`, or `services: {postgres: true}` and restore a dump |
| `fly deploy` | `git push prod main` |
| certificates | automatic |

## Moving a volume

appdeploy creates the volume `ad-<app>-<name>` on the first deploy (`ad` is the agent's
`--instance`). Data from an old volume is copied into it **before the first deploy**, with the old
app stopped so nothing is written meanwhile. Create the new volume with the labels that make
`apps destroy` know it, copy, and check:

```bash
APP=myapp; NAME=data; OLD=myapp_data      # your app, the volume name in appdeploy.yml, the old volume
docker volume create --label appdeploy.instance=ad --label appdeploy.app=$APP --label appdeploy.volume=$NAME ad-$APP-$NAME
docker run --rm -v $OLD:/from:ro -v ad-$APP-$NAME:/to alpine cp -a /from/. /to/
docker run --rm -v ad-$APP-$NAME:/v:ro alpine ls -la /v
```

For a SQLite database, stop the old app first (or copy with `sqlite3 old.db ".backup new.db"`) so
that the copy is consistent. For the files of a host directory, mount it as `/from` instead of the volume.

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
requests its own), and the contents of your app's volumes: `appdeploy export` writes the volume
mounts into the new tool's file, but you copy the data yourself (`docker run --rm -v ad-APP-NAME:/from:ro
-v $PWD:/to alpine tar -C /from -cf /to/data.tar .` makes an archive).
