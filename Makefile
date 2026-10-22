VERSION ?= 0.0.0-dev
LDFLAGS := -s -w -X github.com/mattoddie/proxmox-mcp/internal/tools.Version=$(VERSION)
export CGO_ENABLED := 0

# Platforms for the MCP Bundles (one .mcpb each), as GOOS/GOARCH.
MCPB_PLATFORMS := darwin/arm64 darwin/amd64 windows/amd64 windows/arm64 linux/amd64 linux/arm64

.PHONY: build test lint docs docs-check vulncheck image mcpb server-json clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/proxmox-mcp ./cmd/proxmox-mcp

test:
	go test ./...

lint:
	gofmt -l . | (! grep .) || (echo "Run gofmt -w ." && exit 1)
	go vet ./...

docs:
	go run ./cmd/gen-tool-docs
	go run ./cmd/mcpb manifest

docs-check:
	go run ./cmd/gen-tool-docs -check
	go run ./cmd/mcpb manifest -check

vulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

image:
	docker build --build-arg VERSION=$(VERSION) -t proxmox-mcp:$(VERSION) .

# MCP Bundles for Claude Desktop: dist/proxmox-mcp-$(VERSION)-<os>-<arch>.mcpb.
mcpb:
	rm -rf .build/mcpb dist/*.mcpb
	go build -o .build/mcpb/pack ./cmd/mcpb
	set -e; for p in $(MCPB_PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; \
		if [ "$$os" = windows ]; then ext=.exe; fi; \
		GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o .build/mcpb/$$os-$$arch/proxmox-mcp$$ext ./cmd/proxmox-mcp; \
		.build/mcpb/pack pack -version $(VERSION) -goos $$os -binary .build/mcpb/$$os-$$arch/proxmox-mcp$$ext \
			-o dist/proxmox-mcp-$(VERSION)-$$os-$$arch.mcpb; \
	done

clean:
	rm -rf bin .build dist

# server.json for the MCP Registry, stamped with VERSION and the SHA-256 of each bundle in dist/
# (from `make mcpb`, or downloaded from the release): dist/server.json.
define SERVER_JSON_JQ
.version = $$v
| .packages |= map(
    .identifier |= gsub("0\\.0\\.0-dev"; $$v)
    | if .registryType == "mcpb" then
        (.identifier | split("/") | last) as $$file
        | .fileSha256 = ($$sums[$$file] // error("dist/\($$file) is missing"))
      else . end)
| if ([.packages[] | select(.registryType == "mcpb")] | length) != ($$sums | length)
  then error("dist/ has bundles that server.json doesn't list") else . end
endef
export SERVER_JSON_JQ

server-json:
	@test "$(VERSION)" != 0.0.0-dev || { echo "Set VERSION, e.g. make server-json VERSION=1.2.3"; exit 1; }
	cd dist && sha256sum -- *.mcpb | jq -Rn '[inputs | capture("^(?<sum>[0-9a-f]{64}) [ *](?<file>.+)$$") | {key: .file, value: .sum}] | from_entries' > sums.json
	jq --arg v "$(VERSION)" --slurpfile s dist/sums.json '$$s[0] as $$sums | '"$$SERVER_JSON_JQ" server.json > dist/server.json
	rm dist/sums.json
	@echo "Wrote dist/server.json for $(VERSION)."
