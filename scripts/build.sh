#!/usr/bin/env bash
# 编译三个架构的程序，并生成带校验值的 run.sh。
#   scripts/build.sh 版本号 输出目录
# 输出：dockhop-linux-amd64、dockhop-linux-arm64、dockhop-linux-armv7、SHA256SUMS、run.sh
set -euo pipefail

version="${1:?用法：scripts/build.sh 版本号 输出目录}"
out="${2:?用法：scripts/build.sh 版本号 输出目录}"
root="$(cd "$(dirname "$0")/.." && pwd)"
mkdir -p "$out"

build() {
  local arch="$1" goarch="$2" goarm="${3:-}"
  CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" GOARM="$goarm" \
    go build -C "$root" -trimpath -ldflags "-s -w -X main.version=${version}" \
    -o "${out}/dockhop-linux-${arch}" ./cmd/dockhop
}
build amd64 amd64
build arm64 arm64
build armv7 arm 7

(cd "$out" && sha256sum dockhop-linux-* >SHA256SUMS)
sum() { awk -v f="dockhop-linux-$1" '$2 == f {print $1}' "${out}/SHA256SUMS"; }
# 只替换开头的赋值行，脚本里其他地方出现同样的字样也不会被改到。
sed -e "s/^VERSION=\"__VERSION__\"/VERSION=\"${version}\"/" \
  -e "s/^SHA256_AMD64=\"__SHA256_AMD64__\"/SHA256_AMD64=\"$(sum amd64)\"/" \
  -e "s/^SHA256_ARM64=\"__SHA256_ARM64__\"/SHA256_ARM64=\"$(sum arm64)\"/" \
  -e "s/^SHA256_ARMV7=\"__SHA256_ARMV7__\"/SHA256_ARMV7=\"$(sum armv7)\"/" \
  "${root}/scripts/run.sh.in" >"${out}/run.sh"
chmod +x "${out}/run.sh"
ls -l "$out"
