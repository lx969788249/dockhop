#!/usr/bin/env bash
# 测试 run.sh：用本地下载服务代替 Gitee 和 GitHub，确认它能下载、核对、运行程序并传递参数，
# 能从两个地址里挑快的、下载中断时换地址接着下载，并在校验值不对、没有发布版本时给出明确的提示。
#   scripts/test-run-sh.sh 输出目录（scripts/build.sh 的输出）
set -euo pipefail
trap 'echo "test-run-sh.sh: 第 ${LINENO} 行失败" >&2' ERR

dist="$(cd "${1:?用法：scripts/test-run-sh.sh 输出目录}" && pwd)"
root="$(cd "$(dirname "$0")/.." && pwd)"
version="$(sed -n 's/^VERSION="\(.*\)"$/\1/p' "${dist}/run.sh")"
work="$(mktemp -d)"
trap 'kill "${server:-}" 2>/dev/null || true; rm -rf "$work"' EXIT

# 下载服务按地址的第一段模拟不同的下载地址（见 test-server.py）。plain 只有没压缩的程序（老版本），
# 其他都有压缩包和没压缩的程序（和 Release 一样）。
for dir in full plain slow cut norange-slow; do
  mkdir -p "${work}/srv/${dir}/v${version}"
  if [[ "$dir" == plain ]]; then
    ln -s "${dist}"/dockhop-linux-amd64 "${dist}"/dockhop-linux-arm64 "${dist}"/dockhop-linux-armv7 "${work}/srv/${dir}/v${version}/"
  else
    ln -s "${dist}"/dockhop-linux-* "${work}/srv/${dir}/v${version}/"
  fi
done
port=$((20000 + RANDOM % 20000))
python3 "${root}/scripts/test-server.py" "$port" "${work}/srv" >>"${work}/server.log" 2>&1 &
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

# both Gitee 用的服务 GitHub 用的服务：把 run.sh 里的这两个地址换成本地的下载服务再运行。
both() {
  sed -e "s#^GITEE=.*#GITEE=\"http://127.0.0.1:${port}/$1\"#" \
    -e "s#^GITHUB=.*#GITHUB=\"http://127.0.0.1:${port}/$2\"#" "${dist}/run.sh" >"${work}/both.sh"
  : >"${work}/server.log"
  DOCKHOP_MIRROR="" bash "${work}/both.sh" version 2>&1
}

out="$(both slow full)"
grep -Fq "DockHop ${version}（linux/" <<<"$out"
[[ "$(grep -c '"GET /slow/' "${work}/server.log")" == 1 ]] # 只有比速度时的一次
[[ "$(grep -c '"GET /full/' "${work}/server.log")" == 2 ]]
echo "✓ 先比一比两个地址，从快的那个下载"

out="$(both cut slow)"
grep -Fq "DockHop ${version}（linux/" <<<"$out"
grep -Fq "改从 127.0.0.1:${port} 下载" <<<"$out"
grep -Eq "range /slow/[^ ]+ bytes=[1-9][0-9]*-" "${work}/server.log"
echo "✓ 下载中断时换另一个地址，从断开的地方接着下载"

out="$(both cut norange-slow)"
grep -Fq "DockHop ${version}（linux/" <<<"$out"
grep -Eq "range /norange-slow/[^ ]+ bytes=[1-9][0-9]*-" "${work}/server.log"
[[ "$(grep -c '"GET /norange-slow/' "${work}/server.log")" == 3 ]] # 比速度、试着接着下载、从头下载
echo "✓ 换到的地址不支持接着下载时从头下载"
