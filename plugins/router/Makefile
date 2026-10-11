PLUGIN_ID := lachieh-router
VERSION   ?= dev
GOOS      := $(shell go env GOOS)
GOARCH    := $(shell go env GOARCH)
EXT       := $(if $(filter darwin,$(GOOS)),dylib,$(if $(filter windows,$(GOOS)),dll,so))
LIB       := dist/$(PLUGIN_ID).$(EXT)
ZIP       := dist/$(PLUGIN_ID)_$(VERSION:v%=%)_$(GOOS)_$(GOARCH).zip

.PHONY: build test install package clean

build:
	CGO_ENABLED=1 go build -buildmode=c-shared -trimpath \
		-ldflags "-s -w -X main.version=$(VERSION:v%=%)" -o $(LIB) .
	rm -f dist/$(PLUGIN_ID).h

test:
	go test ./...

# PLUGINS_DIR must match plugins.dir; the host resolves a relative dir against its working directory.
install: build
	@test -n "$(PLUGINS_DIR)" || { echo "set PLUGINS_DIR to your plugins.dir"; exit 1; }
	mkdir -p $(PLUGINS_DIR)/$(GOOS)/$(GOARCH)
	cp $(LIB) $(PLUGINS_DIR)/$(GOOS)/$(GOARCH)/

# Store release asset: library at the zip root, named <id>_<version>_<goos>_<goarch>.zip.
package: build
	rm -f $(ZIP)
	cd dist && zip -q $(notdir $(ZIP)) $(notdir $(LIB))

clean:
	rm -rf dist
