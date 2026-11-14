# appdeploy

The design lives in [SPEC.md](SPEC.md).

## Language

The repository's language is English: all documentation, code, comments, commit
messages and CLI output are written in English, even when the conversation is in
another language.

## Testing

Test against small Go applications in `examples/` (see SPEC.md §22), not Rails apps.
Tests must clean up what they create: containers, images, volumes, build directories.

## Layout and commands

- `cmd/appdeploy` — the single binary: `agent`, `hook`, `shell`, and the client commands.
- `internal/deploy` — the deploy state machine (`pipeline.go`), queue and crash recovery
  (`engine.go`), secrets and git helpers (`files.go`).
- `internal/store` (SQLite journal), `internal/docker` (docker CLI), `internal/proxy`
  (Caddy admin API), `internal/api` (control API on a unix socket), `internal/config`.
- `internal/group` (membership, private CA, invites, mutual TLS), `internal/deploy/group.go`
  (rollout across servers), `internal/api/peer.go` (peer API and client), `internal/services`
  (Postgres/Redis, backups), `internal/traffic` (access log counters for the watchdog),
  `internal/notify` (webhooks).
- `examples/` — the Go apps used by the tests (`hello`, `broken`); each is its own module.
- `go test ./...` for unit tests; `go test -tags e2e -timeout 30m ./e2e` for end-to-end
  tests (needs Docker; they clean up everything they create, labeled `appdeploy.instance`).
- Docs: SPEC.md is the design; §23-§25 record what is implemented and where it differs.
