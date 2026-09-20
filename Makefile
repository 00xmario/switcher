BINARY := switcher
VERSION ?= $(shell cat VERSION 2>/dev/null || echo dev)

.PHONY: build test vet install run dev clean

build:
	go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BINARY) .

test:
	gofmt -l . && go vet ./... && go test ./...

vet:
	go vet ./...

install: build
	install -m 0755 $(BINARY) "$(shell go env GOPATH)/bin"

run: build
	./$(BINARY)

# Dev mode serves web/ from disk: UI changes need only a browser refresh.
dev:
	SWITCHER_DEV=1 go run .

clean:
	rm -f $(BINARY)
