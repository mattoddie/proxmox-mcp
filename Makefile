VERSION ?= 0.0.0-dev
LDFLAGS := -s -w -X github.com/mattoddie/proxmox-mcp/internal/tools.Version=$(VERSION)
export CGO_ENABLED := 0

.PHONY: build test lint docs docs-check vulncheck image clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/proxmox-mcp ./cmd/proxmox-mcp

test:
	go test ./...

lint:
	gofmt -l . | (! grep .) || (echo "Run gofmt -w ." && exit 1)
	go vet ./...

docs:
	go run ./cmd/gen-tool-docs

docs-check:
	go run ./cmd/gen-tool-docs -check

vulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

image:
	docker build --build-arg VERSION=$(VERSION) -t proxmox-mcp:$(VERSION) .

clean:
	rm -rf bin
