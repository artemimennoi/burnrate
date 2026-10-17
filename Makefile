BINARY := bin/burnrate
PKG := ./cmd/burnrate

.PHONY: all build test vet ci clean

all: build

build:
	CGO_ENABLED=0 go build -trimpath -o $(BINARY) $(PKG)

test:
	go test ./...

vet:
	go vet ./...

ci: vet test build

clean:
	rm -rf bin
