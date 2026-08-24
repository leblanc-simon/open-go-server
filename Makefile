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

ARCH    := $(shell go env GOARCH)
# nfpm refuse un numéro de version non sémantique : « v1.2.3-4-gabc » devient
# « 1.2.3-4 », et une construction hors étiquette retombe sur 0.0.0.
DEBVER  := $(shell git describe --tags --abbrev=0 2>/dev/null | sed 's/^v//' || echo 0.0.0)

.PHONY: all build test vet fmt cover clean install dev-init dev-demo \
	doc completion man package integration integration-php integration-package integration-hooks \
	integration-all integration-mysql integration-rabbitmq integration-acme tidy check

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
	rm -rf $(BINARY) coverage.out dist

# --- Documentation -----------------------------------------------------------
# Pages de manuel, scripts de complétion et référence Markdown sont engendrés
# depuis l'arbre des commandes : la documentation ne peut pas dériver du code.
doc:
	rm -rf dist/doc
	go run ./tools/docgen dist/doc

man: doc
completion: doc

# --- Paquet Debian -----------------------------------------------------------
# nfpm n'est pas une dépendance de compilation : on l'exécute depuis son module.
package: doc
	@mkdir -p dist
	gzip -9nf dist/doc/man/*.8
	go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY) $(PKG)
	OGS_VERSION=$(DEBVER) OGS_ARCH=$(ARCH) \
		go run github.com/goreleaser/nfpm/v2/cmd/nfpm@latest package \
		--config nfpm.yaml --packager deb --target dist/
	@ls -1 dist/*.deb

# --- Essais d'intégration ----------------------------------------------------
# Ils exigent un système complet — serveur web, PHP, comptes — et s'exécutent
# donc dans un conteneur. Ils ne font pas partie de « make test ».
# Tous les scénarios, l'un après l'autre.
SCENARIOS := reload-group site-complet hooks mysql postgres rabbitmq pebble paquet

integration-all:
	@for s in $(SCENARIOS); do \
		echo "########## $$s"; \
		./test/integration/run.sh $$s || exit 1; \
	done

integration:
	./test/integration/run.sh reload-group

integration-php:
	./test/integration/run.sh site-complet

integration-package:
	./test/integration/run.sh paquet

integration-hooks:
	./test/integration/run.sh hooks

integration-mysql:
	./test/integration/run.sh mysql

integration-postgres:
	./test/integration/run.sh postgres

integration-rabbitmq:
	./test/integration/run.sh rabbitmq

integration-acme:
	./test/integration/run.sh pebble

tidy:
	go mod tidy

check: fmt vet test

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
