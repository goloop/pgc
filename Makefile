# Build and release helpers for pgc.
#
#   make build     build ./pgc for the current platform
#   make check     gofmt, vet and tests
#   make dist      cross-compile release archives into dist/
#   make release   create the GitHub release for the current tag
#                  and upload the dist/ archives (needs the gh CLI)
#
# Releases refuse to build while the version constant in main.go disagrees with
# the tag. It has fallen behind twice, and a tool that misreports its own
# version writes that wrong version into every pgc.lock.json it produces.

VERSION := $(shell git describe --tags --abbrev=0)
DIST    := dist
TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.PHONY: build check version dist release clean

build:
	go build -trimpath -o pgc .

check: version
	gofmt -l .
	go vet ./...
	go test ./...

# version fails when main.go and the newest tag name different versions, so a
# release cannot be cut with a stale constant. Bump the constant in the same
# commit the tag will point at - documentation-only releases included.
version:
	@expected=$(VERSION); expected=$${expected#v}; \
	found=$$(sed -n 's/^const version = "\(.*\)"$$/\1/p' main.go); \
	if [ "$$found" != "$$expected" ]; then \
		echo "main.go says version $$found, but the tag says $$expected"; \
		echo "bump the constant in the commit the tag points at"; \
		exit 1; \
	fi

dist: version clean
	@mkdir -p $(DIST)
	@cp LICENSE $(DIST)/
	@set -e; for target in $(TARGETS); do \
		os=$${target%/*}; arch=$${target#*/}; \
		bin=pgc; if [ "$$os" = "windows" ]; then bin=pgc.exe; fi; \
		echo "  $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath -ldflags "-s -w" -o "$(DIST)/$$bin" .; \
		tar -C $(DIST) -czf "$(DIST)/pgc_$(VERSION)_$${os}_$${arch}.tar.gz" \
			"$$bin" LICENSE; \
		rm "$(DIST)/$$bin"; \
	done
	@rm $(DIST)/LICENSE
	@cd $(DIST) && sha256sum *.tar.gz > checksums.txt
	@ls -1 $(DIST)

release: dist
	gh release create $(VERSION) $(DIST)/*.tar.gz $(DIST)/checksums.txt \
		--title "$(VERSION)" \
		--notes "See CHANGELOG.md for what changed in $(VERSION)."

clean:
	rm -rf $(DIST) pgc
