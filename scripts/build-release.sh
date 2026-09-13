#!/usr/bin/env sh
# Build Linux/macOS releases with an embedded external sing-box executable.
# Usage: scripts/build-release.sh [version]
# Optional: SING_BOX_VERSION, SING_BOX_SOURCE (Git checkout), KARING_BUILD_TARGETS.
set -eu
set -f

die() {
  echo "$*" >&2
  exit 1
}

[ "$#" -le 1 ] || die "用法: scripts/build-release.sh [版本号]"
VERSION="${1:-dev}"
CORE_VERSION="${SING_BOX_VERSION:-1.14.0}"
TARGETS="${KARING_BUILD_TARGETS:-linux/amd64 linux/arm64 darwin/amd64 darwin/arm64}"
MODULE="github.com/bbbstyyy/karing-tui"
ROOT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
OUT="$ROOT_DIR/dist/karing-tui-${VERSION}"

# Reject path separators and shell metacharacters before creating output.
case "$VERSION" in
  ''|[!a-zA-Z0-9]*|*[!a-zA-Z0-9._-]*) die "非法版本号: $VERSION" ;;
esac
case "$CORE_VERSION" in
  ''|[!0-9]*|*[!a-zA-Z0-9._-]*) die "非法 sing-box 版本号（不要加 v）: $CORE_VERSION" ;;
esac
target_count=0
for target in $TARGETS; do
  case "$target" in
    linux/amd64|linux/arm64|darwin/amd64|darwin/arm64) ;;
    *) die "不支持的构建目标: $target" ;;
  esac
  target_count=$((target_count + 1))
done
[ "$target_count" -gt 0 ] || die "至少需要一个构建目标"
[ ! -e "$OUT" ] || die "输出目录已存在，请使用新版本号或先移走目录: $OUT"
for tool in go git tar gzip; do
  command -v "$tool" >/dev/null 2>&1 || die "缺少构建工具: $tool"
done

mkdir -p "$ROOT_DIR/dist"
WORK_DIR="$(mktemp -d "$ROOT_DIR/dist/.build-${VERSION}.XXXXXX")"
cleanup() {
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
PACKAGES="$WORK_DIR/packages"
mkdir -p "$PACKAGES"

# A local reference checkout is optional. Always export the requested tag,
# including when its current branch or working tree contains other changes.
CORE_REPO="${SING_BOX_SOURCE:-}"
if [ -z "$CORE_REPO" ]; then
  CORE_REPO="$WORK_DIR/sing-box-repo"
  echo "获取 sing-box v${CORE_VERSION} ..."
  git clone --quiet --depth 1 --no-checkout --branch "v${CORE_VERSION}" \
    https://github.com/SagerNet/sing-box.git "$CORE_REPO"
else
  CORE_REPO="$(CDPATH= cd -- "$CORE_REPO" && pwd)"
fi
CORE_REF="refs/tags/v${CORE_VERSION}"
CORE_REVISION="$(git -C "$CORE_REPO" rev-parse "${CORE_REF}^{commit}")"
CORE_NAME="sing-box-${CORE_VERSION}"
git -C "$CORE_REPO" archive --format=tar --prefix="${CORE_NAME}/" \
  "$CORE_REVISION" > "$WORK_DIR/sing-box-source.tar"
gzip -n -9 -c "$WORK_DIR/sing-box-source.tar" > "$PACKAGES/${CORE_NAME}-source.tar.gz"
tar -xf "$WORK_DIR/sing-box-source.tar" -C "$WORK_DIR"
CORE_SOURCE="$WORK_DIR/$CORE_NAME"
CORE_TAGS="$(cat "$CORE_SOURCE/release/DEFAULT_BUILD_TAGS_OTHERS")"
CORE_LDFLAGS="$(cat "$CORE_SOURCE/release/LDFLAGS")"

# Never write embedded payloads into the developer's source tree. This also
# lets normal go build/test commands run while a release is being assembled.
APP_SOURCE="$WORK_DIR/app"
mkdir -p "$APP_SOURCE"
cp "$ROOT_DIR/go.mod" "$ROOT_DIR/go.sum" "$APP_SOURCE/"
cp -R "$ROOT_DIR/cmd" "$ROOT_DIR/internal" "$APP_SOURCE/"
EMBED_DIR="$APP_SOURCE/internal/core/embedded"
find "$EMBED_DIR" -type f -name 'sing-box-*.bin.gz' -delete

for target in $TARGETS; do
  goos="${target%/*}"
  goarch="${target#*/}"
  echo "编译 sing-box ${CORE_VERSION} / karing-tui ${VERSION}: $target ..."
  (
    cd "$CORE_SOURCE"
    GOWORK=off CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
      go build -mod=readonly -buildvcs=false -trimpath -tags "$CORE_TAGS" \
        -ldflags "-X github.com/sagernet/sing-box/constant.Version=${CORE_VERSION} ${CORE_LDFLAGS} -s -w -buildid=" \
        -o "$WORK_DIR/sing-box" ./cmd/sing-box
  )
  asset="$EMBED_DIR/sing-box-${goos}-${goarch}.bin.gz"
  gzip -n -9 -c "$WORK_DIR/sing-box" > "$asset"

  package_dir="$WORK_DIR/package-${goos}-${goarch}"
  mkdir -p "$package_dir/licenses" "$package_dir/docs"
  (
    cd "$APP_SOURCE"
    GOWORK=off CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
      go build -mod=readonly -buildvcs=false -trimpath \
        -ldflags "-s -w -X ${MODULE}/internal/cli.Version=${VERSION} -X ${MODULE}/internal/core.EmbeddedVersion=${CORE_VERSION}" \
        -o "$package_dir/karing" ./cmd/karing
  )
  rm -f "$asset"
  cp "$ROOT_DIR/README.md" "$ROOT_DIR/LICENSE" "$ROOT_DIR/THIRD_PARTY_NOTICES.md" "$package_dir/"
  # Package user documentation explicitly; local development notes may be
  # present even when Git ignores them.
  cp "$ROOT_DIR/docs/services.md" "$ROOT_DIR/docs/karing-headless.service" \
    "$ROOT_DIR/docs/karing-headless.plist" "$package_dir/docs/"
  cp -R "$ROOT_DIR/licenses/." "$package_dir/licenses/"
  cp "$CORE_SOURCE/LICENSE" "$package_dir/licenses/sing-box-LICENSE.txt"
  cat > "$package_dir/BUILD-INFO.txt" <<EOF
karing-tui version: $VERSION
target: $target
Go toolchain: $(go version)
sing-box version: $CORE_VERSION
sing-box repository: https://github.com/SagerNet/sing-box
sing-box commit: $CORE_REVISION
sing-box source archive: ${CORE_NAME}-source.tar.gz
sing-box build tags: $CORE_TAGS
sing-box linker flags: $CORE_LDFLAGS
EOF
  COPYFILE_DISABLE=1 tar -czf "$PACKAGES/karing-tui-${VERSION}-${goos}-${goarch}.tar.gz" \
    -C "$package_dir" karing README.md LICENSE THIRD_PARTY_NOTICES.md licenses docs BUILD-INFO.txt
done

(
  cd "$PACKAGES"
  # Globbing was disabled while validating targets; enable it for archives.
  set +f
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum ./*.tar.gz > checksums.txt
  else
    shasum -a 256 ./*.tar.gz > checksums.txt
  fi
)
[ ! -e "$OUT" ] || die "输出目录在构建期间被创建，请使用新版本号: $OUT"
mv "$PACKAGES" "$OUT"
echo "完成: $OUT"
