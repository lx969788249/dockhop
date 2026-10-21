printf 'tricky-bind-migrated\n' >data/bind.txt
docker exec "$(service_container dm_tricky api)" sh -c 'echo api-state-migrated >/srv/state/seed.txt'
docker exec "$(service_container dm_tricky worker)" sh -c 'echo anon-volume-migrated >/srv/anon/seed.txt'
docker exec dm_standalone sh -c 'echo standalone-migrated >/data/seed.txt'
# 容器里没挂载出来的改动：新增和删除的文件（独立容器）、新增的目录（Compose 服务）。
docker exec dm_standalone sh -c 'echo layer-added >/etc/dockhop-added && rm /etc/motd'
docker exec "$(service_container dm_tricky worker)" sh -c 'mkdir -p /opt/worker && echo worker-layer >/opt/worker/note.txt'
wait_http http://localhost:8084/
