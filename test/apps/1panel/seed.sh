# Halo 第一次启动时在数据库里建好了表；再往 Halo 的数据库和数据目录里各写一点能认出来的东西。
wait_http http://localhost:8090/actuator/health/readiness
docker exec 1Panel-postgresql-dm01 psql -U halo -d halo -v ON_ERROR_STOP=1 \
  -c "CREATE TABLE dm_marker(v text)" -c "INSERT INTO dm_marker VALUES ('migrated-from-1panel')" >/dev/null
echo "halo data" >/opt/1panel/apps/halo/halo/data/dm-marker.txt
