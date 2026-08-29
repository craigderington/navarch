#!/usr/bin/env bash
# Build the versioned `navarch` binaries a release is made of.
#
# Only the CLI is built here. The control plane and the agent ship as container
# images (deploy/Dockerfile), because that is how they are actually run — a
# tarball of a server binary with no Postgres, no migrations and no router is
# not a deployment, and pretending otherwise would give someone a way to install
# half a platform.
#
# Reproducibility is the point of the flags:
#   -trimpath     strips the build machine's paths out of the binary, so the
#                 same source at the same version produces the same bytes on
#                 someone else's machine.
#   CGO_ENABLED=0 static, so it runs on distroless, alpine and a bare VM alike.
#   -s -w         drops the symbol and DWARF tables; nothing debugs a release
#                 binary in place, and it is a third of the size.
set -euo pipefail
cd "$(dirname "$0")/.."

MODULE=github.com/craigderington/navarch
OUT=${OUT:-dist}

step() { printf '\n\033[36m▸ %s\033[0m\n' "$1"; }
note() { printf '  \033[90m%s\033[0m\n' "$1"; }
fail() {
    printf '\033[31mFAIL: %s\033[0m\n' "$1" >&2
    exit 1
}

# A version is derived from the tag unless one is given. `git describe` appends
# -dirty for uncommitted changes, and that suffix is load-bearing: a binary
# built from a working tree must not be able to claim it is a tagged release,
# because nobody can ever reproduce it.
VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
# SOURCE_DATE_EPOCH is the reproducible-builds convention; falling back to the
# commit's own date keeps the stamp a property of the source rather than of the
# moment someone happened to run this.
DATE=$(date -u -d "@$(git log -1 --format=%ct 2>/dev/null || date +%s)" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null ||
    date -u +%Y-%m-%dT%H:%M:%SZ)

case "$VERSION" in
*-dirty)
    if [ "${ALLOW_DIRTY:-}" != "1" ]; then
        fail "working tree is dirty ($VERSION) — commit first, or set ALLOW_DIRTY=1 for a throwaway build"
    fi
    note "building a dirty tree because ALLOW_DIRTY=1"
    ;;
esac

LDFLAGS="-s -w
  -X $MODULE/internal/version.Version=$VERSION
  -X $MODULE/internal/version.Commit=$COMMIT
  -X $MODULE/internal/version.Date=$DATE"

# The platforms an operator plausibly drives this from. Linux arm64 is not
# padding: it is what a Graviton or Ampere box runs, which is exactly the sort
# of cheap VM this platform is meant to sit on.
PLATFORMS=${PLATFORMS:-"linux/amd64 linux/arm64 darwin/amd64 darwin/arm64"}

step "navarch $VERSION ($COMMIT, $DATE)"
rm -rf "$OUT"
mkdir -p "$OUT"

for p in $PLATFORMS; do
    os=${p%/*}
    arch=${p#*/}
    name="navarch_${VERSION}_${os}_${arch}"
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
        go build -trimpath -ldflags "$LDFLAGS" -o "$OUT/$name/navarch" ./cmd/navarch
    tar -C "$OUT/$name" -czf "$OUT/$name.tar.gz" navarch
    rm -rf "${OUT:?}/$name"
    note "$name.tar.gz"
done

step "Checksums"
# Written from inside the directory so the file lists bare names, which is what
# `sha256sum -c SHA256SUMS` expects a downloader to be able to run.
(cd "$OUT" && sha256sum ./*.tar.gz >SHA256SUMS)
note "$(wc -l <"$OUT/SHA256SUMS") artifacts"

step "Smoke test: the host-native binary reports the version it was stamped with"
host="$OUT/navarch_${VERSION}_$(go env GOOS)_$(go env GOARCH).tar.gz"
[ -f "$host" ] || fail "no artifact for this host ($(go env GOOS)/$(go env GOARCH))"
tar -xzf "$host" -C "$OUT"
got=$("$OUT/navarch" version | head -1)
[ "$got" = "navarch $VERSION" ] || fail "binary reports '$got', expected 'navarch $VERSION'"
rm -f "$OUT/navarch"
note "$got"

printf '\n\033[32m%s\033[0m\n' "Release artifacts in $OUT/"
printf '  \033[90mServer side ships as images: docker build -f deploy/Dockerfile --target controlplane -t navarch/controlplane:%s .\033[0m\n' "$VERSION"
