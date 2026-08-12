# Asset Library Manager

Asset Library Manager is a local, loopback-only web application for organizing image assets. This repository currently provides the runnable Go foundation: executable-relative configuration, safe SQLite bootstrap and migrations, a status page, browser launch, and graceful shutdown. Importing and catalog editing are later milestones.

## Run a binary release

No installer, database server, Node.js runtime, or Go toolchain is required.

1. Create an empty directory and copy `asset-library-manager` (or `asset-library-manager.exe`) into it.
2. Optionally copy and edit `config.example.json` as `config.json` in the same directory.
3. Run the binary.

On first run, a missing `config.json` is generated with safe defaults and an empty `openai.api_key`. The application creates `incoming/`, `processed/`, `processed/.staging/`, and a fully migrated `assets.db`, then listens at `http://127.0.0.1:7342/` and opens the default browser. Set `server.open_browser` to `false` to disable browser launch.

The OpenAI key is read only from `config.json`; `OPENAI_API_KEY` is intentionally ignored. Protect the config file as a secret. The application can start without a key, but future AI categorization will remain unavailable until one is configured.

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
