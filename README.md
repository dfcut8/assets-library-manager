# Asset Library Manager

Asset Library Manager is a local, loopback-only web application for organizing image assets. This repository currently provides the runnable Go foundation: executable-relative configuration, safe SQLite bootstrap and migrations, a Codex subscription preflight, a status page, browser launch, and graceful shutdown. Importing and catalog editing are later milestones.

## Run a binary release

The application binary needs no installer, database server, Node.js runtime, or Go toolchain. Subscription-backed AI processing requires a separately installed Codex CLI authenticated with ChatGPT.

1. Install the [Codex CLI](https://learn.chatgpt.com/docs/codex/cli) and confirm `codex --version` succeeds.
2. Run `codex login`, choose **Sign in with ChatGPT**, and confirm `codex login status` reports a ChatGPT login rather than an API key.
3. Create an empty directory and copy `asset-library-manager` (or `asset-library-manager.exe`) into it.
4. Optionally copy and edit `config.example.json` as `config.json` in the same directory. Change `codex.command` only when `codex` is not available on `PATH`.
5. Run the binary.

On first run, a missing `config.json` is generated with safe defaults. The application creates `incoming/`, `processed/`, `processed/.staging/`, and a fully migrated `assets.db`, then launches a short-lived `codex app-server` preflight, listens at `http://127.0.0.1:7342/`, and opens the default browser. Set `server.open_browser` to `false` to disable browser launch.

The preflight completes the App Server handshake and reads its account state. Processing is ready only for ChatGPT authentication. Missing Codex, a signed-out session, API-key authentication, or another provider produces an actionable warning but does not prevent the existing catalog from starting. Asset Library Manager never reads or stores Codex credentials and intentionally ignores `OPENAI_API_KEY`. The Codex desktop app does not need to be running.

Important database safety rule: if `assets.db` is missing while a managed file or any staging entry remains under `processed/`, startup exits non-zero and does not create an empty replacement catalog. Restore the database or move the processed data to a safe location first. An existing empty, corrupt, or incompatible database is also preserved and reported, never replaced.

See [Running and operations](docs/running.md) for platform commands, generated files, configuration, backups, and source-development instructions. The full behavior contract is in the [software design document](docs/software-design-document.md).

## Develop from source

Prerequisites: Go 1.26.5 or newer within the Go 1.26 line, and Git. Then run:

```sh
go mod download
go test ./...
go test -tags=integration -count=1 ./...
CGO_ENABLED=0 go build ./cmd/asset-library-manager
```

Use `make check` on Unix-like systems for the local quality suite.

On Windows, `make install` builds a CGO-free Windows/amd64 executable and installs it with the example configuration in `D:\assets-library`. The target creates `config.json` from the example only when it is missing, so subsequent installs preserve local settings.
