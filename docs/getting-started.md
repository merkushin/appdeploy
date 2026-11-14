# Getting started

This takes you from an empty server to a deployed app with HTTPS. Plan 15 minutes.

## What you need

- A **server**: Debian or Ubuntu with systemd, root access over SSH, and ports 22, 80 and 443
  reachable. About 1 GB of RAM is enough for a small app; builds need more (a Go or Rails
  build can use 2 GB). appdeploy installs Docker and Caddy for you.
- A **domain name** whose DNS `A` record points at the server. Let's Encrypt needs ports 80
  and 443 to issue the certificate.
- An **app** in a git repository with a `Dockerfile` at the root. See
  [what your app must provide](#3-what-your-app-must-provide).
- On your computer: `git`, `ssh` and the `appdeploy` binary (below).

> The install script has not yet been run on a real server by its authors. It is short and
> plain shell: read it first (`scripts/install.sh`), and expect to report problems.

## 1. Get the binary and the install script

Pick a release (here `v0.1.1`; see the [releases page](https://github.com/merkushin/appdeploy/releases))
and download the binary for the server, its checksums, and the install script **of the same
version**:

```bash
VERSION=v0.1.1
BASE=https://github.com/merkushin/appdeploy/releases/download/$VERSION
curl -fsSLO $BASE/appdeploy-linux-amd64
curl -fsSLO $BASE/SHA256SUMS
curl -fsSLO https://raw.githubusercontent.com/merkushin/appdeploy/$VERSION/scripts/install.sh
sha256sum --check --ignore-missing SHA256SUMS      # on macOS: shasum -a 256 -c --ignore-missing SHA256SUMS
chmod +x appdeploy-linux-amd64 install.sh
```

Read `install.sh` before you run it; it runs as root. For your **own computer** download the
matching binary (`appdeploy-darwin-arm64`, `appdeploy-darwin-amd64` or `appdeploy-linux-arm64`),
rename it to `appdeploy` and put it on your `PATH`.

**Check where it came from.** The checksum only proves the download is intact. Each release binary
also carries a build attestation: proof that it was built by this repository's release workflow
and not uploaded from somewhere else. Verify it with the GitHub CLI:

```bash
gh attestation verify appdeploy-linux-amd64 --repo merkushin/appdeploy
```

(Releases made before attestations were added, up to v0.1.1, do not have one.) The release notes
also print the fingerprint of the key the binary will trust for updates. After installing, compare:

```bash
appdeploy version --verbose      # shows the release key and its fingerprint
```

From then on, `appdeploy self-update` accepts only releases signed with that key, so the first
download is the one you verify by hand.

## 2. Install on the server

Copy the files and your **public** SSH key to the server and run the script as root:

```bash
scp appdeploy-linux-amd64 install.sh ~/.ssh/id_ed25519.pub root@my-server:
ssh root@my-server ./install.sh --binary ./appdeploy-linux-amd64 --ssh-key-file ./id_ed25519.pub
```

The script installs git, Docker and Caddy, puts the binary in `/usr/local/bin/appdeploy`,
creates a user `deploy` whose only SSH ability is to run appdeploy, and starts two services:
`appdeployd` (the agent) and `appdeploy-caddy`. It is safe to run again. Keys you pass with
`--ssh-key` or `--ssh-key-file` are **admins**; add teammates with fewer rights using
`--deployer-key` (can push, deploy, roll back, read logs) or `--viewer-key` (can only look).
See [Operations: security](operations.md#security).

It does not change your firewall or `sshd_config`. Allow 22, 80 and 443 and nothing else.

Check that it works from your computer:

```bash
appdeploy --host deploy@my-server ping
```

No output and exit status 0 means the agent answered.

Typing `--host` every time is optional. Say where your server is once, in one of these places
(details in [Commands](commands.md#choosing-the-server)):

```bash
appdeploy hosts add my-server deploy@my-server    # saved on this computer; the first one is the default
# or: host: deploy@my-server        in appdeploy.yml of the project
# or: git remote add prod deploy@my-server:myapp   (the remote named prod or appdeploy)
appdeploy ping                                      # now without --host
```

The rest of this guide assumes you saved the server with `appdeploy hosts add`; without that, put
`--host deploy@my-server` after `appdeploy` in each command.

## 3. What your app must provide

appdeploy runs your app as a container built from your Dockerfile, and needs three things:

1. **A `Dockerfile` at the repository root** (or set `build.dockerfile` / `build.context` in
   [`appdeploy.yml`](configuration.md)). There are no buildpacks.
2. **Listen on `$PORT`** (default 8080; or `EXPOSE` a port in the Dockerfile, or set
   `roles.web.port`). Listen on all interfaces, not just `localhost`.
3. **Answer `GET /up` with a 2xx or 3xx status** once the app is ready. The new version only
   gets traffic after this passes twice in a row. Use another path with
   `roles.web.healthcheck.path`.

Also good to know: stop cleanly on `SIGTERM` (you get 30 seconds, `roles.web.stop_timeout`),
keep state in a database or in a named volume (`roles.<name>.volumes`), not in the container's
own file system, and read configuration from environment variables (`env:` in `appdeploy.yml`
for the ones that are not secret). A minimal example is in
[`examples/hello`](../examples/hello).

## 4. Create the app and deploy

```bash
appdeploy apps create myapp --domain myapp.example.com
```
```
created app myapp (deploy branch main)
  git remote add prod <user>@<server>:myapp
  git push prod main
```

In your project:

```bash
git remote add prod deploy@my-server:myapp
git push prod main
```

You see the deploy as it happens, prefixed with `remote:`. The first deploy builds the image
and takes a while:

```
remote: queued (push)
remote: ==> validating
remote: commit 7e27d62d (no appdeploy.yml: using defaults)
remote: memory: 0B reserved + 512.0MiB for this release of 13.3GiB available
remote: ==> building
remote: ==> starting
remote: started ad-myapp-v1-web-1 (role web)
remote: ==> verifying
remote: ad-myapp-v1-web-1: healthcheck GET /up ok
remote: ==> switching
remote: ==> succeeded: myapp is now serving v1
```

Visit `https://myapp.example.com`. The certificate is requested on the first request and can
take a few seconds.

**Important:** `git push` exits with status 0 even when the deploy fails, because git ignores
what the server's hook reports after accepting your push. Read the `remote:` lines, or in
scripts and CI use `appdeploy deploy` (below), which exits non-zero on failure.

Watch the state at any time:

```bash
appdeploy status myapp
```
```
myapp  release v1  domains: myapp.example.com

CONTAINER          ROLE  RELEASE  STATE
ad-myapp-v1-web-1  web   v1       running

Recent deploys:
2026-11-14 03:09:06  v1  push  succeeded
```

## 5. Configure it

Put a file `appdeploy.yml` in the repository root only when the defaults do not fit. Every
setting is optional ([full reference](configuration.md)):

```yaml appdeploy.yml
roles:
  web:
    memory: 256M
    healthcheck:
      path: /healthz
  worker:
    cmd: worker            # the image's ENTRYPOINT gets this as arguments
    memory: 128M
release_command: migrate   # runs once, before traffic moves
services:
  postgres: true           # gives the app DATABASE_URL and daily backups
```

Secrets and environment variables are set from the command line and stored on the server:

```bash
appdeploy secrets set myapp SESSION_SECRET=... STRIPE_KEY=...
```

This makes a new release (without rebuilding) so the running app picks them up; add
`--no-deploy` to store them for the next release instead.

## 6. Day to day

```bash
git push prod main                                   # deploy
appdeploy logs myapp -f      # follow logs
appdeploy releases myapp     # list releases
appdeploy rollback myapp     # back to the previous release
appdeploy exec myapp -- <command>   # run a command in the running container (no terminal)
```

For deploying from CI, see [Operations: deploying from CI](operations.md#deploying-from-ci).

## Next

- How it works, and why a deploy takes about three minutes: [Concepts](concepts.md)
- All commands: [Commands](commands.md)
- More than one server: [Server groups](groups.md)
- Something went wrong: [Troubleshooting](troubleshooting.md)
- Coming from another tool: [Migration](migration.md)
