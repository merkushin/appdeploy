# appdeploy

Deploy web apps to your own servers with `git push`. A small agent on each server builds your
Dockerfile, starts the new version next to the old one, checks it, switches traffic, watches
the real traffic for a few minutes, and **rolls back by itself** if the new version misbehaves.
It keeps going when your laptop disconnects, and picks up where it left off if the agent
itself restarts.

```bash
git remote add prod deploy@my-server:myapp
git push prod main
```

```
remote: ==> validating
remote: ==> building
remote: ==> starting
remote: ==> verifying
remote: myapp-v2-web-1: healthcheck GET /up ok
remote: ==> switching
remote: ==> observing
remote: ==> succeeded: myapp is now serving v2
```

> **Status: early (v0.1).** The core is covered by an end-to-end test suite that runs real
> Docker and Caddy, but appdeploy has not been used in production yet and the install script
> has not been run on a real Linux server. Read [what it does not do](docs/comparison.md#what-appdeploy-does-not-do)
> before you choose it.

## Why appdeploy

- **The server drives the deploy, not your laptop.** Close the lid, lose Wi-Fi, kill `git push`:
  the deploy finishes anyway. If the agent crashes mid-switch, it finishes or reverts on restart.
- **Rollback by real traffic, not just a health check.** After the switch the old version stays
  up while the new one is watched. Too many 5xx, crash loops or failing health checks put
  traffic back on the old version in one proxy switch.
- **No registry, no cross-building.** Images are built on the server (or on a build server)
  and sent between your servers directly.
- **Small groups without Kubernetes.** 1 to about 10 servers: roles placed by labels, canary
  first, a failed rollout is undone, a server that was down catches up by itself.
- **Easy to move in and out.** `appdeploy import kamal` reads a Kamal setup and says what it becomes
  (and what blocks it); `appdeploy export` writes the same setup for Kamal, Docker Compose,
  Kubernetes or Fly. Your app only needs a Dockerfile, environment variables and `GET /up`.
- **Made to be operated by scripts and AI assistants:** `--json` on every command, a local
  `appdeploy check` for the config, and [a guide written for LLMs](docs/llm-guide.md).
- **Batteries for small apps:** Postgres and Redis per app with daily backups, secrets per
  release, webhooks (Slack, Telegram, generic), signed self-update, SSH roles.

How it compares to Kamal, Dokku, Coolify and others: [docs/comparison.md](docs/comparison.md).

## Documentation

| | |
|---|---|
| [Getting started](docs/getting-started.md) | Install on a server, deploy your first app |
| [Concepts](docs/concepts.md) | How a deploy works, what a release is, what the watchdog does |
| [Commands](docs/commands.md) | Every command of the CLI |
| [Configuration](docs/configuration.md) | `appdeploy.yml`, agent flags, files on disk |
| [Server groups](docs/groups.md) | Several servers, labels, build servers, updating a group |
| [Operations](docs/operations.md) | Backups, secrets, updates, security, monitoring |
| [Troubleshooting](docs/troubleshooting.md) | Error messages and what to do |
| [Migration](docs/migration.md) | From Kamal, Compose, Dokku, Fly, and away from appdeploy |
| [Comparison](docs/comparison.md) | When to choose appdeploy and when not to |
| [HTTP API](docs/api.md) | The agent's JSON API |
| [Guide for AI assistants](docs/llm-guide.md) | Rules for LLMs and agents operating appdeploy ([llms.txt](llms.txt)) |
| [Design](SPEC.md) | The design document and what is implemented |

## Quick start

On a Debian or Ubuntu server with systemd, as root, with the binary and `install.sh` of one
[release](https://github.com/merkushin/appdeploy/releases) (download steps in
[Getting started](docs/getting-started.md#1-get-the-binary-and-the-install-script)):

```bash
./install.sh --binary ./appdeploy-linux-amd64 --ssh-key-file ./my-key.pub
```

On your computer, with the `appdeploy` binary from the same release:

```bash
appdeploy --host deploy@my-server apps create myapp --domain myapp.example.com
git remote add prod deploy@my-server:myapp
git push prod main
```

The full walkthrough, including DNS and what your app must provide, is in
[Getting started](docs/getting-started.md).

## Development

```bash
go test ./...                              # unit tests
go test -tags e2e -timeout 40m ./e2e       # end-to-end: needs Docker; pulls golang:1.27-alpine, caddy:2, postgres and redis
```

The end-to-end tests start their own agent and Caddy, deploy the Go apps in `examples/`, and
remove every container, image, network, volume and directory they create. Set `E2E_KEEP=1`
to keep the temp directory (with `agent.log`) when debugging. The documentation is checked
against the code by `go test ./internal/docs` (commands, configuration keys, agent flags, API
routes, links, examples and the error messages the docs quote); if you change `README.md` or
`docs/`, regenerate `llms-full.txt` with `UPDATE_DOCS=1 go test ./internal/docs`. Releases are described in
[SPEC.md §27](SPEC.md).
