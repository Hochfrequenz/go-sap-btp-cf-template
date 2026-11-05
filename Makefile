# Build recipes. The only thing the BTP stager needs is ./bin/server
# (static Linux/amd64) — everything else is optional.

BINARY   := bin/server
PKG      := ./cmd/server
PKG_PATH := $(shell head -1 go.mod | awk '{print $$2}')/cmd/server

# One rule for version/commit/branch/date, shared with the CI workflows.
BUILD_INFO := $(shell bash .github/scripts/build-info.sh)
build_info = $(patsubst $(1)=%,%,$(filter $(1)=%,$(BUILD_INFO)))
VERSION   := $(call build_info,version)
COMMIT    := $(call build_info,commit)
BRANCH    := $(call build_info,branch)
BUILDDATE := $(call build_info,build_date)

LDFLAGS := -X main.version=$(VERSION) \
           -X main.commit=$(COMMIT) \
           -X main.branch=$(BRANCH) \
           -X main.buildDate=$(BUILDDATE)

.PHONY: build-linux test vet lint clean

# Cross-compile a static Linux/amd64 binary the binary_buildpack can
# launch directly. CGO_ENABLED=0 keeps us off glibc and avoids needing a
# C toolchain on the build host; GOOS/GOARCH pin the target regardless
# of what the developer's laptop runs.
build-linux:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)
	@file $(BINARY) 2>/dev/null || true

test:
	go test ./... -race

vet:
	go vet ./...

lint:
	golangci-lint run

clean:
	rm -rf bin
