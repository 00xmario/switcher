BINARY := switcher
VERSION ?= $(shell cat VERSION 2>/dev/null || echo dev)

.PHONY: build test vet verify install run dev clean

build:
	go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BINARY) .

test:
	gofmt -l . && go vet ./... && go test ./...

vet:
	go vet ./...

# Run the same checks before a tag and in both CI workflows.
verify:
	go test ./...
	go vet ./...
	go test -race ./...
	node --check web/app.js
	node --check web/login.js
	node web/app_state_test.mjs
	node web/login_test.mjs
	@set -eu; tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
		swiftc -parse-as-library -D SWITCHER_LAYOUT_TEST build/macos/menubar.swift \
			build/macos/menubar_layout_test.swift -o "$$tmp/switcher-layout-test"; \
		"$$tmp/switcher-layout-test"; \
		GOOS=darwin GOARCH=arm64 go build -o "$$tmp/switcher-darwin-arm64" .; \
		GOOS=darwin GOARCH=amd64 go build -o "$$tmp/switcher-darwin-amd64" .; \
		GOOS=linux GOARCH=amd64 go build -o "$$tmp/switcher-linux-amd64" .; \
		GOOS=linux GOARCH=arm64 go build -o "$$tmp/switcher-linux-arm64" .
	git diff --check HEAD^ HEAD
	git diff --cached --check
	git diff --check

install: build
	install -m 0755 $(BINARY) "$(shell go env GOPATH)/bin"

run: build
	./$(BINARY)

# Dev mode serves web/ from disk: UI changes need only a browser refresh.
dev:
	SWITCHER_DEV=1 go run .

clean:
	rm -f $(BINARY)
