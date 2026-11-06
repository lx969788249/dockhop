-- dockhop.run 计数用的 D1 数据库：一张表、一行。n 是成功完成迁移的次数，failed 是迁移失败的次数。
-- 成功率：SELECT n AS 成功, failed AS 失败, ROUND(100.0 * n / (n + failed), 1) AS 成功率 FROM counter;
-- 建库：npx wrangler@4 d1 create dockhop --location apac（把输出的 database_id 写进 wrangler.toml）
-- 建表：npx wrangler@4 d1 execute dockhop --remote --file schema.sql
CREATE TABLE counter (n INTEGER NOT NULL, failed INTEGER NOT NULL DEFAULT 0);
INSERT INTO counter (n) VALUES (0);
