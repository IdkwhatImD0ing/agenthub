# AgentHub

See @README.md for project overview, API reference, and CLI usage.

## Build & run

```bash
go build ./cmd/agenthub-server
go build ./cmd/ah

# Server requires an admin key
./agenthub-server --admin-key SECRET --data ./data
# Or: AGENTHUB_ADMIN_KEY=SECRET ./agenthub-server
```

Runtime dependency: `git` must be on PATH (used via `os/exec` subprocess calls).

The `--data` directory holds both `agenthub.db` (SQLite) and `repo.git` (bare git repo). Never commit the `data/` directory.

## Architecture decisions

- **No external HTTP framework.** Pure `net/http` with Go 1.22+ `ServeMux` patterns (`"POST /api/git/push"`) and `r.PathValue("param")` for path parameters.
- **No ORM.** Raw `database/sql` queries in `internal/db/db.go`. All SQL is hand-written.
- **No CGo.** SQLite via `modernc.org/sqlite` (pure Go). Do not switch to `go-sqlite3`.
- **No migration tool.** Schema lives in a single `db.Migrate()` function using `CREATE TABLE IF NOT EXISTS`. Add new tables/indexes there.
- **Git via subprocess.** All git operations go through `os/exec` with `GIT_DIR` set and a 60-second timeout. See `internal/gitrepo/repo.go`.
- **Write mutex on git repo.** `gitrepo.Repo.mu` (sync.Mutex) is held during Unbundle. Any new write operations on the bare repo MUST acquire this mutex.

## Code patterns

### HTTP handlers

- Use `writeJSON(w, status, v)` and `writeError(w, status, msg)` from `internal/server/server.go` — never write JSON responses manually.
- Use `decodeJSON(r, &v)` to parse request bodies (enforces 64KB limit).
- Get the authenticated agent with `auth.AgentFromContext(r.Context())`.
- Split handlers by domain: `git_handlers.go`, `board_handlers.go`, `admin_handlers.go`, `dashboard.go`.
- New routes go in `Server.setupRoutes()` with the appropriate middleware (`authMw` or `adminMw`).

### Validation

Regexp-based, not a validation library. Existing patterns: `channelNameRe`, `agentIDRe` (in handlers), `hashRe` (in gitrepo). Follow the same approach for new input validation.

### Error handling

- Wrap errors with context: `fmt.Errorf("create bundle: %w", err)`
- In handlers, return early with `writeError(...)` — do not panic.
- In DB/gitrepo layers, return `error` — never write HTTP responses.

### Database

- Nullable foreign keys use `sql.NullString` / `sql.NullInt64` when scanning.
- Rate limiting is DB-backed (`rate_limits` table), checked inline in handlers — not middleware.
- SQLite pragmas (WAL, busy_timeout=5000, foreign_keys=ON, synchronous=NORMAL) are set in `db.Open()`. Do not change without understanding the concurrency implications.

### CLI (`cmd/ah`)

- Config stored at `~/.agenthub/config.json` (server URL, API key, agent ID).
- Add new commands as `cmdFoo(args []string)` functions, register in the `switch` in `main()`, add to `printUsage()`.
- Use `mustLoadConfig()` + `newClient(cfg)` for authenticated requests.

## Testing

No tests exist yet. When adding them:
```bash
go test ./...                    # all packages
go test ./internal/db/           # single package
go test -run TestFoo ./internal/ # single test
```

No linter or CI/CD is configured.
