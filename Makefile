.PHONY: all build test lint fmt tidy dist clean check-version

BIN  := bin
DIST := dist
GO   ?= go

# VERSION is embedded in the binaries (`backpack version`) and names the
# tarball. Release builds pass the tag, e.g. `make dist VERSION=v0.1.0`.
# check-version limits it to [A-Za-z0-9._+-] before any recipe uses it; it
# reads VERSION from the environment so a bad value never reaches a shell
# command line.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
export VERSION
LDFLAGS := -s -w -X github.com/ilias-t/bckp/internal/version.Version=$(VERSION)
TARBALL := bckp_$(patsubst v%,%,$(VERSION))_darwin_universal.tar.gz

# The oldest macOS the release binaries run on (Go's own minimum). Without
# it, cgo targets the build machine's macOS version. Passed as flags, not
# MACOSX_DEPLOYMENT_TARGET, because the flags are part of Go's build cache
# key.
MACOS_MIN := -mmacosx-version-min=12.0
DIST_ENV  := CGO_ENABLED=1 CGO_CFLAGS="-O2 $(MACOS_MIN)" CGO_LDFLAGS="$(MACOS_MIN)"

all: build

check-version:
	@case "$$VERSION" in \
	'' | [!A-Za-z0-9]* | *[!A-Za-z0-9._+-]*) \
		echo "Makefile: VERSION may only hold letters, digits and ._+- (got: $$VERSION)" >&2; exit 1 ;; \
	esac

## build: build backpack and backpackd into ./bin, plus the bckp symlink
build: check-version
	@mkdir -p $(BIN)
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/backpack ./cmd/backpack
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/backpackd ./cmd/backpackd
	ln -sfn backpack $(BIN)/bckp

## test: run all tests with the race detector
test:
	$(GO) test -race ./...

## lint: gofmt check and go vet, plus golangci-lint when installed
lint:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	$(GO) vet ./...
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; else echo "golangci-lint not installed; skipped"; fi

## fmt: format all Go files
fmt:
	gofmt -w -s .

## tidy: tidy go.mod/go.sum
tidy:
	$(GO) mod tidy

## dist: universal (arm64 + amd64) release tarball and checksums in ./dist
dist: check-version
	rm -rf $(DIST)
	@mkdir -p $(DIST)/arm64 $(DIST)/amd64 $(DIST)/universal
	for b in backpack backpackd; do \
		$(DIST_ENV) GOARCH=arm64 CC="clang -arch arm64" $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/arm64/$$b ./cmd/$$b && \
		$(DIST_ENV) GOARCH=amd64 CC="clang -arch x86_64" $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/amd64/$$b ./cmd/$$b && \
		lipo -create -output $(DIST)/universal/$$b $(DIST)/arm64/$$b $(DIST)/amd64/$$b || exit 1; \
	done
	cp LICENSE README.md $(DIST)/universal/
	tar -C $(DIST)/universal -czf '$(DIST)/$(TARBALL)' backpack backpackd LICENSE README.md
	cd $(DIST) && shasum -a 256 '$(TARBALL)' > checksums.txt
	@lipo -info $(DIST)/universal/backpack $(DIST)/universal/backpackd
	@echo '$(DIST)/$(TARBALL)'

clean:
	rm -rf $(BIN) $(DIST)
