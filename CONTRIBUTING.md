# Contributing to clagentic-router

## Build and test

```bash
make tidy      # go mod tidy
make build     # produces bin/clagentic-router
make test      # go test ./...
make smoke     # end-to-end smoke test (requires a running daemon)
```

All PRs must pass `make test` and `go vet ./...` clean before review.

## Module path

The module path is `github.com/clagentic/clagentic-router`.

## Branch naming

| Prefix | Use for |
|---|---|
| `feat/` | New functionality |
| `fix/` | Bug fixes |
| `chore/` | Maintenance, dependency bumps, tooling |
| `docs/` | Documentation-only changes |
| `refactor/` | Code restructuring without behaviour change |

Include the task ID when one exists, e.g. `feat/lr-1234-add-retry`.

## Pull request expectations

- Tests are required for every bug fix and every new code path.
- Do not modify existing tests to make them pass — fix the code.
- `go vet ./...` must be clean.
- No new dependencies without explicit sign-off (see `go.mod`).
- No hardcoded paths, hostnames, or secrets.

## Import graph rule

The import graph has a strict acyclic structure. Adding an import that creates a cycle
is a hard error that will be caught by `go build`.

```
config  -> (stdlib)
state   -> (stdlib)
store   -> state
backend -> config
webhook -> state, store
router  -> backend, config, state, store, webhook
server  -> router, state, store
cmd/clagentic-router -> config, backend, router, server, store, webhook
```

Two additional hard constraints enforced at review:

- `webhook` must **never** import `router`
- `backend` must **never** import `store` or `state`

Violations of these constraints must not be merged regardless of build success.

## Code style

- Match the existing style. Run `gofmt` before committing.
- Comments explain why, not what.
- No bare `error` returns without context — wrap with `fmt.Errorf("...: %w", err)`.
- No `//nolint` directives without a justification comment on the same line.
