# Build recipes. The only thing the BTP stager needs is ./bin/server
# (static Linux/amd64) — everything else is optional.

BINARY := bin/server
PKG    := ./cmd/server

.PHONY: build-linux test vet lint clean

# Cross-compile a static Linux/amd64 binary the binary_buildpack can
# launch directly. CGO_ENABLED=0 keeps us off glibc and avoids needing a
# C toolchain on the build host; GOOS/GOARCH pin the target regardless
# of what the developer's laptop runs.
build-linux:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o $(BINARY) $(PKG)
	@file $(BINARY) 2>/dev/null || true

test:
	go test ./... -race

vet:
	go vet ./...

lint:
	golangci-lint run

clean:
	rm -rf bin
