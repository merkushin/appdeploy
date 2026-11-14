# HTTP API

The agent serves a JSON API over a **unix socket** (default `/run/appdeploy/agent.sock`). The
CLI is a client of it. Access is by file permission on the socket: root, plus the socket group if
configured. There is no other authentication, and **no TCP listener** for it.

```bash
curl -s --unix-socket /run/appdeploy/agent.sock http://agent/v1/apps
```

Errors are `{"error":"message"}` with status 400 (404 if not found). The API is not versioned
beyond the `/v1` prefix and may change before 1.0; the CLI is the stable interface.

Times are RFC 3339. A deploy's `state` is one of `queued`, `validating`, `building`, `migrating`,
`starting`, `verifying`, `switching`, `observing`, `succeeded`, `failed`, `rolled_back`,
`superseded` (and `distributing`, `rolling` for a coordinator in a group).

## Apps

| Request | Body | Answer |
|---|---|---|
| `GET /v1/ping` | | `{"ok":true}` |
| `POST /v1/apps` | `{"name","domains":[],"branch"}` | 201, the app: `{"name","domains","branch","current_release","created_at"}` |
| `GET /v1/apps` | | list of apps |
| `GET /v1/apps/{app}` | | `{"app":{...},"containers":[{"name","role","release","running","restarts","host_port"}],"deploys":[{"id","app","release","state","error","ref","source","started_at","finished_at"}],"releases":[{"number","commit","image","created_at","current"}]}` |
| `DELETE /v1/apps/{app}` | query `volumes=1` also deletes the volumes | `{"ok":true,"kept_volumes":[...]}` (destructive) |
| `PUT /v1/apps/{app}/domains` | `{"domains":[]}` | `{"ok":true}` |
| `GET /v1/apps/{app}/releases` | | list of releases |
| `GET /v1/apps/{app}/export?format=kamal` | | `{"files":{"path":"content"}}` |

## Deploying

Each of these answers `202 {"deploy_id":"..."}` and returns at once; follow the deploy with the
events endpoint.

| Request | Body |
|---|---|
| `POST /v1/apps/{app}/deploy` | `{"ref":"sha-or-ref"}` or `{"image":"ref","config_yaml":"..."}` |
| `POST /v1/apps/{app}/redeploy` | none |
| `POST /v1/apps/{app}/rollback` | `{"to":N}` (0 or absent: the previous release) |
| `POST /v1/apps/{app}/push` | `{"ref":"refs/heads/main","sha":"..."}`; called by the git hook; answers 200 `{"message"}` if the ref is not the deploy branch |
| `GET /v1/deploys/{id}` | the deploy, as above |
| `GET /v1/deploys/{id}/events?after=N` | newline-delimited JSON: `{"id","time","msg"}` for each journal line, ending with `{"done":true,"state","error"}` |

The `time` field is absent on the last line. The events stream is how to wait: read until a line with `"done":true` and check `state`
(`succeeded` is success). Reconnect with `after=<last id>` if the connection drops; the deploy
does not depend on it.

## Secrets, logs, commands

| Request | Body | Answer |
|---|---|---|
| `GET /v1/apps/{app}/secrets` | | `{"names":[...]}` |
| `GET /v1/apps/{app}/secrets?reveal=1` | | `{"values":{"KEY":"value"}}` (secret values) |
| `POST /v1/apps/{app}/secrets` | `{"set":{"K":"v"},"unset":["K"],"deploy":true}` | `{"deploy_id"}` if `deploy` and the app has a release |
| `GET /v1/apps/{app}/logs?role=web&follow=1&tail=100` | | text, lines prefixed with the container name |
| `POST /v1/apps/{app}/exec` | `{"role":"web","cmd":["ls","/"]}` | the output as a stream; the exit status is in the HTTP trailer `Appdeploy-Exit` |

## Databases

| Request | Body | Answer |
|---|---|---|
| `POST /v1/apps/{app}/db/backup` | | `{"path"}` |
| `GET /v1/apps/{app}/db/backups` | | `["name",...]`, newest first |
| `POST /v1/apps/{app}/db/restore` | `{"file":"name"}` | `{"ok":true}`; replaces the database's tables |

## Group and updates

| Request | Body | Answer |
|---|---|---|
| `GET /v1/group` | | members and wanted version; 400 if not in a group |
| `GET /v1/group/versions` | | `{"desired","servers":[{"name","addr","labels","version","reachable","busy","error"}]}` |
| `POST /v1/group/invite` | | `{"token"}` (founder) |
| `POST /v1/group/version` | `{"version":"v1.2.3"}` or `""` | the group state (founder) |
| `DELETE /v1/group/servers/{name}` | | the group state (founder) |
| `POST /v1/self-update` | `{"check":false,"force":false,"repo":""}` | `{"log":[...],"from","to","updated","error"}`; after `updated`, the agent restarts in a second |

## Health endpoint

With `--health-listen`, a separate TCP listener answers `GET /health/{app}` with 200 `ok` when this
server runs a web container of the app's live release, else 503 (`not serving`, or `switching`
while traffic is being moved). Nothing else, and no authentication, for load balancers.

## Peer API (between servers)

Served over mutual TLS on the peer port; not for you to call. It is listed so that you know what
a group exposes: `POST /peer/v1/join`, `GET /peer/v1/ping`, `GET /peer/v1/version`,
`GET /peer/v1/group`, `PUT /peer/v1/group`, `POST /peer/v1/image`, `GET /peer/v1/image`,
`POST /peer/v1/source`, `POST /peer/v1/deploy`, `GET /peer/v1/deploys/{id}/events`,
`GET /peer/v1/finals`, `PUT /peer/v1/final`, `GET /peer/v1/release`.
