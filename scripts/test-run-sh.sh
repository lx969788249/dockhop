#!/usr/bin/env bash
# 测试 run.sh：用本地下载服务代替 GitHub，确认它能下载、核对、运行程序并传递参数，
# 并在校验值不对、没有发布版本时给出明确的提示。
#   scripts/test-run-sh.sh 输出目录（scripts/build.sh 的输出）
set -euo pipefail
trap 'echo "test-run-sh.sh: 第 ${LINENO} 行失败" >&2' ERR

dist="$(cd "${1:?用法：scripts/test-run-sh.sh 输出目录}" && pwd)"
root="$(cd "$(dirname "$0")/.." && pwd)"
version="$(sed -n 's/^VERSION="\(.*\)"$/\1/p' "${dist}/run.sh")"
work="$(mktemp -d)"
trap 'kill "${server:-}" 2>/dev/null || true; rm -rf "$work"' EXIT

# full 有压缩包和没压缩的程序（和 GitHub Release 一样），plain 只有没压缩的（老版本）。
mkdir -p "${work}/srv/full/v${version}" "${work}/srv/plain/v${version}"
cp "${dist}"/dockhop-linux-* "${work}/srv/full/v${version}/"
cp "${dist}"/dockhop-linux-amd64 "${dist}"/dockhop-linux-arm64 "${dist}"/dockhop-linux-armv7 "${work}/srv/plain/v${version}/"
port=$((20000 + RANDOM % 20000))
(cd "${work}/srv" && exec python3 -m http.server "$port" --bind 127.0.0.1) >"${work}/server.log" 2>&1 &
server=$!
for _ in $(seq 1 50); do
  curl -s -o /dev/null "http://127.0.0.1:${port}/" && break
  sleep 0.1
done
export DOCKHOP_MIRROR="http://127.0.0.1:${port}/full"

out="$(bash "${dist}/run.sh" version 2>&1)"
grep -Fq "DockHop ${version}（linux/" <<<"$out"
grep -q "GET /full/v${version}/dockhop-linux-[a-z0-9]*\.gz HTTP/[0-9.]*\" 200" "${work}/server.log"
echo "✓ 下载压缩包、核对、运行：${out##*$'\n'}"

out="$(DOCKHOP_MIRROR="http://127.0.0.1:${port}/plain" bash "${dist}/run.sh" version 2>&1)"
grep -Fq "DockHop ${version}（linux/" <<<"$out"
echo "✓ 没有压缩包（老版本）时下载没压缩的程序"

out="$(bash "${dist}/run.sh" in --existing merge x 2>&1)" && exit 1
grep -Fq "replace、skip 或 fail" <<<"$out"
echo "✓ 参数原样传给程序，退出码原样返回"

sed 's/^SHA256_AMD64=".*"/SHA256_AMD64="0000"/; s/^SHA256_ARM64=".*"/SHA256_ARM64="0000"/' "${dist}/run.sh" >"${work}/bad.sh"
out="$(bash "${work}/bad.sh" version 2>&1)" && exit 1
grep -Fq "下载的程序校验不通过，已删除" <<<"$out"
echo "✓ 校验值不对时拒绝运行"

out="$(bash "${root}/scripts/run.sh.in" version 2>&1)" && exit 1
grep -Fq "还没有发布正式版本" <<<"$out"
echo "✓ 没有发布版本时说明原因"

out="$(DOCKHOP_MIRROR="http://127.0.0.1:1" bash "${dist}/run.sh" version 2>&1)" && exit 1
grep -Fq "DOCKHOP_MIRROR" <<<"$out"
echo "✓ 下载失败时提示可以换镜像站"
