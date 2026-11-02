#!/usr/bin/env bash
# 把 GitHub 上已经发布的一个版本同步到 Gitee：推送代码和标签，创建同名的发行版，上传和 GitHub
# 上一样的文件。国内服务器从 Gitee 下载程序快得多（run.sh 会先比较 Gitee 和 GitHub 的速度）。
# 从 GitHub 的服务器往 Gitee 上传很慢（实测每秒十几到几十 KB，还会断），所以文件按给出的顺序
# 上传（run.sh 要用的压缩包放在前面）；Gitee 上已经有的不再重传，失败了重新运行就能接着传。
# Gitee 上每个仓库的附件合计不能超过 1GB，所以只保留最近 10 个版本。
#   GITEE_TOKEN=私人令牌 scripts/gitee-release.sh 标签 文件……
set -euo pipefail

repo="lx969788249/dockhop" # Gitee 上的仓库，和 run.sh 里的 GITEE 地址一致
keep=10
tag="${1:?用法：GITEE_TOKEN=私人令牌 scripts/gitee-release.sh 标签 文件……}"
shift
token="${GITEE_TOKEN:?没有设置 GITEE_TOKEN（Gitee 的私人令牌）}"
api="https://gitee.com/api/v5/repos/${repo}"

# Gitee 上的发行版要挂在标签上；代码也一起推上去，和 GitHub 保持一致。
git push --force "https://${repo%%/*}:${token}@gitee.com/${repo}.git" HEAD:refs/heads/main "refs/tags/${tag}"

id="$(curl -sS "${api}/releases/tags/${tag}?access_token=${token}" | jq -r '.id? // empty' 2>/dev/null || true)"
if [[ -z "$id" ]]; then
  id="$(curl -fsS -X POST "${api}/releases" -F "access_token=${token}" -F "tag_name=${tag}" \
    -F "name=DockHop ${tag#v}" -F "target_commitish=$(git rev-list -n 1 "$tag")" \
    -F "body=和 GitHub 上的 ${tag} 相同：https://github.com/lx969788249/dockhop/releases/tag/${tag}" | jq -r .id)"
fi

# have 文件：Gitee 上已经有这个文件，大小也对得上。大小不对的（没传完整）删掉。
have() {
  local name size aid asize found=1
  name="$(basename "$1")"
  size="$(wc -c <"$1" | tr -d ' ')"
  while read -r aid asize; do
    if [[ "$asize" == "$size" ]]; then
      found=0
    else
      curl -fsS -X DELETE "${api}/releases/${id}/attach_files/${aid}?access_token=${token}" >/dev/null
    fi
  done < <(curl -fsS "${api}/releases/${id}/attach_files?access_token=${token}&per_page=100" |
    jq -r --arg name "$name" '.[] | select(.name == $name) | "\(.id) \(.size)"')
  return "$found"
}

for f in "$@"; do
  for attempt in 1 2 3; do
    if have "$f"; then
      echo "已有 $(basename "$f")"
      continue 2
    fi
    # 慢不要紧，5 分钟几乎没有进展才放弃这一次。下一次先看看是不是其实已经传上去了。
    if curl -fsS --speed-limit 1024 --speed-time 300 -X POST "${api}/releases/${id}/attach_files" \
      -F "access_token=${token}" -F "file=@${f}" >/dev/null; then
      echo "已上传 $(basename "$f")"
      continue 2
    fi
    echo "上传 $(basename "$f") 失败（第 ${attempt} 次）" >&2
  done
  exit 1
done

# 只保留最近的 keep 个版本。
curl -fsS "${api}/releases?access_token=${token}&per_page=100" |
  jq -r --argjson keep "$keep" 'sort_by(.id) | reverse | .[$keep:][] | .id' |
  while read -r old; do
    curl -fsS -X DELETE "${api}/releases/${old}?access_token=${token}" >/dev/null
    echo "已删除较老的发行版 ${old}"
  done

# 确认不登录也能下载：Gitee 上的仓库还没公开（或还在审核）时下载不了。
url="https://gitee.com/${repo}/releases/download/${tag}/dockhop-linux-amd64.gz"
if ! curl -fsSL -o /dev/null "$url"; then
  echo "不登录下载不了 ${url}：Gitee 上的仓库是不是还没公开，或者还在审核？" >&2
  exit 1
fi
echo "✓ 已同步到 Gitee：https://gitee.com/${repo}/releases/tag/${tag}"
