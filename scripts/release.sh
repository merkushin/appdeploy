#!/usr/bin/env bash
# Builds, checksums and signs a release into dist/ (or $DIST).
#
#   RELEASE_SIGNING_KEY=<base64 key> scripts/release.sh v1.2.3
#
# Also writes release-notes.md (or $NOTES_FILE) with the key fingerprint and how to verify the release.
#
# The public key is derived from the signing key. RELEASE_PUBLIC_KEY may be given
# as a check: the script stops if it is not the public half of the signing key.
#
# Creates dist/appdeploy-<os>-<arch> for linux and darwin on amd64 and arm64,
# dist/SHA256SUMS and dist/SHA256SUMS.sig. The binaries are built with the public
# key and the repository (default merkushin/appdeploy, or $REPO) so that
# `appdeploy self-update` only accepts releases signed with the matching key.
set -euo pipefail

VERSION="${1:?usage: release.sh vMAJOR.MINOR.PATCH}"
[[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "release: $VERSION is not a version like v1.2.3" >&2; exit 1; }
: "${RELEASE_SIGNING_KEY:?set RELEASE_SIGNING_KEY (see: go run ./cmd/releasetool keygen)}"
REPO="${REPO:-merkushin/appdeploy}"
DIST="${DIST:-dist}"
cd "$(dirname "$0")/.."

TOOL="$(mktemp -d)/releasetool"
go build -o "$TOOL" ./cmd/releasetool

# The binaries embed the public half of the signing key, so that they only accept their own releases.
DERIVED_PUBLIC_KEY="$(RELEASE_SIGNING_KEY="$RELEASE_SIGNING_KEY" "$TOOL" pubkey -key-env RELEASE_SIGNING_KEY)" \
  || { echo "release: RELEASE_SIGNING_KEY is not a valid signing key (expected the contents of the file made by: releasetool keygen)" >&2; exit 1; }
if [ -n "${RELEASE_PUBLIC_KEY:-}" ] && [ "$RELEASE_PUBLIC_KEY" != "$DERIVED_PUBLIC_KEY" ]; then
  echo "release: RELEASE_PUBLIC_KEY is not the public half of RELEASE_SIGNING_KEY" >&2
  exit 1
fi
RELEASE_PUBLIC_KEY="$DERIVED_PUBLIC_KEY"

rm -rf "$DIST"; mkdir -p "$DIST"
LDFLAGS="-s -w -X main.version=${VERSION#v} -X main.updateRepo=$REPO -X main.updatePublicKey=$RELEASE_PUBLIC_KEY"
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os="${target%/*}"; arch="${target#*/}"
  echo "building $os/$arch"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "$LDFLAGS" -o "$DIST/appdeploy-$os-$arch" ./cmd/appdeploy
done

(
  cd "$DIST"
  if command -v sha256sum >/dev/null; then sha256sum appdeploy-* > SHA256SUMS; else shasum -a 256 appdeploy-* > SHA256SUMS; fi
)
RELEASE_SIGNING_KEY="$RELEASE_SIGNING_KEY" "$TOOL" sign -key-env RELEASE_SIGNING_KEY "$DIST/SHA256SUMS" > "$DIST/SHA256SUMS.sig"
"$TOOL" verify -pub "$RELEASE_PUBLIC_KEY" "$DIST/SHA256SUMS" "$DIST/SHA256SUMS.sig"
# Notes that tell readers what to trust and how to check it; the workflow publishes them with the release.
FINGERPRINT="$(RELEASE_SIGNING_KEY="$RELEASE_SIGNING_KEY" "$TOOL" fingerprint -key-env RELEASE_SIGNING_KEY)"
cat > "${NOTES_FILE:-release-notes.md}" <<NOTES
## Verify before you trust it

Release key fingerprint (sha256): \`$FINGERPRINT\`
Public key (ed25519, base64): \`$RELEASE_PUBLIC_KEY\`

- Every binary has a build attestation: it proves the file was built by the release workflow of
  \`$REPO\` and not elsewhere. With the GitHub CLI: \`gh attestation verify appdeploy-linux-amd64 --repo $REPO\`
- Checksums: \`sha256sum --check --ignore-missing SHA256SUMS\`
- After installing, \`appdeploy version --verbose\` shows the same key and fingerprint. Later updates
  (\`appdeploy self-update\`) are accepted only when signed with that key.
NOTES
echo "release $VERSION is in $DIST/ (notes: ${NOTES_FILE:-release-notes.md})"
