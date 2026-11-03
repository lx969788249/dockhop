//go:build integration

package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// 迁入的各种写法，迁过去都要照原样运行：Compose 服务有多个副本、同一个服务的副本有的运行有的
// 停着、项目目录本身挂进容器，跨项目共用网络、启动有先后（包括依赖猜错的时候），几个软链接指向
// 同一个挂载目录。迁入失败时还原、不安全时停下见 tx_test.go，配置逐字段一致见 fidelity_test.go。

// Compose 服务有多个副本时：按副本个数创建，每个副本的匿名卷和容器里的改动放进对应的新容器，
// 不能都放进同一个。
func TestInComposeReplicas(t *testing.T) {
	p := fmt.Sprintf("dmtest_rp%d", os.Getpid())
	project := p + "proj"
	t.Cleanup(func() { removeProject(project); removePrefixed(p) })
	sh(t, "docker", "pull", "-q", "alpine:3.20")
	proj := filepath.Join(t.TempDir(), "proj")
	os.MkdirAll(proj, 0o755)
	os.WriteFile(filepath.Join(proj, "compose.yml"), []byte(`services:
  worker:
    image: alpine:3.20
    command: ["sleep", "600"]
    labels: {com.centurylinklabs.watchtower.enable: "false"}
    volumes: ["/data"]
    deploy: {replicas: 2}
`), 0o644)
	c := composeCmd(t)
	up := exec.Command(c[0], append(c[1:], "-p", project, "up", "-d")...)
	up.Dir = proj
	if b, err := up.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	replicas := func() []string {
		t.Helper()
		out := sh(t, "docker", "ps", "-a", "--filter", "label=com.docker.compose.project="+project,
			"--format", `{{.Label "com.docker.compose.container-number"}} {{.Names}}`)
		lines := strings.Split(strings.TrimSpace(out), "\n")
		sort.Strings(lines)
		return lines
	}
	old := replicas()
	if len(old) != 2 {
		t.Skipf("Compose 没有按 deploy.replicas 建出 2 个副本：%v", old)
	}
	for _, l := range old {
		n, id, _ := strings.Cut(l, " ")
		exec1(t, id, "echo r"+n+" >/data/id && echo r"+n+" >/etc/id")
	}
	m := startOut(t, "--include", strings.Fields(old[0])[1])
	removeProject(project)

	home := t.TempDir()
	if _, code := m.in(t, home, nil); code != 0 {
		t.Fatalf("迁入应成功：%d", code)
	}
	got := replicas()
	if len(got) != 2 {
		t.Fatalf("应有 2 个副本：%v", got)
	}
	for _, l := range got {
		n, id, _ := strings.Cut(l, " ")
		if v := exec1(t, id, "cat /data/id /etc/id"); v != "r"+n+"\nr"+n {
			t.Errorf("副本 %s 的数据不对（匿名卷、容器里的改动）：%q", n, v)
		}
		if s := state(t, id); s != "running" {
			t.Errorf("副本 %s 应在运行：%s", n, s)
		}
	}
	// 迁入后自己 docker compose up，不应重建容器（容器上记的配置摘要和文件一致）。
	ids := func() string {
		return sh(t, "docker", "ps", "-aq", "--no-trunc", "--filter", "label=com.docker.compose.project="+project)
	}
	before := ids()
	again := exec.Command(c[0], append(c[1:], "-p", project, "up", "-d")...)
	again.Dir = proj
	if b, err := again.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	if after := ids(); after != before {
		t.Errorf("迁入后 docker compose up 重建了容器：\n%s\n→\n%s", before, after)
	}
	m.waitOutSuccess(t)
	noLeftovers(t, home)
}

