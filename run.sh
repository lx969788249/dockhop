#!/usr/bin/env bash
# DockHop 一行命令入口：
#   bash <(curl -fsSL https://raw.githubusercontent.com/lx969788249/dockhop/main/run.sh) [命令和选项]
#
# 判断 CPU 架构 → 下载对应的程序 → 核对 SHA-256 → 运行 → 删除。
# 程序的校验值写在这个脚本里（发布时自动生成），从镜像站下载也能确认没被篡改。
# 国内访问 GitHub 慢时可以设置 DOCKHOP_MIRROR=镜像站地址。
set -euo pipefail

VERSION="1.0.0"
SHA256_AMD64="b5d2f3b621e735f5ea4eb87515a51646fbc16f5199b8da0904b4499eb4eb7425"
SHA256_ARM64="0fe838c54b8acc9119dd2f5fedbddf148a456e36641ba67518f047cf70700b89"
SHA256_ARMV7="5e4c160569204ad93ba1972dd39d3acb9426553c24899f5b390ed77c8d8909ed"
RELEASES="https://github.com/lx969788249/dockhop/releases/download"

fail() {
  printf '✗ %s\n' "$1" >&2
  [[ -z "${2:-}" ]] || printf '  建议：%s\n' "$2" >&2
  exit 1
}

case "$VERSION" in
  "" | *_VERSION_*) fail "DockHop 还没有发布正式版本" "现在请先使用 docker_migrate：https://github.com/lx969788249/docker_migrate" ;;
esac

[[ "$(uname -s)" == "Linux" ]] || fail "DockHop 只能在 Linux 服务器上运行"

case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 sha="$SHA256_AMD64" ;;
  aarch64 | arm64) arch=arm64 sha="$SHA256_ARM64" ;;
  armv7l | armv7 | armhf) arch=armv7 sha="$SHA256_ARMV7" ;;
  *) fail "不支持这种 CPU：$(uname -m)" "DockHop 支持 x86_64、arm64 和 armv7" ;;
esac

base="${DOCKHOP_MIRROR:-$RELEASES}"
url="${base%/}/v${VERSION}/dockhop-linux-${arch}"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/dockhop.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT
bin="${tmp}/dockhop"

printf '正在下载 DockHop %s（%s）……\n' "$VERSION" "$arch" >&2
if command -v curl >/dev/null 2>&1; then
  curl -fL --retry 3 --connect-timeout 15 -sS -o "$bin" "$url" || fail "下载失败：$url" \
    "检查网络；国内服务器访问 GitHub 慢时，可以设置 DOCKHOP_MIRROR=镜像站地址 后重试"
elif command -v wget >/dev/null 2>&1; then
  wget -q -T 15 -t 3 -O "$bin" "$url" || fail "下载失败：$url" \
    "检查网络；国内服务器访问 GitHub 慢时，可以设置 DOCKHOP_MIRROR=镜像站地址 后重试"
else
  fail "需要 curl 或 wget 才能下载 DockHop"
fi

if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "$bin" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
  actual="$(shasum -a 256 "$bin" | awk '{print $1}')"
else
  fail "需要 sha256sum 才能核对下载的程序"
fi
[[ "$actual" == "$sha" ]] || fail "下载的程序校验不通过，已删除" \
  "可能下载不完整，或镜像站提供的文件被改动过；请重试，或换一个下载地址"

chmod +x "$bin"
rc=0
"$bin" "$@" || rc=$?
exit "$rc"
