.PHONY: test build gateway laya bench site

test:
	go vet ./... && go test -race ./...

build:
	go build -o bin/gateway ./cmd/gateway
	go build -o bin/mcp ./cmd/mcp

gateway:
	go run ./cmd/gateway

laya: # local Laya sidecar on :8788 (first run downloads ~3 GB of weights)
	sidecar/.venv/bin/python sidecar/laya_server.py --preload

bench: # decisions only, no chat model is called
	go run ./cmd/bench

site: # publish the newest benchmark report to site/ (deployed by .github/workflows/pages.yml)
	@latest=$$(ls -t bench/results/bench-*.html | head -1); \
	cp "$$latest" site/index.html; cp "$${latest%.html}.json" site/bench.json; \
	echo "site/ updated from $$latest"
