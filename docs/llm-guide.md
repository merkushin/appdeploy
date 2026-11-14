# Guide for AI assistants and agents

For an LLM or an autonomous agent that operates appdeploy on behalf of a person. It is written
to be followed literally. It describes version 0.1.x; if a command here disagrees with
`appdeploy help` on the machine you are on, trust `appdeploy help` and say so.

Files for ingestion: [`llms.txt`](../llms.txt) (an index) and
[`llms-full.txt`](../llms-full.txt) (all the documentation in one file).

## What appdeploy is, in six facts

1. appdeploy deploys **Docker-based web apps** to Linux servers with `git push` or `appdeploy deploy`.
   A deploy is run **by an agent on the server**, not by your terminal.
2. An app is a git repository with a **Dockerfile**. Its settings are an optional `appdeploy.yml`.
3. A deploy builds a **release** (`v1`, `v2`, ...). The old release keeps serving until the new
   one is verified, and for a few minutes after the switch. A failed deploy changes nothing for
   users. The system may **roll back by itself** (`rolled_back`): that is it working, not a bug.
4. **Secrets** are environment variables stored on the server. Releases freeze their secrets.
5. Containers are disposable. Data lives in Postgres/Redis or elsewhere; files that must survive can go in a
   **named volume** (`roles.<name>.volumes: ["data:/data"]`), which is not backed up. Non-secret environment
   goes in `env:` of `appdeploy.yml`, secrets in `secrets set`.
6. The CLI is `appdeploy`. From a workstation: `appdeploy --host deploy@SERVER <command>`. On the
   server itself: `appdeploy <command>` (needs root or the socket group). `--host` may be left out when
   the server comes from `APPDEPLOY_HOST`, `host:` in `appdeploy.yml`, a git remote named `prod` or
   `appdeploy`, or `appdeploy hosts`; the command says on stderr where it took the server from. Do not
   guess a host: when none is known, ask the user.

## Rules

Follow these in order of importance.

1. **Look before you change.** Run `appdeploy --host H status APP` first and say what you saw.
   Never assume an app, a release number or a server exists.
2. **Never judge a deploy by the exit status of `git push`.** It is 0 even when the deploy failed.
   A deploy succeeded only if the output contains `==> succeeded` or `appdeploy deploy` exited 0.
   `deploy failed:` and `rolled back` mean it did not (for `failed`, the old release is still live).
3. **Do not retry blindly.** Read the error line that starts with `deploy failed:` (or `watchdog:`),
   fix the cause, then deploy again. Repeating an unchanged deploy repeats the failure and wastes a
   release number.
4. **Never reveal secrets.** Do not run `secrets export`, and do not run `exec` with commands that print the
   environment (`env`, `printenv`, `cat /proc/*/environ`), unless the person asked for exactly that.
   Use `secrets list` (names only). Never write secret values into files in a repository, logs or
   your answers. Values passed to `secrets set` appear in the shell history: tell the person.
5. **Ask the person first** for anything in the "ask first" rows below. State what will happen, to
   which app and server, and whether it can be undone.
6. **Be patient.** A deploy takes the build time plus a 3-minute watch window by default. Do not
   interrupt it, and do not start a second deploy of the same app to "speed it up": it would only queue behind the
   running one. Use `--detach` and poll `status` every 30 seconds or longer if you must not block.
7. **Do not touch appdeploy's files or containers by hand.** No editing of `/var/lib/appdeploy`, no
   `docker rm` or `docker stop` of containers named `ad-*`, no editing of Caddy's config. Use commands.
8. **Do not invent options.** Only the commands in this documentation exist. If you need something
   that is not here (volumes, buildpacks, an interactive shell), say it is not supported.
9. **Check the docs for what you are not sure about,** and say when you were unsure.

## Command safety

"Role" is the lowest SSH role that may run it (viewer < deployer < admin).

