# 一台模拟服务器：自带独立 Docker 的容器。DockHop 本身不需要任何命令，这些只给测试脚本用。
ARG DIND_VERSION=27.5
FROM docker:${DIND_VERSION}-dind
RUN apk add --no-cache bash coreutils curl iproute2 iptables jq procps && \
  { docker compose version >/dev/null 2>&1 || apk add --no-cache docker-cli-compose; }
# 只用本机 unix socket，不需要 TLS。
ENV DOCKER_TLS_CERTDIR=
