# Concepts

## The pieces

- **The agent** (`appdeploy agent`, service `appdeployd`) runs on every server as root. It owns
  the state, runs deploys, talks to Docker and Caddy, and answers the CLI over a unix socket.
- **Caddy** (service `appdeploy-caddy`) is the reverse proxy and gets the TLS certificates. The
  agent reconfigures it through its admin API. It is a separate process, so restarting the agent
  never interrupts traffic.
- **Docker** runs your containers. appdeploy uses the `docker` command line.
- **The CLI** is the same binary. `appdeploy --host deploy@server <command>` runs the command
  on the server over SSH; the server side of that is a *forced command*, so the SSH user can do
  nothing but run appdeploy (see [Operations: security](operations.md#security)).
- **git**: each app has a bare repository on the server. Pushing to its deploy branch starts a
  deploy through a `post-receive` hook.

## Apps, roles, releases

An **app** is something you deploy: a name, domains, a git repository, secrets and a config.

A **role** is one kind of process of the app. `web` is special: it gets HTTP traffic, a health
check and the domains. Any other role (`worker`, `jobs`, ...) is just a command that must keep
running. Roles can be placed on different servers with [labels](groups.md).

A **release** is one deployable version: an image, the configuration it was deployed with and a
frozen copy of the app's secrets. Releases are numbered `v1`, `v2`, ... and the numbers never
repeat (a failed deploy uses up its number, so you can see `v1`, `v3`). The last five releases
(`keep_releases`) are kept so you can roll back to them.

Because a release freezes its secrets, **rolling back also restores the old secrets**.

## What happens in a deploy

A deploy is a state machine run by the agent. Every step is written to the agent's journal
*before* it runs, which is how it survives crashes. Your terminal only watches.

| State | What happens |
|---|---|
| `queued` | Waiting. Deploys of one app run one at a time; if several wait, only the newest runs and the others become `superseded`. |
| `validating` | Reads the commit and its `appdeploy.yml`, checks memory (the sum of container limits must fit in RAM minus a reserve) and disk. |
| `building` | `docker build` from your Dockerfile. Builds run one at a time per server. |
| `migrating` | Only if `release_command` is set: runs it once in a throwaway container of the new image, **before** any traffic moves. |
| `starting` | Starts the new containers next to the old ones. |
| `verifying` | Probes `GET /up` of each new web container until it answers 2xx/3xx twice in a row (up to `healthcheck.start_period`), then any `smoke` paths. Worker roles must stay up a few seconds. |
| `switching` | One proxy change moves the domains to the new containers. The old ones keep running. |
| `observing` | The **watchdog** (below) watches the new release for `watch.window`. |
| `succeeded` | The old release is stopped and removed; old images beyond `keep_releases` are pruned. |
| `failed` | Something went wrong **before** traffic moved: the new containers, image and release are removed; the old release never stopped serving. |
| `rolled_back` | The watchdog moved traffic back to the old release and removed the new one. |

So a deploy normally takes the build time plus the observation window, **3 minutes by
default**. Lower it with `watch.window` if that is too long for you; the deploy has already
switched traffic by then, so the wait is only for the safety net.

## The watchdog

After the switch the previous release stays running. For `watch.window` the agent counts
responses of the new release from Caddy's access log and watches its containers. It rolls back
(by switching the proxy, so it is immediate) if:

- more than `max(watch.error_rate, 2 x the previous release's error rate)` of the requests got
  a 5xx answer, with at least `watch.min_requests` requests (needs the access log, which the
  install script configures);
- a container restarted `watch.max_restarts` times or exited;
- the health check fails three times in a row.

The first deploy of an app has nothing to go back to, so it completes at once. Latency is not
watched.

## Migrations and the old version

`release_command` (for example `bin/rails db:migrate`) runs before the switch. While it and
the new containers start, **the old release is still serving**, and after a rollback it serves
again. So migrations must work with the old code too: add a column first, remove the old one in
a later release. appdeploy does not run down-migrations.

If the old release starts failing right after the migration (more 5xx than before), the deploy
does not stop, because the migration is already applied and the new code is the fastest fix. It
logs `WARNING migration.suspect` and sends a webhook so a human knows why.

## Crash safety

- Killing `git push` does not stop the deploy; the agent runs it.
- If the agent dies or restarts mid-deploy, it resumes the deploy from the journal (a build
  starts again, mostly from cache; a switch is completed or reverted).
- The agent is not in the traffic path, so restarting or updating it does not affect running
  apps.

## State

Everything the agent knows is under `/var/lib/appdeploy` (see
[Configuration: files on disk](configuration.md#files-on-disk)): a SQLite database with apps,
releases and the deploy journal, the git repositories, secrets (mode 0600) and backups.

## Where appdeploy has opinions

- Containers are disposable. Persistent data belongs in Postgres, Redis (appdeploy can run both
  for you, with backups) or an external service; files that must survive can go in a **named
  volume** (`roles.<name>.volumes`), which is not backed up and is shared by the old and the new
  release until the watch window ends.
- You provide a Dockerfile. appdeploy never guesses a build.
- One web port per app, one proxy per server. Spreading traffic across several servers is up to
  a load balancer in front; each server answers `GET /health/<app>` for it.
