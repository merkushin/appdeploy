# Server groups

A **group** is a few servers (1 to about 10) that deploy together. Use one when you want an app
or its worker on several machines, a separate build server, or to run different roles of one
app on different servers. A single server needs no group.

## How it works

- Every server has a name and **labels** (`web`, `db`, `worker`, `builder`, anything you like).
- A role runs on the servers whose labels match its `on:` list (default `[web]`).
- The first server creates the group and is its **founder**; it holds the group's private
  certificate authority. Others join with a one-hour, single-use invite token. All
  server-to-server traffic is TLS 1.3 with certificates from that authority on both sides.
- You deploy as usual by pushing to **one** server (the app's git repository lives only there:
  always push to the same one). That server is the **coordinator** of the deploy.

A deploy in a group:

1. The coordinator checks out the commit and builds the image **once** (on a server with the
   label `builder` if there is one), runs `release_command` once, and sends the image to the
   other servers directly (no registry).
2. It rolls out in **waves**: up to 3 target servers one at a time; with more, a canary first and
   then 25% at a time. Each server runs the normal sequence (start, verify, switch, observe). The
   canary is watched for at most a minute, later waves for at most 20 seconds, instead of the
   full `watch.window`.
3. If a server fails in a later wave, the servers that already switched are **taken back** to
   their previous release and the deploy fails with `rollout stopped in wave N`.
4. Rollback (`appdeploy rollback`) goes back on every server.

Each server routes only to its own containers: spread traffic across servers with a load
balancer or DNS in front. Each server answers `GET /health/<app>` (enable it with
`--health-listen`) with 200 when it runs the app and 503 otherwise, including for the moment
traffic is being switched.

## Set up a group

You need each server to be able to reach the others on the **peer port** (the port in
`--advertise`, e.g. 7070); nothing else is exposed between them. Install appdeploy on every
server first ([Getting started](getting-started.md)).

**1. Create the group on the first server (the founder):**

```bash
ssh root@a systemctl stop appdeployd
ssh root@a appdeploy group init --server a --advertise a.internal:7070 --labels web,db --group prod
ssh root@a systemctl start appdeployd
```

`--advertise` is the address other servers use to reach this one. The certificate is issued for
that host name or IP, so use the same name later.

**2. Invite a server** (from your computer; admin):

```bash
appdeploy --host deploy@a group invite
```

It prints the command to run on the new server.

**3. On the new server**, after installing appdeploy, run it **before the agent runs**:

```bash
ssh root@b systemctl stop appdeployd
ssh root@b appdeploy group join --server b --advertise b.internal:7070 --labels web --token <token>
ssh root@b systemctl start appdeployd
```

**4. Look at the result:**

```bash
appdeploy --host deploy@a group servers
```
```
SERVER  ADDRESS            LABELS    VERSION  STATE
a       a.internal:7070    web,db    0.1.1    ok
b       b.internal:7070    web       0.1.1    ok
```

The member list is replicated: every server learns of changes within seconds, and a server that
was down learns when it returns. `group remove <name>` (on the founder) drops a server.

Back up `/var/lib/appdeploy/group/` on the founder: it holds the authority's key. Without it you
cannot add servers; there is no key rotation yet. Everything else keeps working.

## Using labels

```yaml appdeploy.yml
roles:
  web:
    on: [web]            # on every server labeled web
    replicas_per_server: 2
  worker:
    on: [worker]
    cmd: worker
release_command: migrate
```

- A server runs only the roles whose labels match. A server with none of an app's labels does
  nothing for it (it can still be the coordinator or the builder).
- Domains are served by every server that runs the `web` role; put DNS or a balancer in front.
  Servers learn an app's domains when they deploy it: after `domains set` on the coordinator,
  run `appdeploy redeploy` so the others get them too.
- `release_command` runs once, on the coordinator, before any server switches.
- `services:` (Postgres, Redis) are only available when every role of the app runs on the
  coordinating server. For an app spread over servers use an external database through a
  `DATABASE_URL` secret.

## Build servers

A server with the label `builder` builds for the group: the coordinator sends it the source,
it builds under its own build slot and disk check, and the coordinator fetches the image. It
removes its copy afterwards. With several, the first by name is used. If the coordinator is
itself a builder, it builds locally. Give a builder a lot of RAM and disk and no app roles.

## When a server is down

By default a deploy stops with `server X is not reachable` before anything changes. With
`allow_partial: true` the reachable servers get the release and the missing one is skipped with
a warning. When it returns (or joins later), it **catches up by itself**: the coordinator records
the release a completed rollout left the group on, servers exchange these records, and a server
that is not on that release fetches it from a peer and runs its part. It does nothing while a
deploy of that app is running on it, and it never moves an app back if something newer
succeeded there since.

A server that speaks a different peer **protocol** (only during an update of the group) refuses
the deploy until all servers run compatible versions.

## Updating all servers

```bash
appdeploy --host deploy@a group set-version v0.2.0
```

Every server installs that release itself, one at a time in name order, waiting while anything
is being deployed, and reverts automatically if the new version does not stay up. Watch it with
`group servers`. Details: [Operations: updating](operations.md#updating-appdeploy).

## Limits

- 1 to about 10 servers. There is no scheduler: you place roles with labels.
- The founder is needed to add or remove servers, not to deploy.
- Every member can call every other member's peer API; there are no per-server permissions.
- No load balancer is provided.
