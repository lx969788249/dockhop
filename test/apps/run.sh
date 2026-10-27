#!/usr/bin/env bash
# 两台模拟服务器之间迁移真实应用。
#
# 每台“服务器”是一个独立的 Docker-in-Docker 容器：有自己的 Docker、文件和网络，
# 新服务器上没有旧服务器的任何东西，只能通过网络从旧服务器下载迁移包。
#
# 用法：
#   test/apps/run.sh <应用名>...    应用名就是 test/apps/ 下的目录名
#   test/apps/run.sh unreachable   新服务器连不上旧服务器时的提示
#   test/apps/run.sh digest        按摘要固定版本的镜像（新服务器连不上镜像仓库 / 连得上）
#
# 每个应用目录里：
#   compose.yml 等   应用本身（官方 Compose 文件，固定版本）
#   app.env          可选：APP_PROJECT（项目名，默认目录名）、NEW_MEMORY（新服务器内存上限）
#   up.sh            可选：自定义启动方式；默认 docker compose up -d --wait
#   seed.sh          在旧服务器上写入一份能认出来的数据
#   verify.sh        在新服务器上确认这份数据还在
#
# 环境变量：
#   DOCKHOP_BIN=程序          要测试的 DockHop（linux 静态编译）；不设置时自动编译
#   OLD_DOCKER=27.5           旧服务器的 Docker 版本；NEW_DOCKER 同理（例如 20.10）
#   DOCKHOP_APPS_ARTIFACTS=目录  失败时把日志复制到那里
set -euo pipefail
trap 'echo "${BASH_SOURCE[0]##*/}: 第 ${LINENO} 行失败（退出码 $?）" >&2' ERR

APPS_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "${APPS_DIR}/../.." && pwd)"
OLD_DOCKER="${OLD_DOCKER:-27.5}"
NEW_DOCKER="${NEW_DOCKER:-27.5}"
BACKUP_PORT=18080
LINK_PATTERN='http://old:[0-9]+/[A-Za-z0-9]+/#[0-9a-z]{58}'

id="dhapps$$"
old="${id}_old"
new="${id}_new"
net="${id}_net"
tmp="$(mktemp -d)"
current=""

log() { printf '\n==> %s\n' "$*"; }
elapsed() { printf '%d 分 %02d 秒' $((($1) / 60)) $((($1) % 60)); }

