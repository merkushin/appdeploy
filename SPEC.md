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
    cmd: null                  # defaults to the image CMD
    memory: 512M
    cpus: 1
    replicas_per_server: 1
    healthcheck: { path: /up, timeout: 5s, start_period: 60s }
    smoke: []                  # extra URLs: response must be < 500
    stop_timeout: 30s
  worker:
    on: [web]
    cmd: /app/server worker
    memory: 512M

release_command: null          # e.g. "/app/server migrate up" (§9)

services:
  postgres: true               # injects DATABASE_URL
  redis: false                 # injects REDIS_URL

watch:                         # automatic rollback (§8)
  window: 3m
  error_rate: 2%
  min_requests: 20
  max_restarts: 2

keep_releases: 5
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
3. The old container gets SIGTERM, has `stop_timeout` to finish in-flight requests, then
   is removed (the image is kept).

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
- **Compatibility check:** during `migrating` and `starting`, the watchdog watches the
  **old** release. If its 5xx rate rises after the migration, the deploy stops before the
  switch and an alert is sent: the schema is probably incompatible with the old code.
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
4. **Traffic metrics for the watchdog:** from Caddy access logs or its Prometheus metrics.
5. **Private network between servers** for Postgres: rely on the provider's network or run
   WireGuard.
6. **Name.**

## 21. Milestones

**M1 — one server, several apps**
`init` installation; `git push` → build → start → healthcheck → switch; Caddy + TLS;
secrets; resource and build limits; deploy queue; journal and resume after an agent
crash; `rollback`, `status`, `logs`, `exec`.

**M2 — reliability**
watchdog with automatic rollback; `release_command` and the migration compatibility
check; Postgres/Redis services and backups; webhook notifications; disk cleanup.

**M3 — multiple servers**
`join`, mTLS, labels and roles; image distribution; waves and canary; `--resume`;
catching-up servers; health endpoint for external load balancers; `builder` server.

**M4 — exit and convenience**
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