// 同一个服务一个副本在运行、一个停着：迁入后照旧一个运行、一个停着（docker compose start
// 会把两个都启动，所以不能用它）；它依赖的服务照常先启动。
func TestInComposeMixedStates(t *testing.T) {
	p := fmt.Sprintf("dmtest_ms%d", os.Getpid())
	project := p + "proj"
	t.Cleanup(func() { removeProject(project); removePrefixed(p) })
	sh(t, "docker", "pull", "-q", "alpine:3.20")
	proj := filepath.Join(t.TempDir(), "proj")
	os.MkdirAll(proj, 0o755)
	os.WriteFile(filepath.Join(proj, "compose.yml"), []byte(`services:
  db:
    image: alpine:3.20
    command: ["sleep", "600"]
    labels: {com.centurylinklabs.watchtower.enable: "false"}
  worker:
    image: alpine:3.20
    command: ["sleep", "600"]
    labels: {com.centurylinklabs.watchtower.enable: "false"}
    depends_on: [db]
    deploy: {replicas: 2}
`), 0o644)
	c := composeCmd(t)
	up := exec.Command(c[0], append(c[1:], "-p", project, "up", "-d")...)
	up.Dir = proj
	if b, err := up.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	states := func() string {
		t.Helper()
		out := sh(t, "docker", "ps", "-a", "--filter", "label=com.docker.compose.project="+project,
			"--format", `{{.Label "com.docker.compose.service"}}-{{.Label "com.docker.compose.container-number"}} {{.State}}`)
		lines := strings.Split(strings.TrimSpace(out), "\n")
		sort.Strings(lines)
		return strings.Join(lines, ",")
	}
	if got := states(); got != "db-1 running,worker-1 running,worker-2 running" {
		t.Skipf("Compose 没有按 deploy.replicas 建出 2 个副本：%s", got)
	}
	sh(t, "docker", "stop", "-t", "1", containerByNumber(t, project, "worker", "2"))
	m := startOut(t, "--include", containerByNumber(t, project, "db", "1"))
	removeProject(project)

	home := t.TempDir()
	if _, code := m.in(t, home, nil); code != 0 {
		t.Fatalf("迁入应成功：%d", code)
	}
	// 停着的副本只创建、不启动（和停着的独立容器一样，状态是 created）。
	if got := states(); got != "db-1 running,worker-1 running,worker-2 created" && got != "db-1 running,worker-1 running,worker-2 exited" {
		t.Errorf("迁入后的状态：%s", got)
	}
	m.waitOutSuccess(t)
	noLeftovers(t, home)
}

// containerByNumber 是 Compose 项目里某个服务的第几个副本的容器名。
func containerByNumber(t *testing.T, project, service, number string) string {
	t.Helper()
	return sh(t, "docker", "ps", "-a", "--filter", "label=com.docker.compose.project="+project,
		"--filter", "label=com.docker.compose.service="+service, "--filter", "label=com.docker.compose.container-number="+number,
		"--format", "{{.Names}}")
}

// 好几个软链接指向同一个挂载目录（或者它里面的目录）：数据只迁一份，每个软链接都要在
// 新服务器上重建，用哪个软链接挂载的容器都能看到数据。
func TestInSymlinkAliases(t *testing.T) {
	p := fmt.Sprintf("dmtest_sl%d", os.Getpid())
	t.Cleanup(func() { removePrefixed(p) })
	sh(t, "docker", "pull", "-q", "alpine:3.20")
	root := t.TempDir()
	real := filepath.Join(root, "real")
	os.MkdirAll(filepath.Join(real, "sub"), 0o755)
	os.WriteFile(filepath.Join(real, "top.txt"), []byte("top"), 0o644)
	os.WriteFile(filepath.Join(real, "sub", "inner.txt"), []byte("inner"), 0o644)
	l1, l2, l3 := filepath.Join(root, "l1"), filepath.Join(root, "l2"), filepath.Join(root, "l3")
	os.Symlink(real, l1)
	os.Symlink(real, l2)
	os.Symlink(filepath.Join(real, "sub"), l3)
	a, b := p+"_a", p+"_b"
	sh(t, "docker", "run", "-d", "--name", a, "--label", noWatchtower, "-v", l1+":/x", "alpine:3.20", "sleep", "600")
	sh(t, "docker", "run", "-d", "--name", b, "--label", noWatchtower, "-v", l2+":/y", "-v", l3+":/z", "alpine:3.20", "sleep", "600")
	m := startOut(t, "--include", a+","+b)
	removeAll(a, b)
	os.RemoveAll(root)
	os.MkdirAll(root, 0o755)

	home := t.TempDir()
	if _, code := m.in(t, home, nil); code != 0 {
		t.Fatalf("迁入应成功：%d", code)
	}
	for link, target := range map[string]string{l1: real, l2: real, l3: filepath.Join(real, "sub")} {
		if got, err := os.Readlink(link); err != nil || got != target {
			t.Errorf("软链接 %s 应指向 %s：%q %v", link, target, got, err)
		}
	}
	if v := exec1(t, a, "cat /x/top.txt"); v != "top" {
		t.Errorf("a 看到的数据：%q", v)
	}
	if v := exec1(t, b, "cat /y/top.txt; echo; cat /z/inner.txt"); v != "top\ninner" {
		t.Errorf("b 看到的数据：%q", v)
	}
	m.waitOutSuccess(t)
	noLeftovers(t, home)
}

