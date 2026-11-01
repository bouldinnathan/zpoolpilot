# Copyright (C) 2026 ZpoolPilot contributors
# SPDX-License-Identifier: GPL-3.0-or-later

GO ?= go
INSTALL ?= install
GOCACHE ?= /tmp/zpoolpilot-gocache

PREFIX ?= /usr
BINDIR ?= $(PREFIX)/bin
SYSCONFDIR ?= /etc
DATADIR ?= $(PREFIX)/share
SYSTEMD_UNITDIR ?= $(PREFIX)/lib/systemd/system
SYSUSERSDIR ?= $(PREFIX)/lib/sysusers.d
TMPFILESDIR ?= $(PREFIX)/lib/tmpfiles.d

VERSION ?= 0.1.0-alpha.1
COMMIT ?= unknown
BUILD_DATE ?= unknown
GO_BUILD_FLAGS ?= -buildvcs=false -trimpath
GO_TEST_FLAGS ?= -buildvcs=false
LDFLAGS ?= -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(BUILD_DATE)

SYSTEMD_UNITS := \
	packaging/systemd/zpoolpilot-collect-fast.service \
	packaging/systemd/zpoolpilot-collect-fast.timer \
	packaging/systemd/zpoolpilot-collect-hardware.service \
	packaging/systemd/zpoolpilot-collect-hardware.timer \
	packaging/systemd/zpoolpilot-exporter.service

.PHONY: all build test test-offline test-race test-integration test-integration-build vet fmt fmt-check \
	check check-packaging install install-check clean

all: check build

build:
	@mkdir -p bin
	GOCACHE=$(GOCACHE) $(GO) build $(GO_BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o bin/zpoolpilot ./cmd/zpoolpilot

test:
	GOCACHE=$(GOCACHE) $(GO) test $(GO_TEST_FLAGS) ./...

# This target proves that the unit suite does not fetch modules or toolchains.
test-offline:
	GOENV=off GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off GOCACHE=$(GOCACHE) \
		$(GO) test $(GO_TEST_FLAGS) -mod=readonly ./...

test-race:
	GOENV=off GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off GOCACHE=$(GOCACHE) \
		$(GO) test $(GO_TEST_FLAGS) -mod=readonly -race ./...

# Never runs as part of all/check/CI. The script requires root, an explicit
# acknowledgement token, and a detected virtual machine before creating a
# uniquely named pool backed only by temporary loop devices.
test-integration: build
	ZPOOLPILOT_INTEGRATION_BINARY="$(CURDIR)/bin/zpoolpilot" ./scripts/run-zfs-integration.sh

# Compile the guarded suite without running its tests or invoking storage tools.
test-integration-build:
	@check_dir="$$(mktemp -d /tmp/zpoolpilot-integration-build.XXXXXXXX)" || exit 1; \
		trap 'rm -f "$$check_dir/integration.test"; rmdir "$$check_dir"' EXIT; \
		GOENV=off GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off GOCACHE=$(GOCACHE) \
		$(GO) test $(GO_TEST_FLAGS) -mod=readonly -tags=zfsintegration -c \
		-o "$$check_dir/integration.test" ./integration

vet:
	GOENV=off GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off GOCACHE=$(GOCACHE) \
		$(GO) vet -buildvcs=false -mod=readonly ./...

fmt:
	gofmt -w cmd internal integration

fmt-check:
	@unformatted="$$(gofmt -l cmd internal integration)" || exit 1; \
	if [ -n "$$unformatted" ]; then \
		printf '%s\n' "Go files need gofmt:" "$$unformatted" >&2; \
		exit 1; \
	fi

check-packaging:
	./scripts/check-packaging.sh

check: fmt-check vet test-offline test-integration-build check-packaging install-check

install: build
	@test "$(BINDIR)" = /usr/bin || { \
		printf '%s\n' 'install: packaged systemd units require BINDIR=/usr/bin' >&2; exit 2; \
	}
	@test "$(SYSCONFDIR)" = /etc || { \
		printf '%s\n' 'install: packaged systemd units require SYSCONFDIR=/etc' >&2; exit 2; \
	}
	$(INSTALL) -d "$(DESTDIR)$(BINDIR)"
	$(INSTALL) -m0755 bin/zpoolpilot "$(DESTDIR)$(BINDIR)/zpoolpilot"
	$(INSTALL) -d "$(DESTDIR)$(SYSCONFDIR)/zpoolpilot"
	@if [ ! -e "$(DESTDIR)$(SYSCONFDIR)/zpoolpilot/config.toml" ]; then \
		$(INSTALL) -m0644 configs/zpoolpilot.toml "$(DESTDIR)$(SYSCONFDIR)/zpoolpilot/config.toml"; \
	else \
		printf '%s\n' "preserving existing $(DESTDIR)$(SYSCONFDIR)/zpoolpilot/config.toml"; \
	fi
	@if [ ! -e "$(DESTDIR)$(SYSCONFDIR)/zpoolpilot/bays.json" ]; then \
		$(INSTALL) -m0644 configs/bays.json "$(DESTDIR)$(SYSCONFDIR)/zpoolpilot/bays.json"; \
	else \
		printf '%s\n' "preserving existing $(DESTDIR)$(SYSCONFDIR)/zpoolpilot/bays.json"; \
	fi
	$(INSTALL) -d "$(DESTDIR)$(SYSTEMD_UNITDIR)"
	$(INSTALL) -m0644 $(SYSTEMD_UNITS) "$(DESTDIR)$(SYSTEMD_UNITDIR)/"
	$(INSTALL) -d "$(DESTDIR)$(SYSUSERSDIR)" "$(DESTDIR)$(TMPFILESDIR)"
	$(INSTALL) -m0644 packaging/sysusers.d/zpoolpilot.conf "$(DESTDIR)$(SYSUSERSDIR)/zpoolpilot.conf"
	$(INSTALL) -m0644 packaging/tmpfiles.d/zpoolpilot.conf "$(DESTDIR)$(TMPFILESDIR)/zpoolpilot.conf"
	$(INSTALL) -d "$(DESTDIR)$(DATADIR)/zpoolpilot/schemas"
	$(INSTALL) -m0644 schemas/*.json "$(DESTDIR)$(DATADIR)/zpoolpilot/schemas/"
	$(INSTALL) -d "$(DESTDIR)$(DATADIR)/doc/zpoolpilot"
	$(INSTALL) -m0644 LICENSE README.md docs/architecture.md docs/threat-model.md docs/testing.md docs/development-plan.md \
		"$(DESTDIR)$(DATADIR)/doc/zpoolpilot/"
	$(INSTALL) -m0644 packaging/README.md "$(DESTDIR)$(DATADIR)/doc/zpoolpilot/packaging.md"
	$(INSTALL) -d "$(DESTDIR)$(DATADIR)/man/man8"
	$(INSTALL) -m0644 packaging/man/zpoolpilot.8 "$(DESTDIR)$(DATADIR)/man/man8/zpoolpilot.8"

install-check:
	./scripts/check-install.sh

clean:
	@rm -f bin/zpoolpilot
