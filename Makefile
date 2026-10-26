OWNER ?= $(shell gh repo view --json owner -q .owner.login 2>/dev/null)

REPO ?= helayoty/forksync
IMAGE_OWNER ?= $(firstword $(subst /, ,$(REPO)))

.PHONY: build test image verify-release
build:
	go build -o bin/forksync ./cmd/forksync

test:
	go vet ./... && go test ./...


# Local snapshot: archives, deb/rpm and per-arch images (loaded, not pushed).
image:
	IMAGE_OWNER=$(IMAGE_OWNER) goreleaser release --snapshot --clean --skip=publish,sign,sbom

# Verify a published release's checksums.txt signature: make verify-release VERSION=v1.2.3
verify-release:
	@test -n "$(VERSION)" || (echo "VERSION=vX.Y.Z required"; exit 1)
	@d=$$(mktemp -d) && trap 'rm -rf "$$d"' EXIT && \
	gh release download "$(VERSION)" --repo "$(REPO)" --dir "$$d" \
	  --pattern checksums.txt --pattern checksums.txt.sigstore.json && \
	cosign verify-blob --bundle "$$d/checksums.txt.sigstore.json" \
	  --certificate-identity-regexp '^https://github.com/$(REPO)/\.github/workflows/forksync-release\.yml@refs/(heads/main|tags/v[0-9].*)$$' \
	  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
	  "$$d/checksums.txt"
