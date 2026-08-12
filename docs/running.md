# Running Asset Library Manager

## Release prerequisites

A release is a single CGO-free executable. You need:

- Windows, macOS, or Linux on a supported release architecture.
- Permission to create files beside the executable.
- A graphical browser only if automatic browser launch is enabled.
- An OpenAI API key only for future AI categorization; startup and the local catalog do not require one.

There is no installer and no runtime dependency on Go, Node.js, a separate database server, or environment variables.

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
- `openai.api_key`: the only supported OpenAI API-key source. `OPENAI_API_KEY` is ignored.

Because `config.json` contains a credential, do not commit it, share it, or include it in diagnostics. Restrict access to the application directory using operating-system permissions. The application redacts the key from structured configuration logs and never sends it to the browser or stores it in SQLite.

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
3. Copy `config.json` separately using secret-safe storage if you need to preserve the API key.
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
