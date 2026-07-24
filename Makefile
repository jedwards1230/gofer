# gofer local build/install targets.
#
# A bare `go install ./cmd/gofer` mis-stamps the version: Go's buildvcs reads
# the PRIMARY git worktree's HEAD, not the linked worktree you actually built,
# so `resolveVersion` falls back to a pseudo-version pinned to the wrong commit.
# That silently defeats the stale-daemon version-skew banner (client and daemon
# both stamp the same wrong SHA). These targets stamp the truthful describe of
# the checkout being built via the same `-X main.version=` ldflags seam the
# release workflow uses (.github/workflows/release.yml).
#
# `--dirty` marks a modified tree so an uncommitted build is visibly non-release.

VERSION ?= $(shell git describe --tags --always --dirty)
LDFLAGS := -X main.version=$(VERSION)
BIN     := bin/gofer

.PHONY: build install

# build compiles a version-stamped binary into bin/gofer (gitignored).
build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/gofer

# install builds and installs a version-stamped gofer into GOBIN/GOPATH/bin.
# Use this instead of a bare `go install` so the binary reports its true HEAD.
install:
	go install -ldflags "$(LDFLAGS)" ./cmd/gofer
