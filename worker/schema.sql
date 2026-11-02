-- dockhop.run 计数用的 D1 数据库：一张表、一行、一个数字（成功完成迁移的次数）。
-- 建库：npx wrangler@4 d1 create dockhop --location apac（把输出的 database_id 写进 wrangler.toml）
-- 建表：npx wrangler@4 d1 execute dockhop --remote --file schema.sql
CREATE TABLE counter (n INTEGER NOT NULL);
INSERT INTO counter VALUES (0);
