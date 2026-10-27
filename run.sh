#!/usr/bin/env bash
# DockHop 一行命令入口：
#   bash <(curl -fsSL dockhop.run) [命令和选项]
#
# 判断 CPU 架构 → 下载对应的程序 → 核对 SHA-256 → 运行 → 删除。
# 程序的校验值写在这个脚本里（发布时自动生成），从哪里下载都能确认没被篡改。
# 先从 dockhop.run 下载（Cloudflare 缓存，国内一般比 GitHub 快），太慢或失败时自动换 GitHub；
# 设置了 DOCKHOP_MIRROR=镜像站地址 时只用这个镜像站。
set -euo pipefail

VERSION="1.0.7"
SHA256_AMD64="ef55c33fb54d1bc799f924f259ff0649cf45cc5857cf47a26b0c0cb5d4018d8c"
SHA256_ARM64="d670feb5f0244b3b9eabed5571bebe9ca41f1ecf55cd63d70cd2dfe90d77c8da"
SHA256_ARMV7="75e3580f0f87aa605c0490f1f56e3c8aa5bb21ad2bb0f79a56930bd79ec86422"
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

name="dockhop-linux-${arch}"
if [[ -n "${DOCKHOP_MIRROR:-}" ]]; then
  sources=("${DOCKHOP_MIRROR%/}")
else
  sources=("https://dockhop.run" "$RELEASES")
fi
tmp="$(mktemp -d "${TMPDIR:-/tmp}/dockhop.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT
bin="${tmp}/dockhop"

if command -v curl >/dev/null 2>&1; then
  getter=curl
elif command -v wget >/dev/null 2>&1; then
  getter=wget
else
  fail "需要 curl 或 wget 才能下载 DockHop"
fi
if command -v sha256sum >/dev/null 2>&1; then
  sumcmd=(sha256sum)
elif command -v shasum >/dev/null 2>&1; then
  sumcmd=(shasum -a 256)
else
  fail "需要 sha256sum 才能核对下载的程序"
fi
sha256() { "${sumcmd[@]}" "$1" | awk '{print $1}'; }

human() { awk -v n="$1" 'BEGIN { if (n >= 1048576) printf "%.1fMB", n / 1048576; else printf "%dKB", n / 1024 }'; }

# show_progress 下载进程 文件：下载时每隔一会儿在同一行显示下载了多少、速度，下载完擦掉这一行。
# 不用 curl 自己的进度条：它出错时会在屏幕上打出“curl: (28) …”这样的英文。
show_progress() {
  local pid="$1" out="$2" start=$SECONDS size total line
  while kill -0 "$pid" 2>/dev/null; do
    size="$(wc -c <"$out" 2>/dev/null || echo 0)"
    total="$(awk 'tolower($1) == "content-length:" { n = $2 + 0 } END { print n + 0 }' "${tmp}/headers" 2>/dev/null || echo 0)"
    line="已下载 $(human "$size")"
    if ((total > 0 && size <= total)); then
      line="${line} / $(human "$total")（$((size * 100 / total))%）"
    fi
    if ((SECONDS > start)); then
      line="${line} · $(human $((size / (SECONDS - start))))/s"
    fi
    printf '\r  %s\033[K' "$line" >&2
    sleep 0.3 2>/dev/null || sleep 1
  done
  printf '\r\033[K' >&2
}

# fetch 地址 文件 是否最后一个地址：下载一个文件，屏幕是终端时显示进度。
# 不是最后一个地址时，连不上或者 15 秒内平均不到 20KB/s 就放弃，换下一个地址。
# 返回 2 表示地址上没有这个文件（例如老版本没有压缩包），其他非 0 表示下载失败。
fetch() {
  local url="$1" out="$2" last="$3" rc=0 pid
  : >"$out"
  : >"${tmp}/headers"
  if [[ "$getter" == curl ]]; then
    local -a opts=(-sL --connect-timeout 10 -o "$out" -D "${tmp}/headers" -w '%{http_code}')
    if [[ "$last" == 1 ]]; then
      opts+=(--retry 3)
    else
      opts+=(--speed-limit 20480 --speed-time 15)
    fi
    curl "${opts[@]}" "$url" >"${tmp}/code" &
  else
    local -a opts=(-q -T 15 -O "$out")
    if [[ "$last" == 1 ]]; then opts+=(-t 3); else opts+=(-t 1); fi
    wget "${opts[@]}" "$url" &
  fi
  pid=$!
  [[ ! -t 2 ]] || show_progress "$pid" "$out"
  wait "$pid" || rc=$?
  if [[ "$getter" == curl ]]; then
    ((rc == 0)) || return "$rc"
    case "$(cat "${tmp}/code")" in
      404) return 2 ;;
      2??) return 0 ;;
      *) return 1 ;;
    esac
  fi
  ((rc != 8)) || return 2 # wget：服务器返回了错误（例如 404）
  return "$rc"
}

# download 地址 是否最后一个地址：从一个地址下载程序到 $bin 并核对校验值。
# 优先下载压缩过的（小一半多），地址上没有压缩包时（例如老版本）再下载没压缩的。
# 返回 0 表示成功，3 表示下载到了但校验不通过，其他表示下载失败。
download() {
  local url="$1/v${VERSION}/${name}" last="$2" rc=2
  rm -f "$bin" "${bin}.gz"
  if command -v gzip >/dev/null 2>&1; then
    rc=0
    fetch "${url}.gz" "${bin}.gz" "$last" || rc=$?
    if ((rc == 0)) && ! gzip -dc "${bin}.gz" >"$bin" 2>/dev/null; then
      return 1
    fi
  fi
  if ((rc == 2)); then
    rc=0
    fetch "$url" "$bin" "$last" || rc=$?
  fi
  ((rc == 0)) || return 1
  [[ "$(sha256 "$bin")" == "$sha" ]] || return 3
}

printf '正在下载 DockHop %s（%s）……\n' "$VERSION" "$arch" >&2
bad_sum=""
for i in "${!sources[@]}"; do
  last=$((i == ${#sources[@]} - 1))
  rc=0
  download "${sources[i]}" "$last" || rc=$?
  ((rc != 0)) || break
  why="下载太慢或失败"
  if ((rc == 3)); then
    bad_sum=1
    why="下载的程序校验不通过"
  fi
  if ((last)); then
    [[ -z "$bad_sum" ]] || fail "下载的程序校验不通过，已删除" "可能下载不完整，或镜像站提供的文件被改动过；请重试，或换一个下载地址"
    fail "下载失败：${sources[i]}/v${VERSION}/${name}" "检查网络；都下载不了时，可以设置 DOCKHOP_MIRROR=镜像站地址 后重试"
  fi
  printf '从 %s %s，改从 %s 下载……\n' "${sources[i]#https://}" "$why" "${sources[i + 1]#https://}" >&2
done

chmod +x "$bin"
rc=0
"$bin" "$@" || rc=$?
exit "$rc"
