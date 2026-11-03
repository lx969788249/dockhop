wait_http http://localhost:8090/actuator/health/readiness
expect_contains "Halo 已就绪，数据库里的记录还在" \
  "$(docker exec 1Panel-postgresql-dm01 psql -U halo -d halo -tAc 'SELECT v FROM dm_marker')" "migrated-from-1panel"
expect_contains "Halo 数据目录里的文件还在" "$(cat /opt/1panel/apps/halo/halo/data/dm-marker.txt)" "halo data"
log="$(find /root/.dockhop/logs -name 'in-*.log' | sort | tail -n1)"
expect_contains "先启动数据库，等它就绪后再启动 Halo" "$(cat "$log")" \
  "项目 halo 用到项目 postgresql，最好等它先就绪：1Panel-halo-dm01 的启动命令里写着 postgresql"
