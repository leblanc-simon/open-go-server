# Cibles de développement d'ogs.
BINARY  := ogs
PKG     := ./cmd/ogs
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X leblanc.io/open-go-server/internal/version.Version=$(VERSION) \
	-X leblanc.io/open-go-server/internal/version.Commit=$(COMMIT) \
	-X leblanc.io/open-go-server/internal/version.Date=$(DATE)

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
	go test -timeout 20m ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w cmd internal

cover:
	go test -timeout 20m -coverprofile=coverage.out ./...
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
# Le « -n » des compressions écarte l'horodatage : sans lui, deux constructions
# du même arbre donneraient deux paquets différents.
package: doc
	@mkdir -p dist
	gzip -9nf dist/doc/man/*.8
	./packaging/changelog.sh $(DEBVER) | gzip -9n > dist/doc/changelog.gz
	go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY) $(PKG)
	OGS_VERSION=$(DEBVER) OGS_ARCH=$(ARCH) \
		go run github.com/goreleaser/nfpm/v2/cmd/nfpm@latest package \
		--config nfpm.yaml --packager deb --target dist/
	@ls -1 dist/*.deb

# --- Essais d'intégration ----------------------------------------------------
# Ils exigent un système complet — serveur web, PHP, comptes — et s'exécutent
# donc dans un conteneur. Ils ne font pas partie de « make test ».
# Tous les scénarios, l'un après l'autre.
SCENARIOS := reload-group site-complet suppression web-durci nginx hooks mysql postgres rabbitmq pebble systemd runtimes harden paquet supervision dkim journaux pare-feu fpm-sans-pool restauration php-anciennes

integration-all:
	@for s in $(SCENARIOS); do \
		echo "########## $$s"; \
		./test/integration/run.sh $$s || exit 1; \
	done

integration:
	./test/integration/run.sh reload-group

integration-php:
	./test/integration/run.sh site-complet

integration-suppression:
	./test/integration/run.sh suppression

integration-web-durci:
	./test/integration/run.sh web-durci

integration-package:
	./test/integration/run.sh paquet

integration-php-anciennes:
	./test/integration/run.sh php-anciennes

integration-nginx:
	./test/integration/run.sh nginx

integration-hooks:
	./test/integration/run.sh hooks

integration-mysql:
	./test/integration/run.sh mysql

integration-systemd:
	./test/integration/run.sh systemd

integration-runtimes:
	./test/integration/run.sh runtimes

integration-harden:
	./test/integration/run.sh harden

integration-supervision:
	./test/integration/run.sh supervision

integration-dkim:
	./test/integration/run.sh dkim

integration-journaux:
	./test/integration/run.sh journaux

integration-pare-feu:
	./test/integration/run.sh pare-feu

integration-fpm-sans-pool:
	./test/integration/run.sh fpm-sans-pool

integration-restauration:
	./test/integration/run.sh restauration

# Hors de la série ordinaire : rbenv et pyenv compilent depuis les sources, ce
# qui demande un quart d'heure, du réseau et des dépendances de compilation.
integration-runtimes-compiles:
	./test/integration/run.sh runtimes-compiles

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
