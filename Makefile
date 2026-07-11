# Build and release helpers for pgc.
#
#   make build     build ./pgc for the current platform
#   make check     gofmt, vet and tests
#   make dist      cross-compile release archives into dist/
#   make release   create the GitHub release for the current tag
#                  and upload the dist/ archives (needs the gh CLI)

VERSION := $(shell git describe --tags --abbrev=0)
DIST    := dist
TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.PHONY: build check dist release clean

build:
	go build -trimpath -o pgc .

check:
	gofmt -l .
	go vet ./...
	go test ./...

dist: clean
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
