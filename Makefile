# aurlock - an AUR helper that installs only approved commits.
# See LICENSE file for copyright and license details.

include config.mk

export CGO_ENABLED = 0

SRC = $(shell find cmd internal -type f 2>/dev/null) go.mod go.sum config.mk

all: build

build: bin/aurlock

# A real file target, so `sudo make install` after `make` only copies and
# doesn't rebuild as root.
bin/aurlock: ${SRC}
	@mkdir -p bin
	${GO} build ${GOFLAGS} -ldflags "${LDFLAGS}" -o $@ ./cmd/aurlock

run:
	${GO} run ./cmd/aurlock ${ARGS}

test:
	${GO} test -v ./...

fmt:
	${GO} fmt ./...

lint:
	${GO} vet ./...
	${GO} tool staticcheck ./...

clean:
	rm -rf bin

install: bin/aurlock
	mkdir -p ${DESTDIR}${PREFIX}/bin
	cp -f bin/aurlock ${DESTDIR}${PREFIX}/bin
	chmod 755 ${DESTDIR}${PREFIX}/bin/aurlock

uninstall:
	rm -f ${DESTDIR}${PREFIX}/bin/aurlock

.PHONY: all build run test fmt lint clean install uninstall