| Command | What it does | Undo | Ask first? | Role |
|---|---|---|---|---|
| `ping`, `version [--verbose]`, `status`, `apps list`, `releases`, `db list`, `group servers` | read only | n/a | no | viewer |
| `check [FILE]` | validates an `appdeploy.yml` on this machine; no server involved | n/a | no | local |
| `import kamal [FILE]` | reads a Kamal config on this machine and prints a plan; changes nothing | n/a | no (but show the person the blockers) | local |
| `logs APP [--role R] [--tail N]` | prints container logs (may contain personal data) | n/a | no | deployer |
| `secrets list APP` | names of secrets | n/a | no | deployer |
| `export APP --format F` | prints config for another tool (no secret values) | n/a | no | admin |
| `self-update --check` | looks for a newer release | n/a | no | admin |
| `deploy APP`, `deploy APP --ref SHA` | new release of the code; the old one serves until verified | `rollback` | **yes on production** | deployer |
| `deploy APP --image REF` | same, from a prebuilt image | `rollback` | **yes on production** | deployer |
| `redeploy APP` | new release of the same image | `rollback` | yes | deployer |
| `rollback APP [--to N]` | puts an older release back live | `rollback` or `deploy` | **yes** | deployer |
| `secrets set/unset APP ...` | new release with changed environment | `rollback` restores the old values | **yes** | admin |
| `domains set APP ...` | replaces the domain list at once | set it again | **yes** | admin |
| `apps create APP` | registers an app | `apps destroy` | yes | admin |
| `exec APP -- CMD` | runs a command in the live container | depends on CMD | **yes** | admin |
| `db backup APP` | writes a backup file | delete the file | no | admin |
| `db restore APP NAME` | **replaces the database's tables** | only by restoring another backup | **always** | admin |
| `apps destroy APP [--delete-volumes]` | **removes the app, its images, releases and secrets** (the database and the volumes are kept unless `--delete-volumes`) | no | **always** | admin |
| `secrets export APP` | **prints secret values** | n/a | **always, and say where the output goes** | admin |
| `self-update`, `self-update --force` | replaces the binary and restarts the agent | the agent reverts a bad update by itself | **yes** | admin |
| `group set-version V` | every server installs release V, one after another | set another version | **yes** | admin |
| `group invite`, `group remove S` | adds or removes a server | re-invite | **yes** | admin |

Commands that never make sense for you: `agent`, `hook`, `shell`, `recover`; they are run by the
system.

## Recipes

Replace `H` with `deploy@SERVER`, `APP` with the app. After every change, run
`appdeploy --host H status APP` and report the result.

### Deploy a change

1. `git status` and `git log -1`: confirm what will be deployed. Does the repository have a
   `Dockerfile`? Does the app listen on `$PORT` and answer `GET /up`?
2. `git push prod main` (remote `prod` = `deploy@SERVER:APP`). Success is the line `==> succeeded`.
3. If it printed `deploy failed:` the old release is still live: read the message and the log lines
   above it, fix, repeat. If `rolled back`: look at `watchdog:` for the reason.

For scripts, push to a branch that does not deploy and use the exit status:
`git push prod HEAD:refs/heads/ci && appdeploy --host H deploy APP --ref "$(git rev-parse HEAD)"`.

### Roll back

`appdeploy --host H releases APP` (what exists, which is current), then
`appdeploy --host H rollback APP` (previous) or `rollback APP --to N`. Success is
`==> succeeded: APP is now serving vN`. Remember that rolling back also restores that release's secrets.

### Change a secret

`appdeploy --host H secrets set APP NAME=value`. This creates a release without rebuilding. Tell the
person that the value is in the command line. To change several, use `--no-deploy` on all but the
last. Never print the value back.

### Add a domain

`appdeploy --host H domains set APP a.example.com b.example.com`. This **replaces** the list: include
the existing domains (`status APP` shows them). DNS must already point at the server, or HTTPS will
not work.

### Add a worker or a database

Edit `appdeploy.yml`, commit, deploy:

```yaml appdeploy.yml
roles:
  web: {}
  worker:
    cmd: worker          # arguments to the image's ENTRYPOINT
release_command: migrate
services:
  postgres: true         # provides DATABASE_URL
```

