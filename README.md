# appdeploy

Deploy web apps to your own servers with `git push`. A small agent on each server builds,
starts, health-checks, switches traffic and cleans up, and it finishes the job even if your
laptop disconnects or the agent itself restarts. See [SPEC.md](SPEC.md) for the design.

Status: usable on one server or a small group. After a deploy the new release is watched
against real traffic and rolled back automatically if it misbehaves; deploy events go to
webhooks; `services: {postgres: true}` gives an app its own database with daily backups;
several servers work as a group with mutual TLS: one push builds once and rolls out to the
servers whose labels match, canary first (SPEC.md §21, §23-§25).

## Quick start

On a Debian/Ubuntu server with systemd (as root; untested on real hardware so far, see
SPEC.md §23):

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o appdeploy-linux-amd64 ./cmd/appdeploy
scp appdeploy-linux-amd64 scripts/install.sh ~/.ssh/id_ed25519.pub root@server:
ssh root@server ./install.sh --binary ./appdeploy-linux-amd64 --ssh-key-file ./id_ed25519.pub
```

Then, from your laptop (point the DNS name at the server first):

```bash
appdeploy --host deploy@server apps create myapp --domain myapp.example.com
git remote add prod deploy@server:myapp
git push prod main
```

Any repository with a `Dockerfile` works; the app listens on `$PORT` (default 8080) and
answers `GET /up` with a 2xx. An optional `appdeploy.yml` overrides defaults (SPEC.md §6).

After the switch the old release keeps running for `watch.window` (3 minutes by default)
while the new one is watched; `git push` reports `succeeded` when the window ends.
Set `watch: {window: 30s}` if that is too long for your app.

## Commands

```
appdeploy [--host user@server] status [app]
appdeploy deploy|redeploy|rollback <app>
appdeploy logs <app> [--role web] [-f]
appdeploy exec <app> -- <cmd...>
appdeploy secrets set|unset|list|export <app> ...
appdeploy db backup|list|restore <app>
appdeploy domains set <app> <domain>...
```

## Development

```bash
go test ./...                              # unit tests
go test -tags e2e -timeout 30m ./e2e       # end-to-end: needs Docker; pulls golang:1.27-alpine and caddy:2
```

The e2e tests start their own agent and Caddy, deploy the Go apps in `examples/`, and remove
every container, image, network and directory they create. Set `E2E_KEEP=1` to keep the
temp directory (with `agent.log`) for debugging.
