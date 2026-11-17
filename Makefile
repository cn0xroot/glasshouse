.PHONY: build test vet vz-helper clean

build:
	go build -o bin/glasshouse ./cmd/glasshouse

test:
	go vet ./...
	go test -race ./...

# macOS only: builds and ad-hoc signs the Virtualization.framework helper.
vz-helper:
	cd vz-helper && swift build -c release
	cp vz-helper/.build/release/glasshouse-vz bin/glasshouse-vz
	codesign --force --sign - --entitlements vz-helper/glasshouse-vz.entitlements bin/glasshouse-vz

clean:
	rm -rf bin vz-helper/.build
