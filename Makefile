BINARY  := git-track
MODULE  := github.com/robertdewilde-dev/git-track
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w
OSES    := linux darwin windows
ARCHES  := amd64 arm64

.PHONY: build install test vet cross clean

# The one binary answers to both `git track` (found by git as git-track) and
# plain `track`; the second name is a symlink to the first.
GOBIN   := $(or $(shell go env GOBIN),$(shell go env GOPATH)/bin)

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) .
	ln -sf $(BINARY) bin/track

install:
	go install -trimpath -ldflags '$(LDFLAGS)' .
	ln -sf $(GOBIN)/$(BINARY) $(GOBIN)/track

test:
	go test ./...

vet:
	go vet ./...

cross:
	@for os in $(OSES); do \
		for arch in $(ARCHES); do \
			ext=""; [ $$os = windows ] && ext=".exe"; \
			out=dist/$(BINARY)_$(VERSION)_$$os\_$$arch/$(BINARY)$$ext; \
			echo "building $$out"; \
			GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
				go build -trimpath -ldflags '$(LDFLAGS)' -o $$out . || exit 1; \
		done; \
	done

clean:
	rm -rf bin dist
