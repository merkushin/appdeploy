# Troubleshooting

Start with these, on your computer and on the server:

```bash
appdeploy --host deploy@my-server status myapp          # what runs, and how the last deploys ended
appdeploy --host deploy@my-server logs myapp --tail 50
ssh root@my-server systemctl status appdeployd appdeploy-caddy
ssh root@my-server journalctl -u appdeployd -n 100 --no-pager
```

A deploy that fails **before traffic moved** (`failed`) leaves the old release serving and cleans
up after itself, so a failure is usually not an outage. The error message is the last `deploy failed:`
line of the output.

## The deploy fails

| Message (part of it) | What it means | What to do |
|---|---|---|
| `healthcheck GET /up did not pass within 1m0s` | The new container never answered 2xx/3xx on that path. The last lines of its log are printed above. | Check the app listens on `$PORT` on all interfaces, serves `/up` (or set `roles.web.healthcheck.path`), and starts within `start_period`. |
| `... exited (code 1)` / `exited right after starting` | The container crashed. Its last log lines are printed. | Fix the crash; reproduce with `docker run` locally using the same environment variables. `killed for exceeding its memory limit` means raise `roles.<name>.memory`. |
| `build failed: ...` | `docker build` failed; the end of its output is in the message and the full output above it. | Fix the Dockerfile; the build ran on the server, so base images must be reachable from it. |
| `Dockerfile not found in the repository (build.dockerfile)` | No file at `build.dockerfile` inside `build.context`. | Check the paths in `appdeploy.yml`; they are relative to the context, which is relative to the repository root. |
| `release command failed` | `release_command` exited non-zero, so nothing started. Its output is above. | Fix the migration. Nothing changed for users. |
| `not enough memory: ... already reserved by running containers + ... needed > ...` | The memory limits of all containers on the server, including the new release's and the old one still running, do not fit in RAM minus a reserve (15% or 512 MB). | Lower limits, remove apps, or use a bigger server. Remember both releases run at once during a deploy. |
| `not enough disk space to build` | Free disk is below 5 GiB or 10%. | Free space (`docker system df`, `docker image prune`) or lower the thresholds (`--min-free-bytes`, `--min-free-frac`). |
| `appdeploy.yml: ... field xyz not found` | An unknown key, usually a typo. | Fix it; keys are listed in [Configuration](configuration.md). |
| `invalid duration "..."` / `invalid size "..."` | A value is wrong. | Use `5s`, `3m`, `512M`, `1G`. |
| `git ref "..." not found in the app repository` | `deploy --ref X` with a commit the server does not have. | Push it first (to a branch that does not deploy). |
| `superseded by ...` | A newer deploy of the same app replaced this one while it waited. | Nothing: the newer one runs. |
| `gave up after N attempts interrupted by agent restarts` | The agent kept restarting during this deploy. | Look at `journalctl -u appdeployd`; deploy again. |
| `pulling X: ... (private registries need docker login on the server)` | `deploy --image` could not pull it. | `docker login` on the server for a private registry; check the name and tag. |
| `invalid image reference` | The `--image` value has unexpected characters. | Use `registry/name:tag`. |

## The deploy succeeded, then rolled back

State `rolled_back`; the last lines say `watchdog: ...`:

| Message | Cause |
|---|---|
| `N of M requests (x%) failed with 5xx, above the y% limit` | The new version returned errors under real traffic. Look at its logs (`logs` shows the live release only, so read the journal above or reproduce), fix, deploy again. |
| `... restarted N times` / `... exited` | A container crashed after it passed the checks (a crash loop, memory limit). |
| `health check GET /up failed 3 times in a row` | The app stopped answering after the switch. |

Raise `watch.error_rate` or lower `watch.min_requests` only if the thresholds do not suit your
traffic. A single failed request does not trigger anything.

## Commands

| Message | Cause and fix |
|---|---|
| `cannot reach the agent at /run/appdeploy/agent.sock` | The agent is not running (`systemctl status appdeployd`) or you cannot use the socket (it is root's, or a group's, see `--socket-group`). Over SSH this appears on the server's side. |
| `Permission denied (publickey)` | Your key is not in `/home/deploy/.ssh/authorized_keys`; run `install.sh` again with `--ssh-key-file` / `--deployer-key` / `--viewer-key`. |
| `"deploy" is not allowed for the viewer role` | Your key's role is too low; see [Operations: security](operations.md#security). |
| `interactive shells are not available` | You ran `ssh deploy@server` without a command. Use `appdeploy --host ...`. |
| `unexpected option "--host"` | You passed a client option inside an SSH command. |
| `no such app "x" (create it with: appdeploy apps create x)` | `git push` to an app that does not exist. |
| `app "x" already exists` | `apps create` of an existing name. |
| `... is not the deploy branch (main): not deploying` | You pushed another branch. That is by design. |
| `release vN of x not found (it may have been pruned)` | `rollback --to N` of a release beyond `keep_releases`. |
| `there is no earlier release to roll back to` | Only one release exists. |
| `vN is already the current release` | `rollback --to N` of the live release. |
| `app "x" has nothing deployed yet` | `redeploy` before the first deploy. |
| `app "x" has a deploy in progress` (apps destroy) | Wait for it. |
| `docker is not reachable` (agent start) | Docker is not running: `systemctl status docker`. |

## HTTPS and domains

- **No certificate / browser warning:** the domain must resolve to this server and ports 80 and 443
  must be reachable from the internet. Look at `journalctl -u appdeploy-caddy` for the
  Let's Encrypt error; wait out rate limits rather than retrying.
- **`404 no such app`:** the host name is not one of the app's domains (`appdeploy status myapp` shows
  them), or the app has no release yet.
- **`502`/connection errors right after a deploy:** check `status`; a container that is not `running`
  is the cause.

## Groups

| Message | Cause and fix |
|---|---|
| `server X is not reachable (...)` | The coordinator cannot reach X's peer port. Start it, fix the firewall, or set `allow_partial: true`. |
| `server X speaks protocol N and this one M` | The group is in the middle of an update. Wait until `group servers` shows one version. |
| `no server of the group carries the labels the roles ask for` | `roles.*.on` matches no server's labels. |
| `services (postgres/redis) are only available when every role runs on the coordinating server` | Use an external `DATABASE_URL` secret instead. |
| `rollout stopped in wave N` | A server failed in wave N; the ones before it were taken back. The line before says why. |
| `invalid or expired invite` | Tokens work once for one hour: run `group invite` again. |
| `the server's CA does not match the invite (wrong server?)` | You are joining a different group than the invite came from. |
| `only the server that created the group can ...` | `invite`, `remove` and `set-version` only work on the founder. |
| `waiting for X to update first` (agent log) | The group's update is going one server at a time; it is not stuck. A server that stays on the old version stops the ones behind it. |

## Updates

| Message | Cause and fix |
|---|---|
| `... has <repo> published a release?` | The GitHub repository has no release. |
| `the signature of SHA256SUMS is not valid ... refusing to install` | The release was not signed by the key built into this binary: do not install it. |
| `this binary was built without a release signing key` | A development build cannot verify updates; install a release binary. |
| `N deploy(s) are in progress` | Try again later, or `--force`. |
| `installing X failed here before and was reverted` (agent log) | A group member will not retry a version that failed on it; set another version. |
