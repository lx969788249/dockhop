# 按 1Panel 的样子安装：1Panel 建好 1panel-network，每个应用是 /opt/1panel/apps/<应用>/<名字>
# 下的一个 Compose 项目，项目名就是安装时起的名字。先装 PostgreSQL、建好 Halo 用的数据库，
# 再装 Halo；Halo 按服务名 postgresql 连数据库（.env 里的 PANEL_DB_HOST）。
docker network create 1panel-network >/dev/null
mkdir -p /opt/1panel/apps/postgresql /opt/1panel/apps/halo
cp -a postgresql /opt/1panel/apps/postgresql/postgresql
cp -a halo /opt/1panel/apps/halo/halo
cd /opt/1panel/apps/postgresql/postgresql
docker compose -p postgresql up -d --wait --wait-timeout 300
docker exec 1Panel-postgresql-dm01 psql -U user_dm01 -d postgres -v ON_ERROR_STOP=1 \
  -c "CREATE USER halo WITH PASSWORD 'halo-db-secret'" -c "CREATE DATABASE halo OWNER halo" >/dev/null
cd /opt/1panel/apps/halo/halo
docker compose -p halo up -d --wait --wait-timeout 600