// 1Panel 的样子：应用和数据库是两个 Compose 项目，接在同一个外部网络上，应用的启动命令里写着
// 数据库的服务名。应用的项目名排在前面，按名字顺序先启动它的话，它连不上数据库、一直在重启，
// 健康检查过不了。要先启动数据库、等它就绪，再启动应用；日志里写明顺序和原因。
func TestInCrossProjectOrder(t *testing.T) {
	p := fmt.Sprintf("dmtest_xo%d", os.Getpid())
	app, db, shared := p+"app", p+"db", p+"net"
	t.Cleanup(func() {
		removeProject(app)
		removeProject(db)
		try("docker", "network", "rm", shared)
		removePrefixed(p)
	})
	sh(t, "docker", "pull", "-q", "alpine:3.20")
	sh(t, "docker", "network", "create", shared)
	root := t.TempDir()
	files := map[string]string{
		db: `services:
  database:
    image: alpine:3.20
    command: ["nc", "-lk", "-p", "5432", "-e", "echo"]
    labels: {com.centurylinklabs.watchtower.enable: "false"}
    networks: [shared]
networks:
  shared: {name: ` + shared + `, external: true}
`,
		app: `services:
  web:
    image: alpine:3.20
    restart: always
    labels: {com.centurylinklabs.watchtower.enable: "false"}
    networks: [shared]
    entrypoint: ["sh", "-c", "for i in 1 2 3; do echo | nc -w 2 database 5432 >/dev/null && touch /tmp/ready && exec sleep 600; sleep 1; done; echo cannot reach database; exit 1", "--"]
    command: ["--db-url=tcp://database:5432/app"]
    healthcheck:
      test: ["CMD", "test", "-f", "/tmp/ready"]
      interval: 2s
      retries: 3
      start_period: 1s
networks:
  shared: {name: ` + shared + `, external: true}
`,
	}
	c := composeCmd(t)
	for _, proj := range []string{db, app} {
		dir := filepath.Join(root, proj)
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(files[proj]), 0o644)
		up := exec.Command(c[0], append(c[1:], "-p", proj, "up", "-d")...)
		up.Dir = dir
		if b, err := up.CombinedOutput(); err != nil {
			t.Fatalf("%v\n%s", err, b)
		}
	}
	web, database := containerByLabel(t, app, "web"), containerByLabel(t, db, "database")
	healthy := func() bool { return try("docker", "inspect", "-f", "{{.State.Health.Status}}", web) == "healthy" }
	for i := 0; i < 60 && !healthy(); i++ {
		time.Sleep(time.Second)
	}
	if !healthy() {
		t.Fatalf("旧服务器上的应用没有就绪：\n%s", try("docker", "logs", web))
	}
	m := startOut(t, "--include", web+","+database)
	removeProject(app)
	removeProject(db)
	sh(t, "docker", "network", "rm", shared)

	home := t.TempDir()
	if _, code := m.in(t, home, nil); code != 0 {
		t.Fatalf("迁入应成功：%d", code)
	}
	if !healthy() {
		t.Errorf("迁入后应用应健康：%s", try("docker", "inspect", "-f", "{{.State.Status}} {{.State.Health.Status}}", web))
	}
	logs, _ := filepath.Glob(filepath.Join(home, "logs", "in-*.log"))
	if len(logs) == 0 {
		t.Fatal("没有迁入日志")
	}
	b, _ := os.ReadFile(logs[len(logs)-1])
	mustContain(t, string(b), "启动顺序：项目 "+db+"、项目 "+app,
		"项目 "+app+" 用到项目 "+db+"，最好等它先就绪："+web+" 的启动命令里写着 database",
		"容器 "+web+" 就绪：健康检查通过")
	m.waitOutSuccess(t)
	noLeftovers(t, home)
}

// upProject 在 root/name 下写好 compose.yml（和 files 里的其他文件），用 Compose 启动。
func upProject(t *testing.T, root, name, compose string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(compose), 0o644)
	for f, content := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o755)
		os.WriteFile(filepath.Join(dir, f), []byte(content), 0o644)
	}
	c := composeCmd(t)
	up := exec.Command(c[0], append(c[1:], "-p", name, "up", "-d")...)
	up.Dir = dir
	if b, err := up.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	return dir
}

// 服务把项目目录本身挂进容器（./:/app）：项目文件随这个挂载目录迁移，迁入前检查要用单独打包的
// 配置文件（含 env_file）检查，不能因为临时目录里没有文件就说新服务器读不懂配置。
func TestInProjectInsideMount(t *testing.T) {
	p := fmt.Sprintf("dmtest_pm%d", os.Getpid())
	t.Cleanup(func() { removeProject(p); removePrefixed(p) })
	sh(t, "docker", "pull", "-q", "alpine:3.20")
	dir := upProject(t, t.TempDir(), p, `services:
  web:
    image: alpine:3.20
    command: ["sleep", "600"]
    labels: {com.centurylinklabs.watchtower.enable: "false"}
    env_file: [conf/app.env]
    volumes: ["./:/app"]
`, map[string]string{"conf/app.env": "GREETING=hello\n", "site/index.html": "migrated-site\n"})
	web := containerByLabel(t, p, "web")
	m := startOut(t, "--include", web)
	removeProject(p)
	os.RemoveAll(dir)

	home := t.TempDir()
	if _, code := m.in(t, home, nil); code != 0 {
		t.Fatalf("迁入应成功：%d", code)
	}
	if got := exec1(t, containerByLabel(t, p, "web"), "cat /app/site/index.html; echo $GREETING"); got != "migrated-site\nhello" {
		t.Errorf("项目文件和 env_file 应迁过来：%q", got)
	}
	m.waitOutSuccess(t)
	noLeftovers(t, home)
	onlyLogs(t, home)
	onlyLogs(t, m.home)
}