save_artifacts() {
  [[ -n "${DOCKHOP_APPS_ARTIFACTS:-}" && -n "$current" ]] || return 0
  local dest="${DOCKHOP_APPS_ARTIFACTS}/${current}"
  mkdir -p "$dest"
  cp "$tmp"/*.log "$tmp"/*.json "$dest"/ 2>/dev/null || true
  for host in "$old" "$new"; do
    docker cp "${host}:/root/.dockhop/logs" "${dest}/${host##*_}-logs" >/dev/null 2>&1 || true
    docker exec "$host" sh -c 'docker ps -a; for c in $(docker ps -aq); do echo "--- $c"; docker logs --tail 40 "$c" 2>&1; done' \
      >"${dest}/${host##*_}-containers.txt" 2>&1 || true
  done
}

remove_hosts() {
  docker rm -fv "$old" "$new" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
}

cleanup() {
  local rc=$?
  ((rc == 0)) || save_artifacts
  remove_hosts
  rm -rf "$tmp"
  exit "$rc"
}
trap cleanup EXIT

# 在某台服务器上执行脚本：先加载 lib.sh，再执行给定的文件。
run_script_on() {
  local host="$1" file="$2"
  shift 2
  cat "${APPS_DIR}/lib.sh" "$file" |
    docker exec -i -e DM_SCRIPT="$(basename "$file")" "$@" "$host" bash -s
}

host_image() { printf 'dockhop-test-host:%s\n' "$1"; }

start_hosts() {
  local new_memory="${1:-}" host i
  docker network create "$net" >/dev/null
  docker run -d --privileged --name "$old" --hostname old \
    --network "$net" --network-alias old "$(host_image "$OLD_DOCKER")" >/dev/null
  docker run -d --privileged --name "$new" --hostname new \
    --network "$net" --network-alias new ${new_memory:+--memory "$new_memory"} "$(host_image "$NEW_DOCKER")" >/dev/null
  for host in "$old" "$new"; do
    for i in $(seq 1 60); do
      docker exec "$host" docker info >/dev/null 2>&1 && break
      ((i < 60)) || {
        echo "模拟服务器 ${host} 里的 Docker 没有启动" >&2
        docker logs --tail 50 "$host" >&2
        return 1
      }
      sleep 1
    done
    docker cp "$DOCKHOP_BIN" "${host}:/usr/local/bin/dockhop"
    show_storage "$host"
  done
}

# 显示模拟服务器的 Docker 版本和镜像存储方式。Docker 29 起新装的默认用 containerd 存储，
# 镜像 ID、导出、docker diff 的行为都和以前不同，这些任务必须真的跑在 containerd 存储上。
show_storage() {
  local host="$1" info version store="经典存储"
  info="$(docker exec "$host" docker info -f '{{.ServerVersion}} {{json .DriverStatus}}')"
  version="${info%% *}"
  [[ "$info" != *containerd* ]] || store="containerd 存储"
  echo "  ${host##*_}：Docker ${version} · ${store}"
  if ((${version%%.*} >= 29)) && [[ "$store" != "containerd 存储" ]]; then
    echo "Docker ${version} 应默认使用 containerd 存储，测试没有覆盖到要测的情况" >&2
    return 1
  fi
}

# 镜像先在本机拉取再导入旧服务器：本机有缓存时不用重复下载。
preload_images() {
  local host="$1" img
  shift
  local -a images=()
  for img in "$@"; do
    [[ -n "$img" ]] || continue
    docker image inspect "$img" >/dev/null 2>&1 || docker pull -q "$img" >/dev/null
    images+=("$img")
  done
  ((${#images[@]} > 0)) || return 0
  docker save "${images[@]}" | docker exec -i "$host" docker load -q >/dev/null
}

compose_images() {
  local dir="$1"
  (cd "$dir" && docker compose config --images 2>/dev/null) |
    while read -r img; do
      # 需要现场构建的镜像拉不到，交给 up.sh 构建。
      if docker image inspect "$img" >/dev/null 2>&1 || docker manifest inspect "$img" >/dev/null 2>&1; then
        printf '%s\n' "$img"
      fi
    done | sort -u
}

# 等容器的健康检查都有结果，避免把“启动中”的状态拿去比较。
wait_settled() {
  local host="$1" i
  for i in $(seq 1 100); do
    if ! docker exec "$host" sh -c \
      'docker ps -q | xargs -r docker inspect -f "{{if .State.Health}}{{.State.Health.Status}}{{end}}"' |
      grep -q starting; then
      return 0
    fi
    sleep 3
  done
  echo "${host} 上有容器的健康检查一直没有结果" >&2
  return 1
}

dump_state() {
  docker exec -i "$1" bash -s <"${APPS_DIR}/state.sh"
}

start_out() {
  local containers
  containers="$(docker exec "$old" docker ps -a --format '{{.Names}}' | sort | paste -sd, -)"
  docker exec -d "$old" bash -c "cd /root && DOCKHOP_KEEP_LOG=1 dockhop out --include '${containers}' \
    --stop-shared --host old --port ${BACKUP_PORT} </dev/null >/root/out.log 2>&1"
}

wait_for_link() {
  local link="" i
  for i in $(seq 1 900); do
    docker exec "$old" cat /root/out.log >"${tmp}/out.log" 2>/dev/null || true
    link="$(grep -oE "$LINK_PATTERN" "${tmp}/out.log" | tail -n1 || true)"
    [[ -z "$link" ]] || break
    if grep -Fq '❌ 迁出失败' "${tmp}/out.log" || { ((i > 5)) &&
      ! docker exec "$old" pgrep -f 'dockhop out' >/dev/null; }; then
      echo "旧服务器迁出失败" >&2
      tail -n 40 "${tmp}/out.log" >&2
      return 1
    fi
    sleep 2
  done
  [[ -n "$link" ]] || {
    echo "等待迁移链接超时" >&2
    tail -n 40 "${tmp}/out.log" >&2
    return 1
  }
  printf '%s\n' "$link"
}

# 等旧服务器的迁出结束（新服务器迁入成功后它会自动结束）。
wait_out_finished() {
  local i
  for i in $(seq 1 60); do
    docker exec "$old" pgrep -f 'dockhop out' >/dev/null || {
      docker exec "$old" cat /root/out.log >"${tmp}/out.log"
      grep -Fq '新服务器已迁入成功' "${tmp}/out.log" && grep -Fq '✅ 迁移完成' "${tmp}/out.log" && return 0
      echo "旧服务器的迁出没有正常结束" >&2
      tail -n 30 "${tmp}/out.log" >&2
      return 1
    }
    sleep 1
  done
  echo "新服务器迁入成功后，旧服务器的迁出应自动结束" >&2
  return 1
}

stop_out() {
  local i
  docker exec "$old" pkill -TERM -f 'dockhop out' || true
  for i in $(seq 1 60); do
    docker exec "$old" pgrep -f 'dockhop out' >/dev/null || return 0
    sleep 1
  done
  echo "旧服务器的迁出没有退出" >&2
  return 1
}

run_in() {
  local link="$1" log="$2"
  shift 2
  docker exec -e DH_LINK="$link" "$@" "$new" bash -c \
    'cd /root && DOCKHOP_KEEP_LOG=1 dockhop in --yes "$DH_LINK" </dev/null' >"$log" 2>&1
}

compare_state() {
  local before="$1" after="$2" what="$3"
  if ! diff <(jq -S . "$before") <(jq -S . "$after") >"${tmp}/state.diff"; then
    echo "${what}：容器配置不一致" >&2
    cat "${tmp}/state.diff" >&2
    return 1
  fi
  echo "  ✓ ${what}：$(jq length "$after") 个容器的状态、端口、网络、挂载、环境变量一致"
}

run_app() {
  local app="$1" dir="${APPS_DIR}/$1" started=$SECONDS t project new_memory link ids_before ids_after
  current="$app"
  [[ -f "${dir}/seed.sh" && -f "${dir}/verify.sh" ]] || {
    echo "未知应用：${app}" >&2
    return 1
  }
  APP_PROJECT="" NEW_MEMORY=""
  # shellcheck disable=SC1091
  [[ ! -f "${dir}/app.env" ]] || source "${dir}/app.env"
  project="${APP_PROJECT:-$app}"
  new_memory="${NEW_MEMORY:-}"

  log "[${app}] 准备两台模拟服务器（Docker ${OLD_DOCKER} → ${NEW_DOCKER}）${new_memory:+，新服务器内存 ${new_memory}}"
  start_hosts "$new_memory"
  mapfile -t images < <(compose_images "$dir")
  preload_images "$old" alpine:3.20 "${images[@]}"
  docker exec "$old" mkdir -p /opt/apps
  docker cp "$dir" "${old}:/opt/apps/${app}"

  log "[${app}] 在旧服务器上启动应用并写入数据"
  t=$SECONDS
  if [[ -f "${dir}/up.sh" ]]; then
    run_script_on "$old" "${dir}/up.sh" -w "/opt/apps/${app}"
  else
    docker exec -w "/opt/apps/${app}" "$old" \
      docker compose -p "$project" up -d --wait --wait-timeout 300
  fi
  run_script_on "$old" "${dir}/seed.sh" -w "/opt/apps/${app}" -e APP_PROJECT="$project"
  wait_settled "$old"
  dump_state "$old" >"${tmp}/before.json"
  echo "  启动并写入数据用时 $(elapsed $((SECONDS - t)))"

  log "[${app}] 旧服务器迁出"
  t=$SECONDS
  start_out
  link="$(wait_for_link)"
  echo "  迁出用时 $(elapsed $((SECONDS - t)))，$(grep -E '^\[[0-9]/[0-9]\]' "${tmp}/out.log" | grep '✓' | tail -n 3 | tr '\n' ' ')"

  log "[${app}] 新服务器迁入"
  t=$SECONDS
  if ! run_in "$link" "${tmp}/in.log"; then
    echo "迁入失败" >&2
    tail -n 60 "${tmp}/in.log" >&2
    return 1
  fi
  grep -Fq '✅ 迁入完成' "${tmp}/in.log" || {
    echo "迁入结果不是成功" >&2
    tail -n 60 "${tmp}/in.log" >&2
    return 1
  }
  grep -E '^\[[0-9]/[0-9]\].*✓' "${tmp}/in.log" | sed 's/^/  /' || true
  echo "  迁入用时 $(elapsed $((SECONDS - t)))"

  log "[${app}] 检查新服务器上的应用和数据"
  wait_settled "$new"
  run_script_on "$new" "${dir}/verify.sh" -e APP_PROJECT="$project"
  dump_state "$new" >"${tmp}/after.json"
  compare_state "${tmp}/before.json" "${tmp}/after.json" "新服务器与旧服务器"
  if docker exec "$new" docker ps -a --format '{{.Label "com.docker.compose.project"}}' |
    grep -v -x -F -e "$project" -e '' | grep -q .; then
    echo "新服务器上多出了别的 Compose 项目" >&2
    docker exec "$new" docker ps -a >&2
    return 1
  fi
  if [[ ! -f "${dir}/up.sh" ]]; then
    # 迁移后在原目录照常 docker compose up：配置和文件一致，不应重建任何容器。
    ids_before="$(docker exec "$new" docker ps -aq --no-trunc | sort)"
    docker exec -w "/opt/apps/${app}" "$new" docker compose -p "$project" up -d >"${tmp}/compose-up.log" 2>&1 || {
      echo "迁移后在原目录 docker compose up 失败" >&2
      cat "${tmp}/compose-up.log" >&2
      return 1
    }
    ids_after="$(docker exec "$new" docker ps -aq --no-trunc | sort)"
    [[ "$ids_before" == "$ids_after" ]] || {
      echo "迁移后 docker compose up 重建了容器（配置和文件不一致）" >&2
      cat "${tmp}/compose-up.log" >&2
      return 1
    }
    echo "  ✓ 迁移后在原目录 docker compose up，没有重建任何容器"
  fi

  log "[${app}] 新服务器迁入成功后，旧服务器自动结束并恢复原状"
  wait_out_finished
  wait_settled "$old"
  dump_state "$old" >"${tmp}/old-after.json"
  compare_state "${tmp}/before.json" "${tmp}/old-after.json" "旧服务器"

  remove_hosts
  current=""
  log "[${app}] 通过，总用时 $(elapsed $((SECONDS - started)))"
}

# 新服务器连不上旧服务器时，第一步就要说清楚原因，而不是重试几分钟。
run_unreachable() {
  local link t rc
  current="unreachable"
  log "[unreachable] 准备两台模拟服务器"
  start_hosts ""
  preload_images "$old" alpine:3.20
  docker exec "$old" docker run -d --name dm_probe alpine:3.20 sleep 3600 >/dev/null
  start_out
  link="$(wait_for_link)"

  log "[unreachable] 端口被防火墙丢弃（云厂商安全组没放行时就是这样）"
  docker exec "$old" iptables -I INPUT -p tcp --dport "$BACKUP_PORT" -j DROP
  t=$SECONDS
  rc=0
  run_in "$link" "${tmp}/drop.log" -e DOCKHOP_TEST_CONNECT_TIMEOUT=5s || rc=$?
  ((rc != 0)) || {
    echo "端口被拦截时迁入不应该成功" >&2
    return 1
  }
  ((SECONDS - t < 60)) || {
    echo "过了 $((SECONDS - t)) 秒才报错，应该很快说明原因" >&2
    return 1
  }
  grep -Fq "连接旧服务器 old 的端口 ${BACKUP_PORT} 超时" "${tmp}/drop.log"
  grep -Fq "安全组" "${tmp}/drop.log"
  grep -Fq "ufw allow ${BACKUP_PORT}/tcp" "${tmp}/drop.log"
  grep -Fq "还没有下载或改动任何东西" "${tmp}/drop.log"
  echo "  ✓ $((SECONDS - t)) 秒内提示：连接超时，请在安全组和防火墙放行 TCP ${BACKUP_PORT}"

  log "[unreachable] 端口被防火墙拒绝"
  docker exec "$old" iptables -D INPUT -p tcp --dport "$BACKUP_PORT" -j DROP
  docker exec "$old" iptables -I INPUT -p tcp --dport "$BACKUP_PORT" -j REJECT --reject-with tcp-reset
  rc=0
  run_in "$link" "${tmp}/reject.log" -e DOCKHOP_TEST_CONNECT_TIMEOUT=5s || rc=$?
  ((rc != 0))
  grep -Fq "旧服务器 old 的端口 ${BACKUP_PORT} 拒绝连接" "${tmp}/reject.log"
  grep -Fq "ufw allow ${BACKUP_PORT}/tcp" "${tmp}/reject.log"
  echo "  ✓ 提示：拒绝连接，请确认迁出还在运行并放行端口"

  log "[unreachable] 放行后同一个链接可以正常使用"
  docker exec "$old" iptables -D INPUT -p tcp --dport "$BACKUP_PORT" -j REJECT --reject-with tcp-reset
  docker exec -e DH_LINK="$link" "$new" bash -c 'cd /root && dockhop check "$DH_LINK" </dev/null' >"${tmp}/check.log" 2>&1
  grep -Fq '✅ 迁入前检查通过' "${tmp}/check.log"
  echo "  ✓ 迁入前检查通过"

  stop_out
  remove_hosts
  current=""
  log "[unreachable] 通过"
}

# 按摘要固定版本的镜像：导出的镜像里没有仓库清单，新服务器上按摘要找不到（设计文档 C14）。
run_digest() {
  local digest ref link
  current="digest"
  log "[digest] 准备两台模拟服务器"
  start_hosts ""
  docker pull -q alpine:3.20 >/dev/null
  digest="$(docker image inspect -f '{{index .RepoDigests 0}}' alpine:3.20)"
  ref="alpine:3.20@${digest#*@}"
  docker exec "$old" docker pull -q "$ref" >/dev/null
  docker exec "$old" docker run -d --name dm_pinned --restart unless-stopped "$ref" sleep infinity >/dev/null

  log "[digest] 新服务器连不上镜像仓库：检查时询问，--yes 改用迁移包里的镜像"
  start_out
  link="$(wait_for_link)"
  # 新服务器只放行到旧服务器所在的网络，其他出站全部丢弃。
  docker exec "$new" sh -c "iptables -I OUTPUT -o lo -j ACCEPT && iptables -I OUTPUT -d \$(ip -4 -o addr show eth0 | awk '{print \$4}') -j ACCEPT && iptables -A OUTPUT -j DROP"
  run_in "$link" "${tmp}/in-offline.log" || {
    echo "连不上镜像仓库时也应能迁入" >&2
    tail -n 40 "${tmp}/in-offline.log" >&2
    return 1
  }
  grep -Fq '镜像按摘要固定了版本' "${tmp}/in-offline.log"
  grep -Fq '改用迁移包里的镜像' "${tmp}/in-offline.log"
  [[ "$(docker exec "$new" docker inspect -f '{{.Config.Image}} {{.State.Status}}' dm_pinned)" == "alpine:3.20 running" ]]
  echo "  ✓ 改用迁移包里的镜像（alpine:3.20），容器正常运行"
  wait_out_finished

  log "[digest] 新服务器连得上镜像仓库：按摘要补齐清单，配置和旧服务器完全一样"
  docker exec "$new" sh -c 'docker rm -f dm_pinned >/dev/null && docker image rm -f alpine:3.20 >/dev/null; iptables -F OUTPUT'
  start_out
  link="$(wait_for_link)"
  run_in "$link" "${tmp}/in-online.log" || {
    tail -n 40 "${tmp}/in-online.log" >&2
    return 1
  }
  [[ "$(docker exec "$new" docker inspect -f '{{.Config.Image}} {{.State.Status}}' dm_pinned)" == "${ref} running" ]]
  docker exec "$new" docker image inspect "$ref" >/dev/null
  echo "  ✓ 镜像名保留 @sha256，按摘要能找到镜像"
  wait_out_finished
  remove_hosts
  current=""
  log "[digest] 通过"
}

(($# > 0)) || {
  sed -n '2,23p' "$0" | sed 's/^# \{0,1\}//'
  exit 2
}
for bin in docker jq; do
  command -v "$bin" >/dev/null 2>&1 || {
    echo "需要 ${bin}" >&2
    exit 1
  }
done
if [[ -z "${DOCKHOP_BIN:-}" ]]; then
  DOCKHOP_BIN="${tmp}/dockhop"
  CGO_ENABLED=0 GOOS=linux go build -C "$ROOT_DIR" -o "$DOCKHOP_BIN" ./cmd/dockhop
fi
for v in $(printf '%s\n' "$OLD_DOCKER" "$NEW_DOCKER" | sort -u); do
  docker build -q --build-arg DIND_VERSION="$v" -t "$(host_image "$v")" -f "${APPS_DIR}/host.Dockerfile" "$APPS_DIR" >/dev/null
done

for target in "$@"; do
  case "$target" in
    unreachable) run_unreachable ;;
    digest) run_digest ;;
    *) run_app "$target" ;;
  esac
done