Run `appdeploy check appdeploy.yml` before you commit: it uses the parser of the deploy and reports
every mistake at once (it does not know the server's free memory or disk). An invalid file also
fails in the `validating` step of a deploy, before anything is built or changed, so a mistake is
cheap. Unknown keys are errors: use only the keys in [Configuration](configuration.md).

### Back up and restore a database

`db backup APP` then `db list APP`. To restore, first **ask**, then take a fresh backup, then
`db restore APP NAME`. Backups are on the server's disk only.

### Run a one-off command

`appdeploy --host H exec APP -- COMMAND ARGS`. There is no terminal: nothing interactive. The
command has to exist in the image. Do not use it to read secrets.

### Look at a problem

1. `status APP`: is the container `running`? what did the last deploys end with?
2. `logs APP --tail 100`.
3. On the server: `systemctl status appdeployd appdeploy-caddy`, `journalctl -u appdeployd -n 100`.
4. Match the message in [Troubleshooting](troubleshooting.md).

### Update appdeploy

`self-update --check`, tell the person, then `self-update`. For several servers: on the founder,
`group set-version vX.Y.Z`, then watch `group servers`. A version that fails to start reverts itself.

## Reading the output

- A deploy prints `queued`, then lines starting with `==> ` that name the state (`validating`,
  `building`, `migrating`, `starting`, `verifying`, `switching`, `observing`, `succeeded`). Failures print `deploy failed: REASON`. Under `git push` each line is prefixed `remote:`.
- Exit status of `appdeploy`: 0 success; 1 failure (including a deploy that did not succeed); 2 wrong
  usage. The exit status of `git push` is **not** a signal.
- Terminal states of a deploy: `succeeded`, `failed` (old release still live), `rolled_back` (old
  release live again), `superseded` (a newer deploy replaced a waiting one).
- **Ask for JSON.** Add `--json` to any command (`appdeploy --host H status APP --json`): lists and
  objects come as one JSON line, a deploy as one JSON line per journal entry whose last line is
  `{"done":true,"state":"succeeded"|"failed"|"rolled_back"|"superseded","error":"..."}`, and errors as
  `{"error":"..."}`. Parse that, not the text tables, which may change. It works over `--host` for every role.
- On the server you can also use the JSON API directly:

```bash
curl -s --unix-socket /run/appdeploy/agent.sock http://agent/v1/apps/APP | jq '.app.current_release, .containers, .deploys[0]'
curl -s --unix-socket /run/appdeploy/agent.sock http://agent/v1/deploys/DEPLOY_ID
```

  Fields and endpoints: [HTTP API](api.md). Wait for a deploy by reading
  `GET /v1/deploys/ID/events` until a line with `"done":true`.

## Things that look wrong but are normal

- The first deploy of an app is quick and has no `observing`: there is no old release to compare.
- Release numbers skip: a failed deploy uses its number (`v1`, then `v3`).
- A deploy "hangs" for three minutes at `observing`: that is the watch window; traffic has already moved.
- `git push` prints `remote: appdeploy: deploy failed: ...` and still exits 0.
- After a successful deploy the previous container disappears; the release is kept for rollback.
- `status` shows a container of two releases for a few minutes during `observing`.
- Pushing a branch that is not the deploy branch prints `not deploying` and does nothing.

## Not supported: say so, do not improvise

Host directory mounts, volume drivers and backups of volumes; buildpacks (a Dockerfile is required); interactive shells or consoles;
Windows servers; more than about ten servers; a web UI; off-site backups; automatic import from
tools other than Kamal (`appdeploy import kamal` exists; for others see [Migration](migration.md) for the
manual steps, and `appdeploy export` for leaving).

## Defaults worth knowing

| Setting | Default |
|---|---|
| Deploy branch | `main` |
| Web port | `EXPOSE` of the image, else `8080` (also passed as `$PORT`) |
| Health check | `GET /up`, 2xx or 3xx, up to 60 s to start |
| Memory / CPU per container | 512M / 1 |
| Watch window after the switch | 3 minutes (roll back above 2% 5xx with at least 20 requests, after 2 restarts, or 3 failed health checks) |
| Releases kept | 5 |
| Stop timeout | 30 s |
| Backups | daily, newest of 7 days and 4 weeks, on the server |
| Docker image of release N | `ad/APP:vN` |
| Agent socket | `/run/appdeploy/agent.sock` |
