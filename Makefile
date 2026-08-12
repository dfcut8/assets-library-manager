INSTALL_DIR := D:/assets-library

.PHONY: build install test test-race integration lint vet verify check cross-build clean

build:
	go build -trimpath -o bin/asset-library-manager ./cmd/asset-library-manager

install:
	powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File scripts/install.ps1 -InstallDir "$(INSTALL_DIR)"

test:
	go test -shuffle=on ./...

test-race:
	go test -race -shuffle=on ./...

integration:
	go test -tags=integration -count=1 ./...

lint:
	golangci-lint run ./...

vet:
	go vet ./...

verify:
	go mod verify

check: test integration vet lint verify

cross-build:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -o dist/asset-library-manager-windows-amd64.exe ./cmd/asset-library-manager
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/asset-library-manager-linux-amd64 ./cmd/asset-library-manager
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -o dist/asset-library-manager-darwin-arm64 ./cmd/asset-library-manager

clean:
	go clean
