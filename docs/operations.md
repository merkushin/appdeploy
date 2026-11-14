# Operations

## Deploying from CI

`git push` exits with status 0 even if the deploy fails (git ignores what the hook reports after
it accepted the push), so a CI job cannot rely on it. Three ways that do work:

**Push to a branch that does not deploy, then deploy by commit.** `deploy` waits and exits
non-zero if the deploy did not succeed:

```bash
git push deploy@my-server:myapp HEAD:refs/heads/ci        # not the deploy branch: nothing happens
appdeploy --host deploy@my-server deploy myapp --ref "$(git rev-parse HEAD)"
```

**Build the image in CI** and have the server pull it. Log the server in to your registry once
(`docker login`), then:

```bash
appdeploy --host deploy@my-server deploy myapp --image registry.example.com/myapp:$GIT_SHA --config - < appdeploy.yml
```

(`--config` names a file on the machine where the command *runs*, which over `--host` is the server;
`--config -` reads it from standard input, so it travels from CI. Leave `--config` out to reuse the
live release's config.)

**Read the push output** for the line `==> succeeded` (and fail if you see `deploy failed`).

Give CI a key with the `deployer` role: it can deploy and roll back, but not read secrets,
run commands in containers or destroy anything.

## Looking at things

```bash
appdeploy --host deploy@my-server status myapp
appdeploy --host deploy@my-server logs myapp -f
appdeploy --host deploy@my-server releases myapp
```

On the server: `systemctl status appdeployd appdeploy-caddy`, `journalctl -u appdeployd`,
`journalctl -u appdeploy-caddy`, `docker ps`. The agent's journal of each deploy is what you
see during the deploy; the last ones are in `status`.

## Secrets

Secrets are the app's environment variables. `secrets set` stores them (mode 0600, readable by
root only) and makes a new release without rebuilding. A release keeps a frozen copy, so
a rollback restores the old values. `secrets list` shows names only; `secrets export` prints
values (admin) and is the way to take them with you.

To change several at once without restarting the app several times, use `--no-deploy` for all
but the last, or all with `--no-deploy` and then `appdeploy redeploy`.

## Domains and HTTPS

`appdeploy domains set myapp a.example.com www.a.example.com` takes effect immediately. Caddy
requests a Let's Encrypt certificate on the first request, so DNS must already point at the
server and ports 80 and 443 must be open. Unknown host names get a plain `404 no such app`.

## Databases and backups

`services: {postgres: true}` gives the app its own database and role on a Postgres container
that appdeploy runs for you (`postgres:17-alpine`, data in a Docker volume), and `DATABASE_URL`
in its environment. The password is generated once and kept across deploys. Redis works the same
(`REDIS_URL`, a Redis database number per app, up to 15 apps; the numbers separate keys, they
are not a security boundary).

Backups: every `--backup-every` (24h) the agent writes a gzip `pg_dump` per app to
`/var/lib/appdeploy/backups/<app>/`, keeping the newest of each of the last 7 days and 4 weeks.
`appdeploy db backup <app>` takes one now; `db list` lists; `db restore <app> <name>` replaces the
database's tables with the backup. **Backups live on the same disk as the data**: copy them off the
server, for example with a nightly job on another machine:

```bash
rsync -a --delete root@my-server:/var/lib/appdeploy/backups/ /backups/appdeploy/
```

`apps destroy` keeps an app's database and backups on purpose, and its volumes too (`--delete-volumes`
removes them). **Volumes are not backed up**: copy them off the server, for example with
`docker run --rm -v ad-APP-NAME:/from:ro -v $PWD:/to alpine tar -C /from -czf /to/APP-NAME.tgz .`.

## Notifications

Webhooks receive deploy events from `notify:` in `appdeploy.yml` (per app) and from the agent's
`--notify-url` (all apps). Slack incoming webhooks and the Telegram Bot API get a text line;
anything else gets JSON:

```json
{"event":"deploy.rolled_back","app":"shop","release":3,"deploy_id":"1a1d9518857872f3fbe1d",
 "state":"rolled_back","commit":"75965914bef2f8d4b6ad882017382889b3e34f04",
 "error":"3 of 40 requests (7.5%) failed with 5xx, above the 2.0% limit","time":"2026-11-14T09:09:21Z"}
```

