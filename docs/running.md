# Running Asset Library Manager

## Release prerequisites

A release is a single CGO-free executable. You need:

- Windows, macOS, or Linux on a supported release architecture.
- Permission to create files beside the executable.
- A graphical browser only if automatic browser launch is enabled.
- A compatible [Codex CLI](https://learn.chatgpt.com/docs/codex/cli) on `PATH` and signed in with ChatGPT for subscription-backed AI processing.

There is no application-side runtime dependency on Go, Node.js, a separate database server, or environment variables. Codex is a separate host prerequisite for AI processing; the catalog remains available when it is missing or unavailable.

## Prepare Codex subscription access

1. Install or update the Codex CLI using the official platform-specific instructions.
2. Confirm the command is visible to the process environment that will launch Asset Library Manager:

   ```text
   codex --version
   ```

3. Sign in with the ChatGPT account whose subscription allowance should be used:

   ```text
   codex login
   ```

   Complete the browser flow and choose ChatGPT subscription access. Do not select API-key authentication.

4. Verify the cached login:

   ```text
   codex login status
   ```

Asset Library Manager starts `codex app-server --listen stdio://` itself. The Codex desktop app and a manually started App Server do not need to be running. Codex owns credential storage and refresh; Asset Library Manager never reads its credential files or operating-system keyring entries.

## Binary-only setup

Create an empty application directory, place the executable there, and run it from any working directory. All runtime paths are resolved relative to the executable itself.

Windows PowerShell:

```powershell
New-Item -ItemType Directory -Path C:\Apps\AssetLibraryManager
Copy-Item .\asset-library-manager.exe C:\Apps\AssetLibraryManager\
& C:\Apps\AssetLibraryManager\asset-library-manager.exe
```

macOS or Linux:

```sh
mkdir -p "$HOME/Applications/asset-library-manager"
cp ./asset-library-manager "$HOME/Applications/asset-library-manager/"
chmod 700 "$HOME/Applications/asset-library-manager/asset-library-manager"
"$HOME/Applications/asset-library-manager/asset-library-manager"
```

If `config.json` is absent, the application publishes a default file atomically and continues. Review that file after the first run.

## Binary-plus-config setup

Copy `config.example.json` beside the executable as `config.json`, edit it, and start the executable. JSON parsing is strict: unknown or duplicate fields, trailing values, unsafe paths, and invalid limits stop startup without overwriting the file.

The important settings are:

- `server.host`: must be an IPv4 or IPv6 loopback address.
- `server.port`: the local HTTP port; default `7342`.
- `server.open_browser`: opens the local URL after successful initialization; default `true`. A launch failure is logged but does not stop the server.
- `storage.*`: normalized paths beneath the executable directory. Use `/` as the separator on every platform (for example, `data/assets.db`, including on Windows).
- `codex.command`: Codex executable name or explicit path; default `codex`.
- `codex.model` and `codex.reasoning_effort`: model settings for future image-analysis turns.
- `codex.startup_timeout_seconds`: maximum time for the startup App Server/authentication preflight.
- `codex.turn_timeout_seconds`, `codex.max_attempts`, and `codex.initial_retry_delay_ms`: bounds for future analysis turns.

`config.json` contains no OpenAI credential. Keep it out of source control because it still describes local paths and runtime policy. `OPENAI_API_KEY` is ignored.

## Startup preflight and status

After configuration, storage, and SQLite initialize, Asset Library Manager performs a bounded preflight:

1. Resolve `codex.command` through `PATH` or use its explicit path.
2. Start a short-lived App Server over stdio.
3. Complete the protocol `initialize`/`initialized` handshake.
4. Call `account/read` and require account type `chatgpt`.
5. Cancel and join the preflight process.

The status page reports one of these outcomes:

| Status | Meaning and action |
| --- | --- |
| Codex subscription ready | The App Server responded and is signed in with ChatGPT. |
| Codex sign-in required | Run `codex login`, complete ChatGPT login, and restart the application. |
| Codex is using an API key | Run `codex logout`, then `codex login` and choose ChatGPT subscription access. |
| Unsupported Codex account | Switch Codex to a ChatGPT account. |
| Codex unavailable | Install/update Codex, fix `codex.command` or `PATH`, and restart. Inspect the `codex preflight failed` structured log for bounded technical details. |

Every non-ready outcome blocks only new AI processing. The HTTP server, existing catalog, and recovery-safe local state still start.

## Generated artifacts and startup behavior

A successful first start produces:

```text
<application-directory>/
  asset-library-manager[.exe]
  config.json
  assets.db
  assets.db-shm          # may exist while running
  assets.db-wal          # may exist while running
  incoming/
  processed/
    .staging/
```

Before HTTP starts, the application creates and migrates SQLite, verifies migration checksums, enables and verifies WAL, `synchronous=FULL`, foreign keys, bounded busy handling, an integrity check, and FTS5. Existing database failures are fatal and the file is never silently replaced.

Missing-database behavior is deliberately conservative:

| State | Result |
| --- | --- |
| Fresh directory or no `processed/` directory | Create directories and a fully migrated database. |
| Existing but empty runtime directories | Create the database. |
| Only an empty `processed/.staging/` exists | Create the database. |
| A managed file exists at any depth under `processed/` | Exit non-zero before creating the database. |
| Any file, directory, symlink, or special entry exists inside `.staging/` | Exit non-zero before creating the database. |
| An existing database is empty, corrupt, unreadable, incompatible, or has a migration checksum conflict | Preserve it and exit non-zero. |

The refusal message explains that catalog metadata may have been lost and instructs you to restore `assets.db` or move the processed data to a safe location. The application does not alter the retained processed entries during this check.

After initialization the server logs its URL to standard error and, by default, asks the operating system to open it. Stop the process with Ctrl+C or a normal termination signal; it drains HTTP, checkpoints WAL, and closes SQLite within the configured timeout.

`--version` prints build information without creating configuration, directories, or a database.

## Backup and recovery

Treat `assets.db` and `processed/` as one inseparable backup set. For a consistent manual backup:

1. Stop the application normally.
2. Copy `assets.db` and the complete `processed/` directory together.
3. Copy `config.json` separately if you need to preserve local runtime settings. Codex credentials are not stored in the application directory and must not be added to this backup.
4. Restart the application.

Do not back up only `assets.db` while the process is running; WAL data may not yet be in the main file. If the database is lost but processed files remain, restore the matching database backup. Moving processed files away allows an intentional fresh catalog, but this milestone does not re-import those managed files automatically.

## Build and test from source

Prerequisites:

- Go 1.26.5 or newer within the Go 1.26 line. The module toolchain directive can download the pinned patch automatically when Go toolchain switching is enabled.
- Git.
- `golangci-lint` v2 for linting.
- `govulncheck` for the vulnerability gate.
- GNU Make only if using the provided Makefile; all commands can be run directly.

PowerShell:

```powershell
$env:CGO_ENABLED = '0'
go mod download
go test ./...
go test -tags=integration -count=1 ./...
go vet ./...
go build -trimpath -o asset-library-manager.exe ./cmd/asset-library-manager
```

macOS or Linux:

```sh
export CGO_ENABLED=0
go mod download
go test ./...
go test -tags=integration -count=1 ./...
go vet ./...
go build -trimpath -o asset-library-manager ./cmd/asset-library-manager
```

Additional quality commands are `go test -race ./...`, `golangci-lint run`, `go mod verify`, and `govulncheck ./...`. Cross-builds keep `CGO_ENABLED=0` and set `GOOS` and `GOARCH` for the target.

### Opinionated Windows install

From the repository root, run:

```powershell
make install
```

This builds a CGO-free Windows/amd64 executable and copies it to `D:\assets-library\asset-library-manager.exe`. The target always refreshes `config.example.json` in that directory. It creates `config.json` from the example only when `config.json` does not already exist, preserving local settings on later installs.
