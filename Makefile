# Cibles de développement d'ogs.
BINARY  := ogs
PKG     := ./cmd/ogs
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X github.com/leviathan/ogs/internal/version.Version=$(VERSION) \
	-X github.com/leviathan/ogs/internal/version.Commit=$(COMMIT) \
	-X github.com/leviathan/ogs/internal/version.Date=$(DATE)

# Binaire statique : aucune dépendance C, donc un seul fichier à déployer.
export CGO_ENABLED = 0

.PHONY: all build test vet fmt cover clean install dev-init dev-demo

all: fmt vet test build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w cmd internal

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

clean:
	rm -f $(BINARY) coverage.out

install: build
	install -m 0755 $(BINARY) /usr/bin/$(BINARY)

# Bac à sable : tout est créé sous ./dev, sans privilèges et sans toucher au système.
DEV := $(CURDIR)/dev

dev-init: build
	./$(BINARY) --prefix $(DEV) init

dev-demo: dev-init
	./$(BINARY) --prefix $(DEV) project create shop.fr --kind php-symfony --app-version 6.4
	./$(BINARY) --prefix $(DEV) project create boutique.fr --kind php-prestashop --php 7.4
	./$(BINARY) --prefix $(DEV) plan
	./$(BINARY) --prefix $(DEV) apply --yes
	./$(BINARY) --prefix $(DEV) project list
