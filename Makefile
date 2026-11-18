.PHONY: all build test vet vz-helper mac clean

UNAME_S := $(shell uname -s)
UNAME_M := $(shell uname -m)

# On a Mac, `make` builds the CLI and the Virtualization.framework helper together, so the pair always matches.
ifeq ($(UNAME_S),Darwin)
all: mac
else
all: build
endif

build:
	go build -o bin/glasshouse ./cmd/glasshouse

test:
	go vet ./...
	go test -race ./...

# macOS only: builds and ad-hoc signs the Virtualization.framework helper for this Mac's CPU.
# On Apple silicon both binaries must be native arm64: a Rosetta (x86_64) shell or Go toolchain would build an
# amd64 glasshouse that can never boot an arm64 guest.
vz-helper:
	@[ "$(UNAME_S)" = Darwin ] || { echo "vz-helper: macOS only" >&2; exit 1; }
	@if [ "$$(sysctl -n sysctl.proc_translated 2>/dev/null)" = 1 ]; then \
	  echo "vz-helper: this shell runs under Rosetta; open a native (arm64) terminal, or run: arch -arm64 make vz-helper" >&2; exit 1; fi
	cd vz-helper && swift build -c release --arch $(UNAME_M)
	mkdir -p bin
	cp vz-helper/.build/release/glasshouse-vz bin/glasshouse-vz
	codesign --force --sign - --entitlements vz-helper/glasshouse-vz.entitlements bin/glasshouse-vz
	@bin/glasshouse-vz check

mac: vz-helper
	GOOS=darwin GOARCH=$(if $(filter arm64,$(UNAME_M)),arm64,amd64) go build -o bin/glasshouse ./cmd/glasshouse

clean:
	rm -rf bin vz-helper/.build
