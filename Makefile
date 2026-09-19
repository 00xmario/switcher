BINARY := switcher
VERSION ?= $(shell date +%Y%m%d-%H%M%S)

.PHONY: build test vet install run clean

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

clean:
	rm -f $(BINARY)
