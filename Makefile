BINARY := bin/burnrate
PKG := ./cmd/burnrate

.PHONY: all build test vet race ci clean

all: build

build:
	CGO_ENABLED=0 go build -trimpath -o $(BINARY) $(PKG)

test:
	go test ./...

race:
	CGO_ENABLED=1 go test -race ./...

vet:
	go vet ./...

ci: vet test race build

clean:
	rm -rf bin
