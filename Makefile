PREFIX ?= /usr/local
BINDIR ?= $(PREFIX)/bin
DATADIR ?= $(PREFIX)/share/home-metrics
INSTALL ?= install

BINS := \
	hm-ble-collector \
	hm-db-check \
	hm-db-migrate \
	hm-alert-worker \
	hm-api-server \
	hm-db-maint \
	hm-cisco-spaces-collector \
	hm-cisco-spaces-export-raw \
	hm-nature-remo-collector \
	hm-echonet-collector \
	hm-apcupsd-collector \
	hm-energy-influx-import

.PHONY: all build install uninstall test clean

all: build

build:
	./tools/build.sh

install: build
	$(INSTALL) -d $(DESTDIR)$(BINDIR)
	for bin in $(BINS); do \
		$(INSTALL) -m 0755 build/$$bin $(DESTDIR)$(BINDIR)/$$bin; \
	done
	$(INSTALL) -d $(DESTDIR)$(DATADIR)/migrations
	for migration in db/migrations/*.sql; do \
		$(INSTALL) -m 0644 $$migration $(DESTDIR)$(DATADIR)/migrations/$$(basename $$migration); \
	done

uninstall:
	for bin in $(BINS); do \
		rm -f $(DESTDIR)$(BINDIR)/$$bin; \
	done
	rm -rf $(DESTDIR)$(DATADIR)/migrations

test:
	mkdir -p .cache/go-build .cache/go-mod
	GOCACHE=$(CURDIR)/.cache/go-build GOMODCACHE=$(CURDIR)/.cache/go-mod go test ./...

clean:
	rm -rf build