| Event | When |
|---|---|
| `deploy.started` | A deploy began (after its config was read). |
| `deploy.succeeded` | The release is live and its watch window passed. |
| `deploy.failed` | The deploy failed before traffic moved; the old release still serves. |
| `deploy.rolled_back` | The watchdog moved traffic back. `error` says why. |
| `migration.suspect` | The old release started failing right after `release_command`. |
| `app.crashed` | A container was restarted by Docker, stopped, or is restarting repeatedly. Once per incident, at most every 10 minutes per container. |
| `disk.low` | Free space on the data disk fell below the build threshold. Once until it recovers. |
| `backup.failed` | The scheduled database backup failed. |
| `agent.update_failed` | An update of the agent was reverted. |

Delivery is retried twice for transient errors, not for 4xx answers. URLs are never written to
logs because they carry secrets.

## Security

- **Ports.** 22, 80 and 443 face the world. In a group, the peer port is used between servers
  only; the health port (`--health-listen`) answers a bare status. The install script does not
  touch the firewall: do it yourself.
- **SSH.** The user `deploy` can run nothing but appdeploy: the key's `authorized_keys` line forces
  the command `appdeploy shell --role <role>`, which accepts git push/fetch of an existing app and
  appdeploy commands, and nothing else; options such as `--host` or `--socket` from the client are
  refused. Roles: `admin` (everything; keys given with `--ssh-key`), `deployer` (push, deploy,
  redeploy, rollback, logs, secret names), `viewer` (status and lists). Anyone who can run `exec`
  or `secrets export` (admin) can read every secret.
- **The agent runs as root** and controls Docker, so whoever controls the agent controls the server.
  The control socket is accessible to root and, if configured, one group.
- **Containers** run with the user your image sets (use a non-root `USER`), have memory and CPU
  limits, no extra privileges, no access to the Docker socket, and a network of their own per app.
  They publish their port on `127.0.0.1` only.
- **Secrets** are files with mode 0600 and are put into containers as environment variables. They
  are not in build arguments, logs or the push output. Values passed to `secrets set` are visible in
  your shell history and process list.
- **Group traffic** is mutually authenticated TLS 1.3 with a private authority, joined with
  single-use tokens that pin the authority's fingerprint.
- **Updates** are verified with a signature before they are installed; the key is built into the
  binary.

## Updating appdeploy

Releases are published on GitHub, signed with a key whose public half is built into every binary
(`appdeploy version --verbose` shows it and its fingerprint, which the release notes repeat).
Each binary also has a build attestation: `gh attestation verify <file> --repo merkushin/appdeploy`.

```bash
appdeploy --host deploy@my-server self-update --check     # is there a newer release
appdeploy --host deploy@my-server self-update             # install it
```

The agent downloads the release, verifies the signature and the checksum, runs the new binary
once, keeps the old one as `/usr/local/bin/appdeploy.previous`, replaces the binary and restarts
(the service manager starts the new one). Your apps keep running; a deploy in progress resumes. It
refuses while deploys are running unless you pass `--force`.

**A bad update reverts itself.** Before each start the service runs `appdeploy recover`. If a
freshly installed version does not manage to stay up (it is confirmed after being healthy for a
minute), the fourth start puts `appdeploy.previous` back, keeps the bad binary as
`appdeploy.failed`, and the agent sends `agent.update_failed`. A version that failed is not tried
again until another is asked for. This needs the `ExecStartPre` line and
`StartLimitIntervalSec=0` that the install script writes, so run the script again on servers
installed earlier.

**A whole group** updates from one command on the founder:

```bash
appdeploy --host deploy@a group set-version v0.2.0
appdeploy --host deploy@a group servers          # watch the versions change, in name order
```

To go back by hand: `mv /usr/local/bin/appdeploy.previous /usr/local/bin/appdeploy` and
`systemctl restart appdeployd`.

## Disk

Old releases beyond `keep_releases` are removed with their images after each deploy, dangling
images of appdeploy's builds too. Docker's build cache and images unrelated to appdeploy are not
touched unless you pass `--prune-build-cache`. Docker's disk use is shown by `docker system df`.
Builds are refused when free disk is below 5 GiB or 10% (`disk.low` is sent at the same
threshold); the thresholds are agent flags.

## Uninstalling

This deletes all apps' state; export what you need first (see [Migration](migration.md)).

```bash
# containers and networks of appdeploy (they are labelled appdeploy.instance)
docker rm -f $(docker ps -aq --filter label=appdeploy.instance)
docker network rm $(docker network ls -q --filter label=appdeploy.instance)
systemctl disable --now appdeployd appdeploy-caddy
rm /etc/systemd/system/appdeployd.service /etc/systemd/system/appdeploy-caddy.service
rm -r /var/lib/appdeploy /etc/appdeploy /usr/local/bin/appdeploy*
userdel -r deploy
```

Docker volumes of Postgres and Redis (`ad-svc-*`) remain: `docker volume rm` them if you want the
data gone.
