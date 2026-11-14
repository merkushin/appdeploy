# Comparison: when to choose appdeploy

appdeploy is for people who run **a handful of web apps on one to ten servers they control** and
want deploys that are boring: `git push`, a safe switch, an automatic way back. Below, how it
differs from the tools you are likely to be weighing. We describe the other tools as we
understand them at the time of writing; check their documentation before deciding.

## The short version

| | appdeploy | Kamal | Dokku | Coolify / Dokploy | Fly.io, Render, Railway | Compose + SSH scripts | Kubernetes |
|---|---|---|---|---|---|---|---|
| Who drives a deploy | agent on the server | your machine or CI, over SSH | server (git hook) | server, via web UI | their platform | your script | the cluster |
| Survives your connection dropping mid-deploy | yes, and resumes after crashes | the client drives it, so no | server-side, but check | yes | yes | no | yes |
| Needs a container registry | no | yes (can be a local one) | no | optional | built in | optional | yes |
| Automatic rollback on bad traffic | **yes** (5xx, restarts, health) | no (health check at deploy) | no | no | varies | no | not by default |
| Multiple servers | small groups, canary, labels | yes, by role/host | one host; other schedulers via plugins | yes | their regions | by hand | yes, at any scale |
| Needs a Dockerfile | yes | yes | no (buildpacks too) | no (buildpacks too) | no | yes | yes |
| Persistent volumes for the app | named volumes | yes | yes | yes | yes | yes | yes |
| Web UI | no | no | no (CLI) | **yes** | yes | no | dashboards |
| Where it runs | your Linux servers | your Linux servers | your Linux server | your Linux servers | their servers | yours | yours or managed |
| Leaving | `export` to Kamal, Compose, k8s, Fly | n/a | n/a | n/a | n/a | n/a | n/a |

## What appdeploy does differently

**The server drives the deploy.** In a tool where your laptop or CI sequences SSH commands, an
interrupted session can leave a server half-way and a lock behind. In appdeploy your terminal only
watches: the agent writes every step to a journal before running it, finishes after you disconnect,
and finishes or reverts after its own crash.

**It watches the release after the switch.** Health checks before the switch catch apps that do not
start; they do not catch an app that starts and then fails on real requests. appdeploy keeps the old
release running for a few minutes (default 3) and puts traffic back by itself if the new one
produces too many 5xx answers, crashes, or stops answering. Rolling back is a single proxy change.

**No registry, no cross-compiling.** The image is built where it runs (or on a build server) and
copied directly to the other servers. Nothing to host, no credentials to distribute, no `arm64` Mac
building `amd64` images.

**Small groups without a cluster.** Put roles on servers by label; a deploy builds once, goes to
one server first, and a failure undoes the servers already switched. A server that was down catches
up when it returns. Updating appdeploy itself on all servers is one command, in order, with automatic
revert.

**Open at the edges.** Your app needs a Dockerfile, environment variables and `GET /up`; nothing
appdeploy-specific is inside your image. `appdeploy export` renders the live setup for Kamal,
Compose, Kubernetes or Fly. See [Migration](migration.md).

## Compared one by one

**Kamal.** The closest cousin: both deploy Docker images to your servers with a proxy doing zero
downtime switches. Kamal is mature, has a large user base, supports accessories and hooks, handles any
number of servers, and keeps the conductor on your side. Choose Kamal if you already use it, need
volumes, hooks or many servers. Choose appdeploy if you want the server to own the deploy, want
rollbacks driven by traffic, and do not want a registry. The two are close enough that
[migrating between them](migration.md) is mostly renaming.

**Dokku.** A self-hosted Heroku on one server: `git push`, buildpacks or a Dockerfile, plugins for
databases, Let's Encrypt, persistent storage. It is the most similar to appdeploy's workflow and
far more mature, with a large plugin ecosystem. Choose Dokku if you want buildpacks, storage mounts
or plugins. Choose appdeploy if you want automatic rollback, a journaled deploy that survives
crashes, or more than one server.

**Coolify, Dokploy and similar.** Self-hosted platforms with a web UI, templates for many databases
and services, and multi-server support. Choose them if you want a dashboard, a catalogue of
one-click services, or a team that prefers clicking. appdeploy has no UI and does one thing: ship
and run your web apps safely, from the command line and git.

**Fly.io, Render, Railway and other hosted platforms.** Someone else runs the servers; you pay for
that and accept their limits and prices. Choose them if you do not want to operate servers at all.
appdeploy is for when you do (cost, data location, control) and want that to be pleasant.

**Docker Compose with SSH scripts.** Fine for one app, until you need the switch without downtime,
rollbacks, secrets per release, TLS and multiple apps; those are what appdeploy adds, and `export`
will give you a Compose file back if you leave.

**Kubernetes.** The right answer at a scale or complexity appdeploy deliberately does not aim at:
dozens of services, autoscaling, many teams, stateful workloads. Below that it is a lot to run.

## What appdeploy does not do

Be sure none of these is a deal breaker before you adopt it:

- **Only named volumes, on one server, without backups.** Files that survive deploys can live in
  a named volume (`roles.<name>.volumes`); host directories, volume drivers and backups of volumes
  are not supported, and during a deploy the old and the new container share the volume for the
  watch window. Postgres and Redis (with backups) are the better home for important data.
- **No buildpacks.** You provide a Dockerfile.
- **Linux servers and Docker only.** The install script targets Debian and Ubuntu with systemd.
- **No web UI** and no multi-user accounts beyond SSH key roles.
- **Not for large fleets.** Up to about ten servers; no scheduler, no autoscaling, no load
  balancer (you bring one, or DNS).
- **Backups stay on the server** unless you copy them; there is no built-in off-site backup.
- **Young.** Version 0.1: thoroughly tested by its own end-to-end suite against real Docker and
  Caddy, but not yet proven in production, and the install script has not been run on a real server
  by its authors. Pin your expectations accordingly, and keep the exit door in mind: `appdeploy export`.
