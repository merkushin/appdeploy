# Commands

```
appdeploy [--host user@host] [--socket path] <command> [arguments]
```

- **`--host user@host`** (or the environment variable `APPDEPLOY_HOST`) runs the command on that
  server over SSH. Without it the command talks to the agent on this machine.
- **`--socket path`** (or `APPDEPLOY_SOCKET`) is the agent's socket, default
  `/run/appdeploy/agent.sock`. You need it only when running on the server yourself.
- **Exit status:** `0` success, `1` the command failed (an error is printed to stderr, a deploy
  that did not succeed counts as a failure), `2` wrong usage.
- **Roles.** Over SSH, what you may run depends on your key's role: a *viewer* can run the
  read-only commands, a *deployer* adds deploy, rollback and logs, an *admin* can run everything.
  The tables below say which. See [Operations: security](operations.md#security).

Most commands that change something stream the agent's journal until the deploy ends, so you
see what a `git push` shows. Press Ctrl-C to stop watching; **the deploy continues on the server**
(its id is printed so you can look it up).

## Reading

| Command | Role | |
|---|---|---|
| [`appdeploy ping`](#appdeploy-ping) | viewer | is the agent up |
| [`appdeploy status`](#appdeploy-status) | viewer | what runs |
| [`appdeploy apps list`](#appdeploy-apps-list) | viewer | all apps |
| [`appdeploy releases`](#appdeploy-releases) | viewer | releases of an app |
| [`appdeploy logs`](#appdeploy-logs) | deployer | container logs |
| [`appdeploy secrets list`](#appdeploy-secrets) | deployer | names of secrets |
| [`appdeploy db list`](#appdeploy-db-backup-list-restore) | viewer | backups of an app |
| [`appdeploy group servers`](#appdeploy-group) | viewer | servers of a group |
| [`appdeploy version`](#appdeploy-version) | any | version of this binary |

## appdeploy ping

Exits 0 and prints nothing if the agent answers.

## appdeploy status

```
appdeploy status [<app>]
```

Without an app: the list of apps (same as `apps list`). With an app: its live release,
domains, containers with their state (and restart counts) and the last five deploys.

```
shop  release v2  domains: shop.example.com

CONTAINER               ROLE  RELEASE  STATE
adocs498-shop-v2-web-1  web   v2       running

Recent deploys:
2026-11-14 03:09:21  v2  rollback  succeeded
2026-11-14 03:09:16  v3  push      succeeded
2026-11-14 03:09:14  v1  rollback  succeeded
```

A deploy's *source* is `push`, `cli` (`appdeploy deploy`), `redeploy`, `rollback`, `image`
(`deploy --image`); in a group also `peer`, `build` and `undo` (work a coordinator asked this
server to do). Deploy states are in [Concepts](concepts.md#what-happens-in-a-deploy).

## appdeploy apps create

```
appdeploy apps create <app> [--domain <domain>]... [--branch main]
```

Registers an app and creates its git repository. The name is lowercase letters, digits and
dashes, starting with a letter, at most 32 characters. Repeat `--domain` for several domains.
`--branch` is the branch that deploys when pushed (default `main`; an `appdeploy.yml` `branch:`
overrides it). Fails if the app exists. **Admin.**

```
created app shop (deploy branch main)
  git remote add prod <user>@<server>:shop
  git push prod main
```

## appdeploy apps list

```
APP   RELEASE  DOMAINS
shop  v1       shop.example.com
```

## appdeploy apps destroy

```
appdeploy apps destroy <app>
```

**Destructive.** Stops and removes the app's containers, removes its images, network, git
repository, releases, secrets and routes. It **keeps** the app's Postgres database and role and
its backups (delete them by hand if you want them gone). Refuses while a deploy is running.
**Admin.**

## appdeploy deploy

```
appdeploy deploy <app> [--ref <sha-or-ref>] [--detach]
appdeploy deploy <app> --image <image> [--config appdeploy.yml] [--detach]
```

Deploys without a push. Without `--ref` it deploys the head of the app's deploy branch in the
server's repository. `--ref` takes a commit that is already in the server's repository (push it
to another branch first). The command waits and **exits non-zero if the deploy did not
succeed**, which makes it the right tool for scripts and CI. `--detach` prints the deploy id and
returns.

`--image` deploys a finished image instead of building one: the server uses it if Docker has it,
otherwise pulls it (a private registry needs `docker login` on the server). `--config` sends an
`appdeploy.yml` to use (`--config -` reads it from standard input; a file name is read on the
machine where the command runs, which with `--host` is the server); without it the config of the live release is reused, or the defaults on a
first deploy. **Deployer.**

## appdeploy redeploy

```
appdeploy redeploy <app> [--detach]
```

New release from the **current image** with the **current secrets**: no build. Use it to pick up
changes made with `secrets set --no-deploy`. **Deployer.**

## appdeploy rollback

```
appdeploy rollback <app> [--to <N>] [--detach]
```

Re-activates an older release: by default the newest one older than the current. It starts the
old image again (with the secrets and config that release had), verifies it, switches traffic
and stops the newer release. It does not create a new release number and is not watched by the
watchdog. If the image of that release was pruned, it fails. In a group it goes back on every
server. **Deployer.**

## appdeploy releases

```
RELEASE  COMMIT    CREATED              
v3       75965914  2026-11-14 03:09:17  <- current
v2       7e27d62d  2026-11-14 03:09:10  
v1       7e27d62d  2026-11-14 03:09:08  
```

## appdeploy logs

```
appdeploy logs <app> [--role <role>] [-f] [--tail <N>]
```

Logs of the containers of the live release, every line prefixed with the container's name.
`--tail` lines per container (default 100), `-f` to follow, `--role` to pick one role.
Containers keep at most three 20 MB log files. **Deployer.**

## appdeploy exec

```
appdeploy exec <app> [--role <role>] -- <command> [arguments...]
```

Runs a command inside a running container of the live release (the first one of the role,
default `web`). Output is streamed; the exit status is the command's. There is **no terminal**:
no interactive shells or consoles. The command must exist in your image. **Admin.**

## appdeploy secrets

```
appdeploy secrets set <app> KEY=value... [--no-deploy]
appdeploy secrets unset <app> KEY... [--no-deploy]
appdeploy secrets list <app>
appdeploy secrets export <app>
```

Secrets are environment variables stored on the server (file mode 0600). `KEY` is letters,
digits and underscores, not starting with a digit; values must be one line.

- `set` and `unset` create a **new release without rebuilding** so running containers get the
  change, and stream it like a deploy. With `--no-deploy` they only store the change for the
  next release (use `redeploy` to apply it).
- `list` prints names only. **Deployer.**
- `export` prints `KEY=value` lines **with values**. It exists to take your secrets with you
  when migrating. **Admin.**
- `set` and `unset` are **admin**.

A secret with the same name as a generated variable (`DATABASE_URL`, `REDIS_URL`) wins, which is
how an app is pointed at an external database. Values given on the command line are visible in
your shell history and the process list while the command runs.

## appdeploy domains set

```
appdeploy domains set <app> <domain>...
```

Replaces the app's domains and reconfigures the proxy at once; no deploy. Domains from
`appdeploy.yml` (`domains:`) are added to these. **Admin.**

## appdeploy export

```
appdeploy export <app> --format kamal|compose|k8s|fly [--out <dir>]
```

Writes the live release's setup in the format of another tool: roles, ports, limits, health
path, domains, the names of secrets (never values) and servers by label. Without `--out` the files
are printed with `==> path <==` headers. See [Migration](migration.md#leaving-appdeploy).
**Admin.**

## appdeploy db backup, list, restore

```
appdeploy db backup <app>
appdeploy db list <app>
appdeploy db restore <app> <backup-name>
```

For apps with `services: {postgres: true}`. `backup` runs `pg_dump` now into
`/var/lib/appdeploy/backups/<app>/<UTC time>.sql.gz` and applies the retention (the newest of each
of the last 7 days and of the last 4 weeks). The agent also backs up every app daily. `list`
prints names, newest first. **`restore` replaces the tables of the app's database with the
backup's.** `list` is viewer; `backup` and `restore` are **admin**. Backups stay on the server's
disk: copy them elsewhere.

## appdeploy group

See [Server groups](groups.md) for the concepts.

```
appdeploy group init --server <name> --advertise <host:port> [--labels web,db] [--group <name>] [--data-dir <dir>]
appdeploy group join --token <token> --server <name> --advertise <host:port> [--labels web] [--data-dir <dir>]
appdeploy group invite
appdeploy group servers
appdeploy group set-version <vX.Y.Z | none>
appdeploy group remove <name>
```

- `init` and `join` run **locally, before the agent starts** (they prepare the server's
  identity under the data directory); the others talk to the agent.
- `invite` (founder only) prints a command to run on the new server; the token works once, for
  one hour.
- `servers` lists members with their labels, agent version and state.
- `set-version` (founder only) makes every server install that release, one at a time, in name
  order. `none` clears it.
- `remove` (founder only) drops a server from the group.

Everything except `servers` is **admin**; `servers` is viewer.

## appdeploy self-update

```
appdeploy self-update [--check] [--force] [--repo owner/name]
```

Asks the agent to install the latest GitHub release if it is newer and correctly signed, then
restart. `--check` only reports. `--force` allows the same or an older version and installing
while deploys are running. See [Operations: updating](operations.md#updating-appdeploy).
**Admin.**

## appdeploy version

Prints `appdeploy <version>` (`dev` for a build that did not come from a release).

## appdeploy agent, hook, shell, recover

Used by the system, not by you:

- `appdeploy agent [flags]` runs the agent ([flags](configuration.md#agent-flags)).
- `appdeploy hook post-receive` is called by git after a push.
- `appdeploy shell` is the SSH forced command.
- `appdeploy recover` runs before each start of the service and puts the old binary back after
  a failed update.
