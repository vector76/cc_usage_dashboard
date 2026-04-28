.PHONY: test test-verbose test-userscript vet lint build-cli build-trayapp build-trayapp-windows release clean ci help

# Common build flags. -s and -w strip the symbol table and DWARF debug info
# (~30% size reduction); -trimpath removes filesystem paths from the binary
# (smaller + reproducible). Pure-Go build (CGO disabled by default thanks to
# modernc.org/sqlite) means cross-compilation needs no C toolchain.
RELEASE_LDFLAGS := -s -w
GOFLAGS         := -trimpath

help:
	@echo "Available targets:"
	@echo "  test                  - Run all Go tests (also see test-userscript)"
	@echo "  test-verbose          - Run all Go tests with verbose output"
	@echo "  test-userscript       - Run userscript JS unit tests (node:test)"
	@echo "  vet                   - Run go vet"
	@echo "  lint                  - Run go vet + gofmt check"
	@echo "  build-cli             - Build CLI binary for Linux (debug)"
	@echo "  build-trayapp         - Build trayapp binary for Linux (debug, headless server mode)"
	@echo "  build-trayapp-windows - Cross-compile trayapp for Windows (debug)"
	@echo "  release               - Build all binaries with size-optimized flags (Linux + Windows)"
	@echo "  clean                 - Remove built binaries"

test:
	go test ./...
	@echo "(userscript JS tests are not run here; use 'make test-userscript')"

test-verbose:
	go test -v ./...

test-userscript:
	npm --prefix userscript test

vet:
	go vet ./...

lint: vet
	gofmt -d .

build-cli:
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -o clusage-cli ./cmd/clusage-cli

build-trayapp:
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -o trayapp ./cmd/trayapp

build-trayapp-windows:
	GOOS=windows GOARCH=amd64 go build $(GOFLAGS) -o trayapp.exe ./cmd/trayapp

# Size-optimized release builds. -H=windowsgui detaches the .exe from the
# console so the trayapp runs purely in the system tray.
release:
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -ldflags="$(RELEASE_LDFLAGS)" -o clusage-cli ./cmd/clusage-cli
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -ldflags="$(RELEASE_LDFLAGS)" -o trayapp ./cmd/trayapp
	GOOS=windows GOARCH=amd64 go build $(GOFLAGS) -ldflags="$(RELEASE_LDFLAGS) -H=windowsgui" -o trayapp.exe ./cmd/trayapp

clean:
	rm -f clusage-cli trayapp trayapp.exe

ci: vet test test-userscript build-cli build-trayapp
	@echo "CI checks passed"
