# appdeploy — specification

Status: draft v0.1 (2026-11-13). Working name.

## 1. Why

Deploying web applications to your own servers should be simpler than Kamal and more
reliable.

The key difference from Kamal: **the deploy is driven not by a client over SSH but by an
agent on the server.** The client only states the desired state ("commit `abc123` should be
running") and can disconnect right away. The agent runs the deploy as a transaction: it
either completes it or rolls it back.

```
git push prod main      # that's it
```

### Goals

- Zero configuration by default: a repository with a Dockerfile is deployable as is.
- Several applications on one server or on a group of servers.
- Small groups: 1–10 servers.
- A deploy survives a dropped connection, an agent crash, or a bug in the new version.
- Automatic rollback based on real traffic, not only on a healthcheck.
- Safe database migrations as a separate deploy phase.
- No registry, no cross-building on a laptop.
- Easy exit: the whole setup exports to Kamal, Compose, Kubernetes or Fly.

### Non-goals

- Large clusters, autoscaling, a placement scheduler.
- Kubernetes, service mesh, overlay networks.
- A web dashboard (possible later as a separate product on top of the API).
- Managing cloud infrastructure (creating VPSes, DNS, load balancers).
- RDS-grade managed databases: Postgres here is simple, with backups, no replication in v1.

## 2. Principles

1. **The server converges to the desired state.** Every step is idempotent, journaled and
   resumable after a failure.
2. **Nothing changes for users before the traffic switch.** Any error before the switch
   just discards the new release.
3. **Any degradation after the switch means rollback** to the previous release.
4. **No proprietary formats at the boundaries.** Dockerfile, environment variables,
   `GET /up`, plain Postgres. The application does not know what deploys it.
5. **Defaults instead of settings.** Config describes only deviations from the standard.
6. **The agent is not in the traffic path.** Restarting or upgrading the agent does not
   affect running containers or the proxy.
7. **Explicit over clever.** Placement by labels, not an automatic scheduler.

## 3. Concepts

| Concept | What it is |
|---|---|
| **Group** | A set of servers under one management. Shared CA, shared list of apps. |
| **Server** | A machine with the agent installed. Has a name and labels (`web`, `db`, `worker`…). |
| **App** | An application: git repository, secrets, roles, domains. |
| **Role** | An application process: `web` (receives HTTP) or any other (`worker`, `jobs`…). A role is placed on servers by labels. |
| **Release** | An immutable "image + config + secrets" bundle. The number grows monotonically: `shop@v42`. |
| **Deploy** | A transaction moving an app from release N to release N+1. |
| **Service** | A shared group service: Postgres, Redis. Each app gets its own database and user in it. |

Hierarchy: **Group → Servers (with labels)**, **Group → Apps → Roles → (labels → Servers)**.

## 4. Architecture

On each server:

```
┌──────────────────────────── server ────────────────────────────┐
│  sshd ──forced command──> appdeploy-shell                      │
│                              ├─ git-receive-pack (push)        │
│                              └─ CLI commands                   │
│                                                                │
│  appdeployd (agent, systemd)                                   │
│    ├─ state.db (SQLite: desired state, deploy journal)         │
│    ├─ drives Docker Engine                                     │
│    ├─ drives Caddy via its admin API                           │
│    └─ mTLS API :7070 for other agents in the group             │
│                                                                │
│  caddy (systemd) — proxy, TLS, routing by domain               │
│  docker — application and service containers                   │
└────────────────────────────────────────────────────────────────┘
```

- **appdeployd** — a single static Go binary. Stores state, runs deploys, watches health,
  talks to other agents.
- **appdeploy-shell** — the same binary in SSH forced-command mode. The client needs only
  SSH access: no separate tokens or exposed ports.
- **Caddy** — a separate process so that restarting the agent does not drop traffic. Its
  config is changed through the admin API and persisted to disk (Caddy survives a reboot
  without the agent).
- **Docker Engine** — the runtime. Podman is an open question (§20).

### On-disk layout

```
/etc/appdeploy/
  group.yml            # group name, servers, labels, addresses
  pki/                 # group CA, this server's certificate and key
  authorized_keys      # user keys (+ access roles)
/var/lib/appdeploy/
  state.db             # SQLite
  secrets/<app>.env    # 0600, root
  apps/<app>/
    repo.git/          # bare repository
    build/             # build checkouts (cleaned up)
    releases/          # release metadata
/var/log/appdeploy/    # agent log
```

## 5. Installation and groups

```bash
# on the first server
curl -fsSL https://get.appdeploy.dev | sh -s -- init --group prod --name prod-1 --labels web,db
# on the next one: prod-1 issues the invite token
curl -fsSL https://get.appdeploy.dev | sh -s -- join <invite-token> --name prod-2 --labels web
```

The installer:

1. Installs Docker (if missing), Caddy, appdeployd and systemd units.
2. Creates a `deploy` user with the `appdeploy-shell` forced command.
3. On `init`, creates the group CA. On `join`, obtains a certificate with a one-time token
   (the token carries the address and the CA fingerprint, valid for 1 hour).
4. Enables basic hardening: firewall (22, 80, 443; 7070 only for group servers),
   unattended-upgrades.

It can run from cloud-init. Re-running it is idempotent.

The group's server list is replicated to all agents. There is no consensus. Membership
changes are made explicitly by an admin (`server add/remove`), they are versioned, and the
higher version wins.

## 6. Application configuration

Creating an app:

```bash
appdeploy apps create shop --domain shop.example.com
git remote add prod deploy@prod-1.example.com:shop
git push prod main
```

An `appdeploy.yml` file at the repository root is **optional**. A full example with all
defaults:

```yaml
app: shop                      # defaults to the repository name on the server
branch: main                   # a push to this branch = a deploy

build:
  dockerfile: Dockerfile
  context: .
  args: {}                     # build args (not secrets)

domains: [shop.example.com]    # TLS is issued automatically

roles:
  web:
    on: [web]                  # server labels; defaults to [web]
    port: 8080                 # defaults to the Dockerfile EXPOSE, otherwise 8080;
                               # also passed to the app as the PORT env var
    cmd: null                  # replaces the image CMD; the ENTRYPOINT is kept
    memory: 512M
    cpus: 1
    replicas_per_server: 1
    healthcheck: { path: /up, timeout: 5s, start_period: 60s }
    smoke: []                  # extra URLs: response must be < 500
    stop_timeout: 30s
  worker:
    on: [web]
    cmd: worker                # passed to the image ENTRYPOINT, e.g. "/server worker"
    memory: 512M

release_command: null          # like cmd: replaces the image CMD, e.g. "migrate up" (§9)

services:
  postgres: true               # injects DATABASE_URL
  redis: false                 # injects REDIS_URL

watch:                         # automatic rollback (§8)
  window: 3m
  error_rate: 2%
  min_requests: 20
  max_restarts: 2

keep_releases: 5
allow_partial: false           # group: roll out to the reachable servers, let the others catch up (§25)
notify: []                     # webhook URLs for deploy events
```

Rules:

- Config is taken **from the commit being deployed**. Changing config is an ordinary commit.
- Settings that belong to the server rather than the code (domains, secrets, labels) can
  be set with CLI commands; they are stored in state.db and become part of the release.
- Unknown keys are an error (at the `validating` stage, before the build).

## 7. Deploy lifecycle

A deploy is a state machine. Each transition is written to the journal (state.db)
**before** the step runs. Steps are idempotent.

```
queued
  → validating      # config, free resources, disk, reachability of target servers
  → building        # image build on the coordinator server
  → distributing    # image transfer to the other servers (§10)
  → migrating       # release_command (§9)
  → starting        # new containers next to the old ones
  → verifying       # healthcheck + smoke
  → switching       # atomic proxy switch, old containers stopped
  → observing       # watchdog (§8)
  → succeeded
```

Error branches:

| Where it fails | What happens | Outcome |
|---|---|---|
| before `switching` | new containers are removed, traffic never changed | `failed` |
| `switching` / `observing` | proxy points back to the old release, old containers are started again (images are kept) | `rolled_back` |
| `migrating`, partially applied | see §9 | `failed`, needs attention |

### Deploy sources

- `git push` to the app's branch (the main path). Deploy output is streamed to the git
  client as `remote: …` lines. A dropped connection **does not abort** the deploy.
  Note: git ignores the exit status of a `post-receive` hook, so `git push` exits 0
  even when the deploy failed; the failure is only visible in the `remote:` lines.
  Scripts and CI should use `appdeploy deploy`, which exits non-zero on failure.
- `appdeploy deploy shop --ref <sha>` — redeploy or ship a specific commit.
- `appdeploy deploy shop --image <ref>` — a prebuilt image from CI (the build is skipped).
- `appdeploy config set` / `secrets set` — a new release without a rebuild.

### Queue and locks

- Deploys of **the same** app run strictly one at a time. A push during a deploy is
  queued. The queue collapses: of several waiting deploys, only the latest runs.
- Deploys of **different** apps run in parallel, except for builds (§11).
- A lock is a state.db record with an owner and a lease. After a restart the agent resumes
  its unfinished deploys from the last step (before `switching`) or rolls them back (after).

### Switching the web role

1. The new container is running and has passed `verifying`.
2. The agent atomically changes the upstream in Caddy (one admin API call).
3. The old container keeps running while the new release is observed (§8), so a rollback
   is only a proxy switch. When the observation window ends, the old container gets
   SIGTERM, has `stop_timeout` to finish in-flight requests, then is removed (the image
   is kept). Without a previous release, or for a manual rollback, there is nothing to
   observe against and the old containers are retired at once.

Non-web roles: new containers start, then the old ones get SIGTERM with `stop_timeout`.
A short overlap of two worker versions is allowed; jobs must be idempotent.

## 8. Observation and automatic rollback (watchdog)

During `observing` the agent compares the new release against the previous release's
baseline. Data comes from Caddy access logs and Docker events.

Roll back if, within `watch.window` (3 minutes by default):

- the 5xx rate > max(`error_rate`, 2 × baseline rate) with ≥ `min_requests` requests;
- or a role container restarted ≥ `max_restarts` times;
- or the healthcheck fails 3 times in a row.

At low traffic (< `min_requests`) only the last two criteria apply.

p95 latency > 3 × baseline is only a warning in v1 (too noisy for an automatic decision).

After the observation window the release is considered successful, but the background
healthcheck keeps running: on failure the container is restarted (no rollback) and a
notification is sent.

## 9. Migrations

`release_command` runs during `migrating`: in a separate container from the new image,
with the new release's secrets, on one server, **before** the traffic switch.

Rules:

- The new schema must work with the **old** code (expand/contract). The old release serves
  traffic during and after the migration and serves it again after a rollback.
- **Compatibility check:** after the release command ran, the agent watches the **old**
  release (still serving traffic) until the new one has passed `verifying`. If its 5xx
  rate rose above the watchdog limit, it logs `migration.suspect` and sends the webhook.
  It does **not** stop the deploy: the migration is already applied and cannot be undone,
  so the old code is already broken and the fastest fix is for the new code to take over
  (where the watchdog then watches it). The warning tells a human why.
- There are no automatic down-migrations. Rolling back a release restores the code, not
  the schema.
- If `release_command` fails, the deploy goes to `failed`. Postgres DDL is transactional,
  so a typical migration (goose, golang-migrate, Rails) is either applied completely or
  not at all; for other
  cases the journal holds the command output.
- Optional (v2): a SQL migration linter that warns about unsafe operations
  (renaming or dropping columns without two phases).

## 10. Multiple servers

### Placement

A role is placed on **all** servers that carry at least one of its `on` labels. No
automatic scheduler: if you want three web servers, put the `web` label on three servers.

### Coordinator

The deploy coordinator is the agent that received the push (or the CLI command). It:

1. Takes the app lock lease on all target servers.
2. Builds the image.
3. Distributes the image: `docker save | zstd` over mTLS to the other agents, which run
   `docker load`. In v2, only missing layers are transferred.
4. Runs `migrating` once, locally.
5. Rolls out in waves:
   - **wave 1 (canary):** one server, observed for `min(window, 1m)`;
   - **next waves:** 1 server at a time if there are ≤ 3 servers, otherwise 25% at a time.
6. Each target agent runs `starting → verifying → switching → observing` itself and
   reports the result. A rollback on one server stops the rollout and rolls back the
   servers already switched.

### Failures

- **A target server is unreachable at `validating`:** the deploy does not start
  (`--allow-partial` rolls out to the reachable ones, and the unreachable one catches up
  when it returns — see below).
- **The coordinator dies mid-rollout:** each agent stores the app's desired release. Agents
  that received the new release stay on it, the others stay on the old one.
  `appdeploy deploy shop --resume` (from any server) finishes the rollout. The mixed state
  is safe thanks to the expand/contract rule.
- **A server comes back after downtime:** its agent compares its release with the latest
  successful one on its peers and catches up.

### Load balancing across servers

Outside appdeploy's scope. Options: a cloud load balancer, DNS round-robin, Caddy on one
server proxying to the rest. The agent serves `GET :7071/health/<app>` (no mTLS, status
only) for external load balancers and takes the server out of rotation during `switching`.

## 11. Multiple applications per server

- **Routing:** Caddy picks the app by `Host`. Container ports are assigned by the agent and
  are not published externally.
- **Networks:** each app has its own Docker network. Services are attached to the app's
  network, not the other way around.
- **Limits:** each role has a memory and CPU limit (cgroups). At `validating` the agent
  checks that the sum of limits on the server ≤ RAM − reserve (15% or 512 MB by default).
  Otherwise it refuses with a clear error.
- **Builds:** at most one at a time per server. A separate cgroup limited to
  min(4 GB, 50% RAM) with lowered CPU/IO priority so a build cannot OOM its neighbors. If
  the group has a server labeled `builder`, builds run there.
- **Rollback isolation:** each app has its own watchdog, metrics and rollback.
- **Disk:** `keep_releases` images are kept per app; the rest are removed after a
  successful deploy. A build does not start with < 10% or < 5 GB free. Container logs are
  rotated (json-file, 3 × 20 MB).

## 12. Services (Postgres, Redis)

- Placed on the server labeled `db` (the first server in the group by default).
- One instance per group. Each app gets its own database and user; `DATABASE_URL` /
  `REDIS_URL` are added to its secrets automatically.
- Access from other group servers — over a private network or an mTLS tunnel (v2).
- Postgres backups: daily `pg_dump` to local disk + S3-compatible storage, keeping 7 daily
  + 4 weekly. `appdeploy db backup|restore|psql shop`.
- Exit: `DATABASE_URL` can point to an external Postgres with no code changes.

## 13. Secrets

```bash
appdeploy secrets set shop STRIPE_KEY=sk_live_… SESSION_SECRET=…
appdeploy secrets list shop        # names only
appdeploy secrets export shop      # .env to stdout
```

- Stored in `/var/lib/appdeploy/secrets/<app>.env` (0600, root) on the servers where the
  app runs. Replicated over mTLS.
- Encrypting at rest with a key on the same disk adds no protection, so there is none.
  Threat model: root on the server = access to secrets.
- Optional: `.appdeploy/secrets.age` in the repository, encrypted to the group's public
  key. Decrypted by the agent at deploy time.
- Changing secrets creates a new release without a rebuild (the full cycle with
  verification and rollback).
- Secrets never go into build args, logs or `git push` output.

## 14. Proxy and TLS

- Caddy with automatic Let's Encrypt (HTTP-01; DNS-01 in v2).
- HTTP → HTTPS redirect; HSTS off by default.
- `X-Forwarded-*` headers are set.
- Long-lived connections (WebSocket, SSE) get `stop_timeout` to finish on switch.
- Maintenance mode: `appdeploy maintenance on shop` — a static 503 page.

## 15. CLI

The CLI is the same binary run locally, talking to the server over SSH. Without a local
binary you can call it directly: `ssh deploy@prod-1 status shop`.

```
appdeploy apps create|list|destroy <app>
appdeploy deploy <app> [--ref sha | --image ref] [--resume] [--allow-partial]
appdeploy rollback <app> [--to vN]
appdeploy status [<app>]              # releases, roles, servers, deploy state
appdeploy logs <app> [--role web] [-f]   # aggregated across servers
appdeploy releases <app>
appdeploy exec <app> [--role web] -- <cmd>   # one-off admin commands, shells
appdeploy config set|unset|list <app>
appdeploy secrets set|unset|list|export <app>
appdeploy domains add|remove <app> <domain>
appdeploy db backup|restore|psql <app>
appdeploy maintenance on|off <app>
appdeploy server add|remove|label|list
appdeploy export <app> --format kamal|compose|k8s|fly
appdeploy self-update
```

The target server is taken from the current repository's git remote or the `--host` flag.

## 16. Agent-to-agent protocol

- HTTP/2 + mTLS on port 7070, certificates from the group CA.
- Operations: `GetState`, `SetDesiredRelease`, `AcquireLease`/`ReleaseLease`,
  `PushImage` (stream), `RunStep`, `StreamEvents`, `ReplicateSecrets`, `ReplicateGroup`.
- Every mutating operation carries a `deploy_id` (the coordinator's ULID) and is idempotent
  by it.
- Protocol version in a header; agents of different minor versions are compatible.

## 17. Observability and notifications

- `appdeploy status` and `logs` are the primary interface.
- The deploy journal (all steps, build and migration output) is kept for 30 days.
- Webhooks on events: `deploy.started|succeeded|failed|rolled_back`, `app.crashed`,
  `migration.suspect`, `disk.low`, `backup.failed`. JSON payload; ready-made adapters for
  Slack and Telegram.
- Agent metrics in Prometheus format on `:7071/metrics` (optional).

## 18. Exit to another tool

`appdeploy export` generates the target tool's config from the current state (app, roles,
domains, services, secret names):

| Format | What is generated |
|---|---|
| `kamal` | `config/deploy.yml`, `.kamal/secrets` (variable references) |
| `compose` | `compose.yml` + `.env.example` |
| `k8s` | Deployment/Service/Ingress per role, a Secret template |
| `fly` | `fly.toml` per app, `fly secrets set` commands |

Secret values are exported by a separate command (`secrets export`) and are never written
into generated files. Postgres data moves with a plain `pg_dump`.

Compatibility guarantee: everything appdeploy can do is expressible in these formats. A
feature that cannot be exported is a reason not to build it.

## 19. Security

- Only 22, 80 and 443 are open externally. Port 7070 is open only to group server addresses.
- User access — SSH keys in `/etc/appdeploy/authorized_keys` with roles: `admin`
  (everything), `deployer` (push, deploy, rollback, logs, status), `viewer`.
- App containers get no Docker socket and no privileges; a non-root user in the Dockerfile
  is recommended (warning if root).
- Agent upgrade: `self-update` downloads a signed binary and verifies the signature.
- Audit: who did what is recorded in the state.db journal.

## 20. Open questions

1. **Proxy:** Caddy (TLS out of the box, admin API), kamal-proxy (simpler, built for
   this), or our own. Caddy is chosen for now.
2. **Docker or Podman** (rootless, daemonless).
3. **Cron jobs:** a role with supercronic inside the image (as today) or `schedule:` in
   the config, run by the agent.
4. ~~Traffic metrics for the watchdog~~ — decided: Caddy's JSON access log, written to a
   file the agent follows (`--access-log`). It needs no metrics configuration and survives
   agent restarts; the agent keeps per-host counters in memory.
5. **Private network between servers** for Postgres: rely on the provider's network or run
   WireGuard.
6. **Name.**

## 21. Milestones

**M1 — one server, several apps** (implemented, see §23)
install script; `git push` → build → start → healthcheck → switch; Caddy + TLS;
secrets; memory limits and serialized builds; deploy queue; journal and resume after an
agent crash; `rollback`, `status`, `logs`, `exec`; `release_command`.

**M2 — reliability** (implemented, S3 upload excepted, see §24)
watchdog with automatic rollback; the migration compatibility check; Postgres/Redis
services and backups; webhook notifications; disk cleanup.

**M3 — multiple servers** (implemented, see §25)
`join`, mTLS, labels and roles; image distribution; waves and canary; `--resume`;
catching-up servers; health endpoint for external load balancers; `builder` server.

**M4 — exit and convenience** (all but the migration linter implemented, see §26 and §27)
`export` to all formats; `--image` from CI; `self-update`; access roles; migration linter.

### M1 acceptance criteria

- A Go app with a Dockerfile deploys via `git push` with no `appdeploy.yml`.
- Three apps on one 2 GB VPS, each on its own domain with TLS.
- `kill -9` of the agent mid-build and mid-switch → after restart the deploy completes or
  rolls back correctly, with no traffic interruption.
- Interrupting `git push` (Ctrl-C) does not abort the deploy.
- Building one app does not OOM its neighbors.

## 22. Test applications

appdeploy is tested against small Go applications kept in `examples/`. They build fast,
have no runtime dependencies and let each test exercise exactly one behavior:

| App | Purpose |
|---|---|
| `hello` | Minimal HTTP server with `GET /up`; the zero-config path. |
| `pg` | Uses Postgres via `DATABASE_URL`, `migrate up` subcommand as `release_command`, plus a `worker` role. |
| `broken` | Behavior switched by env/build flag: fails healthcheck, crash-loops, or returns 5xx after N requests — to exercise `failed` and `rolled_back`. |
| `slow` | Slow start and long-lived connections (SSE) — `start_period`, `stop_timeout`, draining. |
| `heavy-build` | A Dockerfile that deliberately uses a lot of memory while building — for the build-limit criterion, since normal Go builds are too light to test it. |

The apps are framework-free so that nothing in appdeploy depends on a particular
language; a Rails example may be added later only as a compatibility check.

## 23. M1 implementation status

M1 is implemented in this repository. What exists, and where it differs from the text
above:

**Works and is covered by tests** (`go test ./...`; `go test -tags e2e ./e2e` runs the real
agent against Docker and a Caddy container with the apps in `examples/`):

- zero-config deploy by `git push`; update with no failed request (hundreds of requests
  during the switch, none failed); failed deploys leave the live release untouched and
  clean up their containers, image and release row;
- crashing, unhealthy and never-listening apps are rejected, even on the first deploy;
- rollback, secrets (new release without a rebuild; rollback restores the old secrets),
  `release_command`, extra roles (`worker`), `logs`, `exec`, domains;
- several apps on one server, with memory limits checked against RAM;
- queue: pending deploys collapse to the latest one;
- `git push` killed mid-deploy does not stop the deploy;
- `kill -9` of the agent mid-build resumes the same deploy after restart;
- `kill -9` of the agent at each step of the traffic switch (before the store changes,
  after, after the proxy changes) is completed after restart; traffic keeps flowing;
- the SSH entry point (`appdeploy shell`) accepts only `git-receive-pack`/`git-upload-pack`
  of an existing app and client commands, and refuses everything else.

**Differences from the spec above**

- `release_command` is in M1 (it was planned for M2).
- Builds are serialized (one at a time per server) but not yet confined to a cgroup with
  a memory limit (§11); a heavy build can still starve neighbors.
- Port 8080 is the default web port, passed to the app as `PORT`.
- `cmd` and `release_command` replace the image's CMD (the ENTRYPOINT stays).
- Containers publish their port on `127.0.0.1` (random host port); Caddy proxies to it.
  The agent flag `--upstream-host` changes the host the proxy dials (the tests run Caddy
  in a container and use `host.docker.internal`).
- Release numbers come from a counter shared with deploys, so a failed deploy burns its
  number (`v1`, then `v3` after a failed `v2`).
- Every SSH key in `authorized_keys` is an admin (roles are M4). `secrets export` and
  `exec` are therefore available to every deployer.
- `scripts/install.sh` has only been syntax-checked, not run on a real Linux server.

## 24. M2 implementation status

**Watchdog (§8), covered by e2e tests.** After the switch the deploy enters `observing`
for `watch.window` (default 3m, so an update takes that long to report `succeeded`; the
client may disconnect, and `watch.window` can be shortened). The previous release stays
up. The new release is rolled back (traffic back to the previous release, new containers,
row and image removed, state `rolled_back`) when, within the window:

- the 5xx rate exceeds `max(watch.error_rate, 2 x the rate over the minute before the
  switch)` with at least `watch.min_requests` requests (counted from Caddy's access log);
- or a container restarted `watch.max_restarts` times, or exited;
- or the web health check fails 3 times in a row.

Without `--access-log` only the last two apply. A first deploy has nothing to roll back
to and completes immediately. p95 latency is not watched (the spec makes it warn-only).
A crash of the agent during the switch or during observation is resumed (the observation
window restarts).

**Notifications (§17).** `notify:` URLs in `appdeploy.yml` and the agent's repeatable
`--notify-url` get `deploy.started`, `deploy.succeeded`, `deploy.failed`,
`deploy.rolled_back` and `migration.suspect` as JSON. Slack incoming webhooks and the
Telegram Bot API get `{"text": ...}`. Delivery is asynchronous with 2 retries; 4xx
answers are not retried; webhook URLs are never written to logs (they carry secrets).
`backup.failed` is sent by the backup schedule; `app.crashed` and `disk.low` by the monitor (§26).

**Disk cleanup (§11).** After a successful deploy (and only while no build runs) dangling
images of this agent's builds are pruned. `--prune-build-cache 168h` additionally drops
build cache unused for that long; it is off by default because the cache is shared with
everything else on that Docker. Release images beyond `keep_releases` were already
pruned in M1. Log rotation of containers (3 x 20 MB) and of Caddy's access log (20 MB x 3)
is configured.

**Migration compatibility check (§9)** warns instead of stopping, see §9. Only the
decision function is unit-tested; there is no e2e test for it.

**Services and backups (§12), covered by an e2e test.** `services: {postgres: true,
redis: true}` starts one shared `postgres:17-alpine` and/or `redis:7-alpine` container per
agent (named volumes, memory-limited, restarted by Docker) and puts `DATABASE_URL` /
`REDIS_URL` into the release's environment. Each app gets its own Postgres role and
database (`app_<name>`), with a random password kept in `/var/lib/appdeploy/services/`
(0600) and reused on every deploy. The service container is attached to the app's own
network, so apps cannot reach each other's containers. A secret with the same name
(`DATABASE_URL`) overrides the generated one, which is how an app points at an external
database. Redis gives each app its own database number (1-15, so at most 15 apps); that
separates keys but is not a security boundary, since all apps share the server password,
and the URL uses the user name `default` because `redis-cli` rejects an empty one.

`appdeploy db backup|list|restore <app>`: `pg_dump --clean --if-exists` to
`/var/lib/appdeploy/backups/<app>/<UTC time>.sql.gz`. The agent backs up every app with
Postgres every `--backup-every` (default 24h; 0 disables) and keeps the newest backup of
each of the last 7 days plus of each of the last 4 weeks. A failed scheduled backup sends
`backup.failed`. Restoring replaces the database's tables with the dump's.

Gaps: no S3-compatible upload (backups live on the server's disk, so a lost server loses
them: copy `/var/lib/appdeploy/backups` elsewhere), no Postgres replication, service
containers are not counted in the memory check, `apps destroy` keeps the app's database
and role, services are reachable only from this server (a private network for multi-server
groups is M3), service images are not upgraded by appdeploy.

**Testing hooks.** `APPDEPLOY_FAILPOINT=switch:before|switch:after-db|switch:after-proxy|observe:start`
makes the agent SIGKILL itself at that point of the traffic switch or observation. It exists only so the
e2e tests can crash the agent at exact moments.

## 25. M3 implementation status

**Groups, covered by unit and e2e tests (three servers sharing one Docker daemon).**

- `appdeploy group init --server a --advertise host:7070 --labels web,db` on the first
  server creates a private CA (ECDSA P-256) and a certificate for the server; the agent
  then also listens on the peer port (TLS 1.3, client certificates required).
  `appdeploy group invite` (on the founder) prints a one-hour, single-use token holding
  the founder's address, the CA fingerprint and a secret. On the new server
  `appdeploy group join --token ... --server b --advertise ... --labels web` pins that
  fingerprint, sends a certificate request and receives its certificate. Run `join` before
  starting the agent of the new server.
- Membership is a versioned list replicated to every member; the higher version wins and
  only the founder changes it (`group servers`, `group remove`). Consequently the founder
  is a single point for adding or removing servers (not for deploying) and its CA key is
  the group's root of trust; there is no CA rotation or certificate renewal (certificates
  last 5 years).
- A deploy is coordinated by the server that received the push or command. It builds once,
  runs `release_command` once, sends the image (`docker save | gzip`) to every other
  target in parallel, then rolls out in waves: up to 3 targets one at a time, otherwise a
  canary and then 25% at a time. A server is a target if its labels intersect any role's
  `on:`; each runs only the roles that match. The coordinator may also be a target.
  Each server runs the usual steps (start, verify, switch, observe) and reports through
  its journal, which the coordinator copies into its own as `[server] ...` lines. The
  canary is observed for at most 1 minute, later waves for at most 20 seconds (or
  `watch.window`, if shorter) — the full window applies to single-server deploys.
- If a wave fails, the servers that already switched go back (to their previous release,
  or are cleared if it was their first) and the coordinator's deploy fails with
  `rollout stopped in wave N`. `appdeploy rollback` goes back on every server.
- Requests are idempotent per release number: asking a server again for a release it
  already serves does nothing. If the coordinator dies mid-rollout, its restart runs the
  deploy again from the start (the build is mostly cached) and the servers that finished
  answer "already serving"; this stands in for `deploy --resume`.
- Containers get `APPDEPLOY_SERVER` (the server's name) in their environment.
- `--health-listen :7071` serves `GET /health/<app>`: 200 when the server runs a web
  container of the live release, 503 otherwise and while a deploy is in `switching` on
  this server (a moment: the proxy is being changed), with no authentication.
- **Build servers.** A server labeled `builder` builds for the group: the coordinator sends
  it the checked-out source (a tar stream, unpacked with path and link checks), the builder
  runs the build under its own build slot and disk check, and the coordinator fetches the
  finished image and distributes it as usual. A builder does not need to carry any role
  label; if one does, it is skipped when images are sent because it already has the image.
  The builder removes the source and the image afterwards (it keeps the image if that
  release is live there). A coordinator that is itself a builder builds locally. With
  several builders the first by name is used; there is no failover to another builder.
- **Catch-up.** When a rollout or rollback completes, the coordinator records a *final*
  (app, release, time) and announces it to every server. A server that was down misses the
  announcement but learns it from its peers when it starts and every 30 seconds after,
  so a server that was unreachable, was restarted, or joined later converges: if one of
  its labels matches a role of the app and it is not on that release, it fetches the
  release's description (config, environment, commit) and image from a peer that has them
  and deploys it like any other member. It does nothing while a deploy of that app is in
  progress on it, and it never moves an app back if something newer succeeded here after the
  final was recorded. A rollback is a new final, so it spreads the same way.
- **Partial rollouts.** Before distributing, the coordinator pings every target. An
  unreachable one stops the deploy with a message, unless the app sets `allow_partial: true`
  (the same for rollback): then it is skipped with a warning and left to catch up.
- `--network-pool CIDR` makes the agent allocate /24 subnets for app networks from that
  pool; Docker's default pool holds about 30 networks, so about 15 apps. The install
  script configures Docker with 172.80.0.0/12 split into /24s.

**Not done:** `--allow-partial` as a command-line flag (the config key above covers it);
no failover to a second builder; catch-up does not apply to apps whose roles carry only
labels this server lacks, and it only runs for apps that completed a rollout through
a coordinator (apps deployed before servers were grouped have no final record until their
next deploy); Caddy on each server routes only to its own containers, so spreading traffic
across servers needs an external balancer (use the health endpoint); services
(Postgres/Redis) are refused when a role runs on a server other than the coordinator
(they would not be reachable), so use an external `DATABASE_URL` secret; secrets are sent
to servers inside each release's payload over mutual TLS, not replicated on their own,
and apps are created on the other servers by their first deploy; access to the peer API
is all-or-nothing for any member; the git repository of an app lives only on the server
that was pushed to, so push to the same server each time (a different server needs its own
`apps create`).

## 26. M4 implementation status

**Export (§18), covered by unit tests and an e2e test.** `appdeploy export <app> --format
kamal|compose|k8s|fly [--out dir]` renders the live release:

- `kamal`: `config/deploy.yml` (roles with the group's server addresses by label, proxy with
  the domains, health path and port, secret names, Postgres/Redis as accessories) and
  `.kamal/secrets` (references such as `NAME=$NAME`);
- `compose`: `compose.yml` (a service per role, memory and CPU limits, Postgres/Redis, and
  Caddy with a generated `Caddyfile` when there are domains) and `.env.example`;
- `k8s`: one file with a Secret template, a Deployment per role (replicas = replicas per
  server x servers, readiness probe, limits), a Service and an Ingress;
- `fly`: `fly.toml` (processes, health check, VM size) and `secrets.sh`.

Only the names of secrets are written, never values; the e2e test sets a secret and checks
that no format contains it. The compose file is validated with `docker compose config`.
Images stay on the servers, so the files point at `your-registry.example.com/<app>:v<N>`:
push the image there first. Data in Postgres moves with `db backup` / `pg_restore`.

**Roles for SSH keys (§19).** `appdeploy shell --role viewer|deployer|admin` in the key's
`command=` in `authorized_keys` (the install script writes it: `--ssh-key` admin,
`--deployer-key`, `--viewer-key`). A viewer sees `status`, `releases`, `apps list`,
`group servers`; a deployer adds `git push`/fetch, `deploy`, `redeploy`, `rollback`, `logs`
and `secrets list`; an admin does everything, including `exec`, secrets, apps, domains,
database backup and restore, export and group changes. No role means admin, so installs
made before roles keep working. Roles apply to the SSH entry point; whoever can reach the
agent's unix socket as root or the socket group still has full control. Peer-to-peer
access in a group remains all-or-nothing.

**Prebuilt images.** `appdeploy deploy <app> --image REF [--config appdeploy.yml]` skips the
build: the image is used from this server's Docker if present, otherwise pulled (use
`docker login` on the server for private registries), and the release then follows the usual
path: release command, start, verify, switch, observe, rollback. The config is the file given
with `--config`, else that of the live release, else the defaults. It is a normal release
with its own number, and works in a group (the image is distributed like a built one).

**Alerts.** The agent looks every `--monitor-every` (default 30s) at the containers of live
releases and sends `app.crashed` when one was restarted by Docker, is not running, or is
restarting repeatedly (once per incident, at most every 10 minutes per container; apps being
deployed are skipped), and `disk.low` when free space on the data disk falls below the
build thresholds (5 GiB or 10%), once until it recovers by 20%. Both go to the global
webhooks and to the app's `notify:` list.

**Not done:** the migration linter, uploading backups to S3-compatible storage, a cgroup memory limit for builds, and
services for apps whose roles run on several servers.

## 27. Releases and self-update

Releases are published on GitHub (`merkushin/appdeploy`, public, so servers need no token).
Pushing a tag `vMAJOR.MINOR.PATCH` runs `.github/workflows/release.yml` on a Linux runner:
`go vet`, `go test`, then `scripts/release.sh`, which cross-compiles
`appdeploy-<os>-<arch>` for linux and darwin on amd64 and arm64, writes `SHA256SUMS`, signs
it with ed25519 (`SHA256SUMS.sig`, base64) and verifies the signature before anything is
published. The binaries are built with the matching public key and the repository
(`-X main.updatePublicKey`, `-X main.updateRepo`, `-X main.version`).

**One-time setup, by the owner of the repository.**
`go run ./cmd/releasetool keygen -out release.key` writes the private key (mode 0600,
never overwrites) and prints the public key. Store the private key as the repository
secret `RELEASE_SIGNING_KEY` and keep a backup; store the public key as the repository
variable `RELEASE_PUBLIC_KEY`. `.gitignore` excludes `*.key` and `dist/`.

**Updating.** `appdeploy self-update [--check] [--force] [--repo owner/name]` (admin; over
SSH as `appdeploy --host deploy@server self-update`) asks the agent, which runs as root:

1. fetch the latest release from the GitHub API;
2. refuse unless it is newer than the running version (a `dev` build takes any release;
   `--force` allows the same or an older one);
3. download `SHA256SUMS`, `SHA256SUMS.sig` and the binary for this OS and CPU, verify the
   signature against the key built into the running binary, then the binary's hash;
4. write the binary next to the current one, run it with `version` and require it to report
   the release's version;
5. keep the old binary as `appdeploy.previous` and rename the new one into place;
6. reply, then exit cleanly a second later so that systemd (`Restart=always`) starts the new
   binary. Containers keep running, and unfinished deploys resume.

Without `--force` it refuses while deploys are in progress. A binary built without a
public key (a plain `go build`) cannot verify updates and says so. To go back by hand:
`mv appdeploy.previous appdeploy` and restart the service.

**Trust.** The signature, not GitHub's TLS, is what is trusted: someone who gets into the
GitHub account cannot ship code to servers unless they also have the signing key. The risk
that remains is theft of the key, which lives in the repository's Actions secrets; a
protected environment with required reviewers for that secret narrows it further. There is
no key rotation: a new key means a release of binaries that embed it, which servers accept
only if it is still signed with the old key, and that migration is not automated.

**Not done:** updating a whole group from one command (run it on each server), the install
script downloading the latest release itself (it still takes `--binary`), a release
channel other than "latest", and `--check` notifications.