// 项目 aapp 把项目 zproxy 建的网络当外部网络用（反向代理常见写法），而 aapp 的名字排在前面：
// 要先创建 zproxy（Compose 按它的文件建网络），aapp 才找得到网络。
func TestInSharedComposeNetwork(t *testing.T) {
	p := fmt.Sprintf("dmtest_sn%d", os.Getpid())
	app, proxy, network := p+"aapp", p+"zproxy", p+"_proxynet"
	t.Cleanup(func() {
		removeProject(app)
		removeProject(proxy)
		try("docker", "network", "rm", network)
		removePrefixed(p)
	})
	sh(t, "docker", "pull", "-q", "alpine:3.20")
	root := t.TempDir()
	upProject(t, root, proxy, `services:
  proxy:
    image: alpine:3.20
    command: ["sleep", "600"]
    labels: {com.centurylinklabs.watchtower.enable: "false"}
    networks: [proxy]
networks:
  proxy: {name: `+network+`}
`, nil)
	upProject(t, root, app, `services:
  app:
    image: alpine:3.20
    command: ["sleep", "600"]
    labels: {com.centurylinklabs.watchtower.enable: "false"}
    networks: [proxy]
networks:
  proxy: {name: `+network+`, external: true}
`, nil)
	m := startOut(t, "--include", containerByLabel(t, app, "app")+","+containerByLabel(t, proxy, "proxy"))
	removeProject(app)
	removeProject(proxy)
	try("docker", "network", "rm", network)

	home := t.TempDir()
	if _, code := m.in(t, home, nil); code != 0 {
		t.Fatalf("迁入应成功：%d", code)
	}
	for _, c := range []string{containerByLabel(t, app, "app"), containerByLabel(t, proxy, "proxy")} {
		if s := state(t, c); s != "running" {
			t.Errorf("%s 应在运行：%s", c, s)
		}
	}
	m.waitOutSuccess(t)
	noLeftovers(t, home)
}

// 从配置里猜依赖可能猜错：数据库的环境变量里碰巧写着网站容器的名字，看起来像数据库要用网站；
// 网站其实是按固定 IP 连数据库（认不出来），数据库没起来时它的健康检查过不了。猜出来的依赖
// 等不到时不能拦着：接着启动数据库，网站最后再等一次，迁入照样成功。
func TestInWrongGuessDoesNotBlock(t *testing.T) {
	p := fmt.Sprintf("dmtest_wg%d", os.Getpid())
	web, db, network := p+"web", p+"db", p+"net"
	octet := 20 + os.Getpid()%200
	dbIP := fmt.Sprintf("172.31.%d.20", octet)
	t.Cleanup(func() { removeAll(web, db); try("docker", "network", "rm", network); removePrefixed(p) })
	sh(t, "docker", "pull", "-q", "alpine:3.20")
	sh(t, "docker", "network", "create", "--subnet", fmt.Sprintf("172.31.%d.0/24", octet), network)
	sh(t, "docker", "run", "-d", "--name", db, "--label", noWatchtower, "--network", network, "--ip", dbIP,
		"--restart", "unless-stopped", "-e", "APP_NAME="+web, "alpine:3.20", "nc", "-lk", "-p", "5432", "-e", "echo")
	sh(t, "docker", "run", "-d", "--name", web, "--label", noWatchtower, "--network", network, "--restart", "unless-stopped",
		"--health-cmd", "echo | nc -w 1 "+dbIP+" 5432", "--health-interval", "1s", "--health-timeout", "1s", "--health-retries", "3",
		"alpine:3.20", "sleep", "600")
	m := startOut(t, "--include", web+","+db)
	removeAll(web, db)
	sh(t, "docker", "network", "rm", network)

	home := t.TempDir()
	if _, code := m.in(t, home, nil); code != 0 {
		t.Fatalf("猜错的依赖不应让迁入失败：%d", code)
	}
	logs, _ := filepath.Glob(filepath.Join(home, "logs", "in-*.log"))
	if len(logs) == 0 {
		t.Fatal("没有迁入日志")
	}
	b, _ := os.ReadFile(logs[len(logs)-1])
	mustContain(t, string(b), "容器 "+db+" 用到容器 "+web+"，最好等它先就绪：", "可能认错了，先接着启动容器 "+db)
	m.waitOutSuccess(t)
	noLeftovers(t, home)
}
