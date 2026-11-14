# Configuration

There are three places to configure things:

1. [`appdeploy.yml`](#appdeployyml) in your repository: how an app is built and run.
2. Commands: secrets, domains, apps ([Commands](commands.md)).
3. [Agent flags](#agent-flags): how a server's agent behaves; set once when installing.

## appdeploy.yml

An optional file at the root of your repository. It is read **from the commit being deployed**,
so a config change is an ordinary commit. Unknown keys are an error (a typo cannot be silently
ignored). With no file, all defaults apply: one `web` role.

```yaml appdeploy.yml
app: shop                    # informational; a warning if it differs from the app's name
branch: main                 # the branch that deploys when pushed

build:
  dockerfile: Dockerfile     # relative to the build context
  context: .                 # relative to the repository root
  args: {}                   # --build-arg values: not for secrets, they end up in image history

domains: [shop.example.com]  # in addition to the ones set with `domains set`

roles:
  web:
    port: 3000
    memory: 512M
    cpus: 1
    healthcheck: {path: /up, timeout: 5s, start_period: 60s}
    smoke: [/, /pricing]
    stop_timeout: 30s
  worker:
    cmd: worker
    memory: 256M

release_command: migrate up
services:
  postgres: true
  redis: false
watch:
  window: 3m
  error_rate: 2%
  min_requests: 20
  max_restarts: 2
keep_releases: 5
notify: [https://hooks.slack.com/services/...]
```

### Keys

| Key | Default | Meaning |
|---|---|---|
| `app` | the app's name | Informational. |
| `branch` | `main` | Pushing this branch deploys. Overrides the branch chosen at `apps create`. |
| `build.dockerfile` | `Dockerfile` | Path of the Dockerfile, relative to the context. |
| `build.context` | `.` | Directory sent to `docker build`, relative to the repository root. Must stay inside the repository. |
| `build.args` | none | `--build-arg KEY=value`. Not for secrets. |
| `domains` | none | Domains served by the app. Combined with those set by `appdeploy domains set`. |
| `roles` | one `web` role | A map of role name to its settings. Names: lowercase letters, digits, `-` and `_`, starting with a letter. A role named `web` receives HTTP traffic. |
| `roles.<name>.on` | `[web]` | Server labels that run this role; only matters in a [group](groups.md). |
| `roles.<name>.port` | `EXPOSE` of the image, else `8080` | Port the app listens on. Passed to the app as `PORT`. Web role only. |
| `roles.<name>.cmd` | image's `CMD` | Replaces the image's `CMD` (arguments to its `ENTRYPOINT`). Quotes are respected, no shell is involved. |
| `roles.<name>.memory` | `512M` | Memory limit (`K`, `M`, `G`; at least `4M`). Swap is off. |
| `roles.<name>.cpus` | `1` | CPU limit. |
| `roles.<name>.replicas_per_server` | `1` | Containers of this role per server (1 to 32). |
| `roles.<name>.healthcheck.path` | `/up` | Web role. Must answer 2xx or 3xx. |
| `roles.<name>.healthcheck.timeout` | `5s` | Per request. |
| `roles.<name>.healthcheck.start_period` | `60s` | How long a new container has to become healthy. |
| `roles.<name>.smoke` | none | Extra paths (starting with `/`) that must answer below 500 before the switch. |
| `roles.<name>.stop_timeout` | `30s` | Time to finish in-flight work after `SIGTERM` before the container is killed. |
| `env` | none | A map of non-secret environment variables for every container, kept in git. Names are letters, digits and `_`; `PORT` and `APPDEPLOY_*` are reserved; values are single-line. Secrets with the same name win. Do not put secrets here: `appdeploy check` warns about names that look like one. |
| `roles.<name>.volumes` | none | Named volumes to mount, as `name:/absolute/path[:ro]`. The name is lowercase letters, digits, `-` and `_`; host directories are not supported. The Docker volume is `ad-<app>-<name>` (with the default `--instance`), is created on the first deploy, is shared by every release and is never removed by a deploy. Not backed up by appdeploy. |
| `release_command` | none | Run once per deploy before traffic moves, in a throwaway container of the new image with the release's environment. Like `cmd` it replaces the image's `CMD`. A failure stops the deploy. |
| `services.postgres` | `false` | A database for this app on the server's shared Postgres; provides `DATABASE_URL`. |
| `services.redis` | `false` | A Redis database number on the shared Redis; provides `REDIS_URL`. |
| `watch.window` | `3m` | How long the new release is watched after the switch. |
| `watch.error_rate` | `2%` | Roll back above this share of 5xx answers (or twice the previous release's rate, if higher). |
| `watch.min_requests` | `20` | Fewer requests than this in the window are not judged by the error rate. |
| `watch.max_restarts` | `2` | Roll back when a container restarted this many times. |
| `keep_releases` | `5` | Releases (and their images) kept for rollback. |
| `allow_partial` | `false` | In a group, roll out to the reachable servers and let the others catch up, instead of stopping at an unreachable one. |
| `notify` | none | Webhook URLs for this app's events (see [Operations](operations.md#notifications)). |

Roles other than `web` have no health check; they pass `verifying` if they are still running a
few seconds after start.

### The environment of your containers

Every container gets the `env` section of `appdeploy.yml`, then the app's secrets (a secret overrides an `env` entry of the same name), plus:

| Variable | Value |
|---|---|
| `PORT` | the web role's port (web role only) |
| `APPDEPLOY_APP`, `APPDEPLOY_RELEASE`, `APPDEPLOY_ROLE` | the app, the release number, the role name |
| `APPDEPLOY_SERVER` | the server's name (in a group) |
| `DATABASE_URL`, `REDIS_URL` | when `services` asks for them (a secret of the same name wins) |

### Volumes

A volume keeps files across deploys, rollbacks and restarts. Only **named volumes** are supported.
Things to know:

- During a deploy the old and the new container run at the same time, and both have the volume until
  the old one stops, after `watch.window` (3 minutes by default). Files that must have one writer,
  such as a SQLite database, are used by two processes meanwhile. SQLite copes with that in WAL mode,
  but you may set `watch.window` lower. The deploy log prints a warning, and `appdeploy check`
  warns when `replicas_per_server` is above 1 on a writable volume.
- If the image declares `VOLUME /data` and the role does not mount something there, the deploy is
  refused: Docker would use an anonymous volume that is deleted with the container.
- `appdeploy apps destroy` keeps the volumes (use `--delete-volumes` to remove them).
- Volumes live on one server. In a group, each server has its own copy of the data.

## Agent flags

Set on the `appdeploy agent` command line; for the installed service, edit `ExecStart` in
`/etc/systemd/system/appdeployd.service` and `systemctl daemon-reload && systemctl restart appdeployd`.
The install script sets the ones it needs.

| Flag | Default | Meaning |
|---|---|---|
| `--data-dir` | `/var/lib/appdeploy` | State directory. |
| `--socket` | `/run/appdeploy/agent.sock` | Control socket. |
| `--socket-group` | none (root only) | Group allowed to use the socket (the install script uses `appdeploy`). |
| `--repo-owner` | none | Unix user that owns app git repositories: the SSH user (`deploy`). |
| `--instance` | `ad` | Prefix of Docker names, so several agents can share one Docker (tests). |
| `--caddy-admin` | `http://127.0.0.1:2019` | Caddy's admin API, or `none` to run without a proxy. |
| `--caddy-listen` | `:443` | Where Caddy serves apps. |
| `--caddy-admin-listen` | none | Re-declare Caddy's admin address in the config it is given (needed when Caddy runs in a container). |
| `--no-tls` | off | Plain HTTP only (testing). |
| `--upstream-host` | `127.0.0.1` | Host the proxy uses to reach containers. |
| `--access-log` | none | Caddy's access log, read by the watchdog. Without it the watchdog only sees restarts and health. |
| `--caddy-access-log` | same as `--access-log` | The log's path as Caddy sees it, if different. |
| `--default-watch-window` | `3m` | `watch.window` for apps that do not set it. |
| `--min-free-bytes` | 5 GiB | Refuse to build with less free disk. |
| `--min-free-frac` | `0.10` | Refuse to build below this free fraction; also the `disk.low` alert threshold. |
| `--monitor-every` | `30s` | How often to look for crashed containers and low disk (0 = never). |
| `--notify-url` | none | A webhook for every app's events (repeatable). |
| `--backup-every` | `24h` | Postgres backups of all apps that use it (0 = never). |
| `--prune-build-cache` | off | After each deploy remove Docker build cache unused for this long (e.g. `168h`). Off because the cache is shared with everything else on that Docker. |
| `--network-pool` | Docker's default | CIDR to carve /24 networks for apps from (e.g. `172.80.0.0/12`). Docker's default pool holds only ~30 networks. |
| `--health-listen` | off | Address for `GET /health/<app>`, for load balancers. |
| `--peer-listen` | the advertised port | Address of the server-to-server API in a group. |
| `--update-repo` | `merkushin/appdeploy` | GitHub repository releases come from. |
| `--update-api` | GitHub's API | API URL for self-update (tests). |
| `--update-check-every` | `1m` | How often a group member checks whether it is its turn to install the group's wanted version (0 = never). |
| `--update-confirm-after` | `1m` | How long a freshly updated agent must stay healthy to count the update as good. |

## Files on disk

| Path | Contents |
|---|---|
| `/usr/local/bin/appdeploy` | the binary; `appdeploy.previous` after an update, `appdeploy.failed` after a revert |
| `/var/lib/appdeploy/state.db` | apps, releases, deploy journal (SQLite) |
| `/var/lib/appdeploy/apps/<app>/repo.git` | the app's git repository |
| `/var/lib/appdeploy/apps/<app>/releases/` | the frozen secrets of each release (mode 0600) |
| `/var/lib/appdeploy/secrets/<app>.env` | the app's current secrets (mode 0600) |
| `/var/lib/appdeploy/services/` | generated Postgres and Redis credentials |
| `/var/lib/appdeploy/backups/<app>/` | database backups |
| `/var/lib/appdeploy/group/` | the group's CA, this server's certificate and the member list |
| `/var/lib/appdeploy/update-pending.json`, `update-failed.json` | update bookkeeping (see [Operations](operations.md#updating-appdeploy)) |
| `/var/lib/appdeploy/caddy/` | Caddy's certificates and saved config |
| `/var/log/appdeploy/access.log` | Caddy's JSON access log (rotated, 3 x 20 MB) |
| `/home/deploy/.ssh/authorized_keys` | who may use appdeploy, with their role |
