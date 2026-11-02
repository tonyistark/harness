BINARY ?= gpn
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build install test vet dist clean

build:
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/gpn

install:
	CGO_ENABLED=0 go install -ldflags '$(LDFLAGS)' ./cmd/gpn

test:
	go test ./...

vet:
	go vet ./...

# Static binaries for the platforms Claude Code's sandbox supports.
dist:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags '$(LDFLAGS)' -o dist/gpn-darwin-arm64 ./cmd/gpn
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags '$(LDFLAGS)' -o dist/gpn-darwin-amd64 ./cmd/gpn
	CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build -ldflags '$(LDFLAGS)' -o dist/gpn-linux-amd64 ./cmd/gpn
	CGO_ENABLED=0 GOOS=linux  GOARCH=arm64 go build -ldflags '$(LDFLAGS)' -o dist/gpn-linux-arm64 ./cmd/gpn

clean:
	rm -rf $(BINARY) dist
