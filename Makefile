.PHONY: test vet lint build-cli build-trayapp build-trayapp-windows help

help:
	@echo "Available targets:"
	@echo "  test              - Run all tests"
	@echo "  test-verbose      - Run all tests with verbose output"
	@echo "  vet               - Run go vet"
	@echo "  lint              - Run go vet + gofmt check (gofmt must be installed separately)"
	@echo "  build-cli         - Build CLI binary for Linux"
	@echo "  build-trayapp     - Build trayapp binary for Linux (headless server mode)"
	@echo "  build-trayapp-windows - Cross-compile trayapp for Windows (requires mingw-w64)"
	@echo "  clean             - Remove built binaries"

test:
	go test ./...

test-verbose:
	go test -v ./...

vet:
	go vet ./...

lint: vet
	@echo "gofmt check (add gofmt installation as needed)"
	gofmt -d .

build-cli:
	GOOS=linux GOARCH=amd64 go build -o clusage-cli ./cmd/cli

build-trayapp:
	GOOS=linux GOARCH=amd64 go build -o trayapp ./cmd/trayapp

build-trayapp-windows:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=1 CC=x86_64-w64-mingw32-gcc CXX=x86_64-w64-mingw32-g++ go build -o trayapp.exe ./cmd/trayapp

clean:
	rm -f clusage-cli trayapp trayapp.exe

ci: vet test build-cli build-trayapp
	@echo "CI checks passed"
