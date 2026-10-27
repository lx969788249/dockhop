//go:build integration

package integration

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/crypto"
)

// 事务和安全场景（设计文档附录 C）：新服务器上已经有同名的服务、数据时，迁入失败或
// 被中断都要删除迁入的内容、让新服务器回到迁入前的样子；不安全的情况在改动任何东西之前停下。

// txFixture：旧服务器上一个独立容器 web（固定 IP、命名卷、挂载目录）。
type txFixture struct {
	p, web, vol, net, dir, ip string
}

func newTx(t *testing.T, tag string) *txFixture {
	t.Helper()
	p := fmt.Sprintf("dmtest_%s%d", tag, os.Getpid())
	octet := 20 + (os.Getpid()+len(tag)*7)%200
	f := &txFixture{p: p, web: p + "_web", vol: p + "_vol", net: p + "_net", dir: filepath.Join(t.TempDir(), "data"),
		ip: fmt.Sprintf("172.30.%d.10", octet)}
	t.Cleanup(func() { removePrefixed(p) })
	sh(t, "docker", "pull", "-q", "alpine:3.20")
	sh(t, "docker", "network", "create", "--subnet", fmt.Sprintf("172.30.%d.0/24", octet), f.net)
	sh(t, "docker", "volume", "create", f.vol)
	sh(t, "docker", "run", "--rm", "-v", f.vol+":/v", "alpine:3.20", "sh", "-c", "echo src-vol >/v/src.txt")
	os.MkdirAll(f.dir, 0o755)
	os.WriteFile(filepath.Join(f.dir, "src.txt"), []byte("src-dir"), 0o644)
	return f
}

// runWeb 启动 web；extra 是额外的 docker run 参数，cmd 是命令。
func (f *txFixture) runWeb(t *testing.T, extra []string, cmd ...string) {
	t.Helper()
	args := append([]string{"run", "-d", "--name", f.web, "--label", noWatchtower, "--network", f.net, "--ip", f.ip,
		"-v", f.vol + ":/data", "-v", f.dir + ":/bind"}, extra...)
	if len(cmd) == 0 {
		cmd = []string{"sleep", "600"}
	}
	sh(t, "docker", append(append(args, "alpine:3.20"), cmd...)...)
}

// becomeTarget 把这台机器变成“已经有同名服务和数据的新服务器”：web 换成另一个容器
// （带 old=1 标签，占着同一个固定 IP），卷和目录换成新服务器自己的数据。
func (f *txFixture) becomeTarget(t *testing.T) {
	t.Helper()
	removeAll(f.web)
	sh(t, "docker", "run", "--rm", "-v", f.vol+":/v", "alpine:3.20", "sh", "-c", "rm -rf /v/* && echo target-vol >/v/target.txt")
	os.RemoveAll(f.dir)
	os.MkdirAll(f.dir, 0o755)
	os.WriteFile(filepath.Join(f.dir, "target.txt"), []byte("target-dir"), 0o644)
	sh(t, "docker", "run", "-d", "--name", f.web, "--label", noWatchtower, "--label", "old=1", "--network", f.net, "--ip", f.ip,
		"-v", f.vol+":/data", "-v", f.dir+":/bind", "alpine:3.20", "sleep", "600")
}

// targetIntact 确认新服务器还是迁入前的样子。
func (f *txFixture) targetIntact(t *testing.T, oldID string) {
	t.Helper()
	c := inspect(t, f.web)
	if c["Id"] != oldID {
		t.Errorf("web 应是原来的容器")
	}
	if s := state(t, f.web); s != "running" {
		t.Errorf("原来的 web 应恢复运行：%s", s)
	}
	nets := c["NetworkSettings"].(map[string]any)["Networks"].(map[string]any)
	if ep, ok := nets[f.net].(map[string]any); !ok || ep["IPAddress"] != f.ip {
		t.Errorf("原来的 web 应重新连上网络、拿回固定 IP：%v", nets)
	}
	if got := volumeFiles(t, f.vol); !strings.Contains(got, "./target.txt") || strings.Contains(got, "src.txt") {
		t.Errorf("卷应还原成新服务器原来的数据：\n%s", got)
	}
	if got := dirFiles(t, f.dir); !strings.Contains(got, "target.txt") || strings.Contains(got, "src.txt") {
		t.Errorf("目录应还原成新服务器原来的数据：\n%s", got)
	}
	if b := try("docker", "ps", "-a", "--filter", "name=dockhop-backup", "--format", "{{.Names}}"); b != "" {
		t.Errorf("不应留下改名备份的容器：%s", b)
	}
}

// 已有同名容器、卷、目录时：各种失败都整体还原；最后正常迁入，替换得干干净净。
func TestInReplaceAndRollback(t *testing.T) {
	f := newTx(t, "rb")
	f.runWeb(t, nil)
	m := startOut(t, "--include", f.web)
	f.becomeTarget(t)
	oldID := inspect(t, f.web)["Id"].(string)
	home := t.TempDir()

	// 附录 C-16：--existing fail 在停止服务、覆盖数据之前就停下。
	out, code := m.in(t, home, nil, "--existing", "fail")
	if code == 0 {
		t.Fatal("--existing fail 时应失败")
	}
	mustContain(t, out, "--existing fail", "还没有下载或改动任何东西")
	f.targetIntact(t, oldID)

	// 附录 C-4：同一台机器同时只允许一个迁入，第二个在改动任何东西之前退出。
	lockPath := filepath.Join(home, "locks", "in.lock")
	os.MkdirAll(filepath.Dir(lockPath), 0o700)
	lf, _ := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	unix.Flock(int(lf.Fd()), unix.LOCK_EX)
	out, code = m.in(t, home, nil)
	unix.Flock(int(lf.Fd()), unix.LOCK_UN)
	lf.Close()
	if code == 0 || !strings.Contains(out, "另一个 DockHop 任务") {
		t.Fatalf("有迁入在运行时应直接退出：%d", code)
	}
	f.targetIntact(t, oldID)

	// 附录 C-2、C-6：新容器创建并验证之后失败——旧容器（含网络和固定 IP）、卷、目录一起还原。
	out, code = m.in(t, home, []string{"DOCKHOP_TEST_FAIL=container:" + f.web})
	if code == 0 {
		t.Fatal("模拟失败时迁入应失败")
	}
	mustContain(t, out, "❌ 迁入失败，迁入的内容已全部删除", "原来的服务和数据都在", "重新运行时不用重新下载")
	f.targetIntact(t, oldID)
	noLeftovers(t, home)

	// 附录 C-12：数据写入失败时准确报告并还原。
	out, code = m.in(t, home, []string{"DOCKHOP_TEST_FAIL=volume:" + f.vol})
	if code == 0 {
		t.Fatal("模拟写入失败时迁入应失败")
	}
	mustContain(t, out, "数据卷 "+f.vol+" 写入失败", "迁入的内容已全部删除")
	if strings.Contains(out, "下载迁移包 ✓") && !strings.Contains(out, "0 秒") {
		t.Log("（重试时应直接用上次下载的文件）")
	}
	f.targetIntact(t, oldID)

	// 迁入到一半按 Ctrl+C：还原后退出。
	p := start(t, []string{"DOCKHOP_HOME=" + home, "DOCKHOP_TEST_PAUSE=after-data"}, "in", "--yes", m.link)
	waitFile(t, filepath.Join(home, "test-paused-after-data"), 10*time.Minute)
	if got := dirFiles(t, f.dir); !strings.Contains(got, "src.txt") {
		t.Fatalf("停住的时候目录应已换成迁移的数据：\n%s", got)
	}
	_ = p.cmd.Process.Signal(os.Interrupt)
	p.wait(t, 5*time.Minute)
	mustContain(t, p.out(), "⚠️ 迁入已中断，迁入的内容已全部删除")
	f.targetIntact(t, oldID)

	// 迁入到一半被 kill -9：下次运行时先还原，再正常迁入。
	os.Remove(filepath.Join(home, "test-paused-after-data"))
	p = start(t, []string{"DOCKHOP_HOME=" + home, "DOCKHOP_TEST_PAUSE=after-data"}, "in", "--yes", m.link)
	waitFile(t, filepath.Join(home, "test-paused-after-data"), 10*time.Minute)
	_ = p.cmd.Process.Kill()
	p.wait(t, time.Minute)
	if _, err := os.Stat(filepath.Join(home, "rollback", "current", "journal.jsonl")); err != nil {
		t.Fatalf("被强制结束后事务日志应留在磁盘上：%v", err)
	}
	if b := try("docker", "ps", "-a", "--filter", "name=dockhop-backup", "--format", "{{.Names}}"); b == "" {
		t.Fatal("被强制结束时原来的 web 应正改名备份着（用来验证下次运行会还原）")
	}

	// 附录 C-9：正常迁入（先还原上次留下的改动）——旧的数据不能残留。
	out, code = m.in(t, home, nil)
	if code != 0 {
		t.Fatalf("应成功，退出码 %d", code)
	}
	mustContain(t, out, "上次迁入", "已删除上次迁入的内容", "✅ 迁入完成")
	c := inspect(t, f.web)
	if c["Id"] == oldID || c["Config"].(map[string]any)["Labels"].(map[string]any)["old"] != nil {
		t.Error("web 应换成迁移过来的容器")
	}
	if ep := c["NetworkSettings"].(map[string]any)["Networks"].(map[string]any)[f.net].(map[string]any); ep["IPAddress"] != f.ip {
		t.Errorf("新的 web 应拿到固定 IP（旧容器释放了它）：%v", ep["IPAddress"])
	}
	if got := volumeFiles(t, f.vol); strings.Contains(got, "target") || !strings.Contains(got, "src.txt") {
		t.Errorf("卷里不应残留新服务器原来的文件：\n%s", got)
	}
	if got := dirFiles(t, f.dir); strings.Contains(got, "target") || !strings.Contains(got, "src.txt") {
		t.Errorf("目录里不应残留新服务器原来的文件：\n%s", got)
	}
	if exists("container", oldID) {
		t.Error("被替换的旧容器应在提交后删除")
	}
	m.waitOutSuccess(t)
	noLeftovers(t, home)
}

// 不安全或无法自动处理的情况在改动之前停下；回滚失败、提交后清理失败都如实报告。
func TestInSafetyStops(t *testing.T) {
	f := newTx(t, "sf")
	f.runWeb(t, nil)
	m := startOut(t, "--include", f.web)
	removeAll(f.web)
	home := t.TempDir()

	// 附录 C-5：挂载目录盖住 DockHop 自己的目录时，在改动数据之前停下。
	inside := filepath.Join(f.dir, "dockhop-home")
	out, code := m.in(t, inside, nil)
	if code == 0 || !strings.Contains(out, "重叠") || !strings.Contains(out, "DOCKHOP_HOME") {
		t.Fatalf("挂载目录和 DockHop 的目录重叠时应停下：%d", code)
	}
	if exists("container", f.web) {
		t.Error("停下时不应创建任何容器")
	}
	// 经过软链接的上级目录指进挂载目录，也算重叠。
	via := filepath.Join(t.TempDir(), "via")
	os.Symlink(f.dir, via)
	out, code = m.in(t, filepath.Join(via, "home"), nil)
	if code == 0 || !strings.Contains(out, "重叠") {
		t.Fatalf("经过软链接指进挂载目录时也应停下：%d", code)
	}

	// 附录 C-13：同名卷存储方式不同，在清空之前就停下（--yes 也不能自动处理）。
	sh(t, "docker", "volume", "rm", f.vol)
	sh(t, "docker", "volume", "create", "--driver", "local", "--opt", "type=tmpfs", "--opt", "device=tmpfs", f.vol)
	sh(t, "docker", "run", "--rm", "-v", f.vol+":/v", "alpine:3.20", "sh", "-c", "echo keep >/v/keep.txt")
	out, code = m.in(t, home, nil)
	if code == 0 || !strings.Contains(out, "存储方式和旧服务器不同") {
		t.Fatalf("同名卷存储方式不同时应停下：%d", code)
	}
	if v := sh(t, "docker", "volume", "inspect", "-f", "{{.Options.type}}", f.vol); v != "tmpfs" {
		t.Errorf("卷不应被改动：%s", v)
	}
	sh(t, "docker", "volume", "rm", f.vol)

	// 附录 C-3：同名网络核心设置相同、只是标签不同时，提示并沿用。
	sh(t, "docker", "network", "rm", f.net)
	subnet := f.ip[:strings.LastIndex(f.ip, ".")] + ".0/24"
	sh(t, "docker", "network", "create", "--subnet", subnet, "--label", "owner=target", f.net)
	netID := sh(t, "docker", "network", "inspect", "-f", "{{.Id}}", f.net)

	// 附录 C-17：回滚本身没有完全成功时，如实报告“没有完全成功”，回滚资料保留。
	out, code = m.in(t, home, []string{"DOCKHOP_TEST_FAIL=container:" + f.web + ",rollback-data"})
	if code == 0 {
		t.Fatal("应失败")
	}
	mustContain(t, out, "迁入的内容没能全部删除", "回滚资料：")
	if strings.Contains(out, "已全部删除") {
		t.Error("回滚没有完全成功时不能说已全部删除")
	}
	if _, err := os.Stat(filepath.Join(home, "rollback", "current", "journal.jsonl")); err != nil {
		t.Errorf("回滚资料应保留：%v", err)
	}

	// 附录 C-18：新服务已验证、但提交后清理失败——不能报成功，也不能回滚已提交的数据。
	// （这次运行开头会先把上次没还原完的改动还原掉。）
	out, code = m.in(t, home, []string{"DOCKHOP_TEST_FAIL=commit-cleanup"})
	if code == 0 {
		t.Fatal("清理失败时不应报告成功")
	}
	mustContain(t, out, "上次迁入", "只是标签和旧服务器不同", "服务和数据都已迁入，但清理没有完成")
	if strings.Contains(out, "正在删除这次迁入的内容") {
		t.Error("已经提交的迁入不能回滚")
	}
	if s := state(t, f.web); s != "running" {
		t.Errorf("已经提交的新服务应在运行：%s", s)
	}
	if id := sh(t, "docker", "network", "inspect", "-f", "{{.Id}}", f.net); id != netID {
		t.Error("同名网络应沿用，不应重建")
	}
	m.waitOutSuccess(t) // 服务已经迁入，旧服务器照常结束
}

// 附录 C-8：没有健康检查的容器启动后马上退出，判为失败并保留旧容器。
// 附录 C-15：Compose 项目的新配置不健康时，自动还原新服务器上原来的项目。
func TestInStartFailureKeepsOld(t *testing.T) {
	f := newTx(t, "sv")
	// /run 下是系统路径：只重建挂载，不迁移内容。用里面的 ok 文件控制服务能不能正常启动。
	webFlag, appFlag := filepath.Join("/run", f.p, "web"), filepath.Join("/run", f.p, "app")
	for _, d := range []string{webFlag, appFlag} {
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "ok"), nil, 0o644)
	}
	t.Cleanup(func() { os.RemoveAll(filepath.Join("/run", f.p)) })
	f.runWeb(t, []string{"-v", webFlag + ":/flag"}, "sh", "-c", "test -e /flag/ok && exec sleep 600")
	proj := filepath.Join(t.TempDir(), "proj")
	os.MkdirAll(proj, 0o755)
	writeCompose := func(env string) {
		os.WriteFile(filepath.Join(proj, "compose.yml"), []byte(`services:
  app:
    image: alpine:3.20
    command: ["sleep", "600"]
    environment: {VERSION: "`+env+`"}
    labels: {com.centurylinklabs.watchtower.enable: "false"}
    volumes: ["`+appFlag+`:/flag"]
    healthcheck: {test: ["CMD", "test", "-e", "/flag/ok"], interval: 1s, retries: 2}
`), 0o644)
	}
	writeCompose("new")
	project := f.p + "proj"
	t.Cleanup(func() { removeProject(project) })
	c := composeCmd(t)
	compose := func(args ...string) {
		cmd := exec.Command(c[0], append(c[1:], append([]string{"-p", project}, args...)...)...)
		cmd.Dir = proj
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v\n%s", err, b)
		}
	}
	compose("up", "-d")
	app := containerByLabel(t, project, "app")
	m := startOut(t, "--include", f.web+","+app)

	// 新服务器上：已有 web（旧的）和同名项目（旧配置 VERSION=old）。
	f.becomeTarget(t)
	oldWeb := inspect(t, f.web)["Id"].(string)
	removeProject(project)
	writeCompose("old")
	compose("up", "-d")
	oldApp := containerByLabel(t, project, "app")
	oldEnv := sh(t, "docker", "inspect", "-f", "{{json .Config.Env}}", oldApp)
	home := t.TempDir()
	checkOld := func() {
		t.Helper()
		app := containerByLabel(t, project, "app")
		if app != oldApp {
			t.Errorf("旧项目的容器名：%s", app)
		}
		if env := sh(t, "docker", "inspect", "-f", "{{json .Config.Env}}", app); env != oldEnv {
			t.Errorf("旧项目应原样还原：%s", env)
		}
		if s := state(t, app); s != "running" {
			t.Errorf("旧项目应恢复运行：%s", s)
		}
		if b, _ := os.ReadFile(filepath.Join(proj, "compose.yml")); !strings.Contains(string(b), `"old"`) {
			t.Errorf("项目文件应还原：%s", b)
		}
		if inspect(t, f.web)["Id"] != oldWeb || state(t, f.web) != "running" {
			t.Error("旧的 web 应保留并恢复运行")
		}
		noLeftovers(t, home)
	}

	// 附录 C-15：Compose 项目的新配置健康检查不通过——还原原来的项目（原样重建）。
	os.Remove(filepath.Join(appFlag, "ok"))
	out, code := m.in(t, home, nil)
	if code == 0 {
		t.Fatal("应失败")
	}
	mustContain(t, out, "健康检查没有通过", "迁入的内容已全部删除")
	checkOld()

	// 附录 C-8：没有健康检查的 web 启动后马上退出——判为失败，旧容器保留。
	os.WriteFile(filepath.Join(appFlag, "ok"), nil, 0o644)
	os.Remove(filepath.Join(webFlag, "ok"))
	out, code = m.in(t, home, nil)
	if code == 0 {
		t.Fatal("应失败")
	}
	mustContain(t, out, "启动后马上退出", "迁入的内容已全部删除")
	checkOld()
}

// 附录 C-14：迁移包里有穿过软链接写到目标目录以外的成员时，在改动任何数据之前拒绝。
func TestInRejectsUnsafeBundle(t *testing.T) {
	p := fmt.Sprintf("dmtest_un%d", os.Getpid())
	t.Cleanup(func() { removePrefixed(p) })
	sh(t, "docker", "pull", "-q", "alpine:3.20")
	sh(t, "docker", "create", "--name", p+"_web", "--label", noWatchtower, "-v", p+"_vol:/data", "alpine:3.20", "sleep", "600")
	raw := sh(t, "docker", "inspect", p+"_web")
	removeAll(p + "_web")
	try("docker", "volume", "rm", p+"_vol")

	link, _ := crypto.NewLink("127.0.0.1", 9)
	path := filepath.Join(t.TempDir(), "bundle")
	out, _ := os.Create(path)
	w, _ := bundle.NewWriter(out, link)
	var list []json.RawMessage
	json.Unmarshal([]byte(raw), &list)
	w.AddJSON("containers/"+p+"_web.json", list[0])
	w.AddJSON("volumes/"+p+"_vol.json", map[string]any{"Name": p + "_vol", "Driver": "local"})
	// 卷里先放一个指向 /etc 的软链接，再通过它写 /etc/dmtest-evil。
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	tw.WriteHeader(&tar.Header{Name: "dockhop-data/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: "dockhop-data/etc", Typeflag: tar.TypeSymlink, Linkname: "/etc"})
	tw.WriteHeader(&tar.Header{Name: "dockhop-data/etc/dmtest-evil", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4})
	tw.Write([]byte("evil"))
	tw.Close()
	parts, n, _ := w.AddStream("volumes/"+p+"_vol", &tb)
	img, _ := exec.Command("docker", "save", "alpine:3.20").Output()
	iparts, in, _ := w.AddStream("images", bytes.NewReader(img))
	m := bundle.Manifest{Format: bundle.Format, Version: bundle.Version, Source: bundle.Source{Arch: "x86_64"},
		Containers: []bundle.Container{{Name: p + "_web", Image: "alpine:3.20", State: "created"}},
		Volumes:    []bundle.Volume{{Name: p + "_vol", Driver: "local", Bytes: n, Parts: parts}},
		Images:     bundle.Images{Refs: map[string]string{"alpine:3.20": ""}, Bytes: in, Parts: iparts}}
	w.AddJSON("manifest.json", m)
	w.Close()
	out.Close()

	home := t.TempDir()
	got, code := runIn(t, home, nil, "in", "--yes", "--file", path, link.String())
	if code == 0 || !strings.Contains(got, "已拒绝迁入") {
		t.Fatalf("应拒绝不安全的迁移包：%d", code)
	}
	if exists("volume", p+"_vol") || exists("container", p+"_web") {
		t.Error("拒绝时不应创建任何东西")
	}
	if _, err := os.Stat("/etc/dmtest-evil"); err == nil {
		os.Remove("/etc/dmtest-evil")
		t.Fatal("迁移包写到了目标目录以外")
	}
	if left, _ := filepath.Glob(filepath.Join(home, "work", "*", "data")); len(left) > 0 {
		t.Errorf("失败时应删除解密后的数据：%v", left)
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("用 --file 给的迁移包不能删")
	}
}

// 一次迁入两个互不相干的服务、其中一个起不来：默认全部删除；--keep-succeeded 时只删除
// 起不来的、保留正常运行的；处理好之后用同一个链接重新迁入，跳过已经迁入的、只迁入剩下的。
func TestInPartial(t *testing.T) {
	p := fmt.Sprintf("dmtest_pa%d", os.Getpid())
	flag := filepath.Join("/run", p) // 系统路径：只重建挂载、不迁移内容，用来控制 bad 能不能启动
	t.Cleanup(func() { removePrefixed(p); os.RemoveAll(flag) })
	sh(t, "docker", "pull", "-q", "alpine:3.20")
	good, bad := p+"_good", p+"_bad"
	os.MkdirAll(flag, 0o755)
	os.WriteFile(filepath.Join(flag, "ok"), nil, 0o644)
	for _, n := range []string{good, bad} {
		sh(t, "docker", "volume", "create", n)
		sh(t, "docker", "run", "--rm", "-v", n+":/v", "alpine:3.20", "sh", "-c", "echo "+n+" >/v/data.txt")
	}
	sh(t, "docker", "run", "-d", "--name", good, "--label", noWatchtower, "-v", good+":/data", "alpine:3.20", "sleep", "600")
	// 用户自己的容器，名字碰巧以 dockhop-helper- 开头（没有 DockHop 的标签）：迁出、迁入时都不能删。
	decoy := "dockhop-helper-" + p
	sh(t, "docker", "run", "-d", "--name", decoy, "--label", noWatchtower, "-v", "/keep", "alpine:3.20", "sleep", "600")
	decoyID := inspect(t, decoy)["Id"]
	sh(t, "docker", "run", "-d", "--name", bad, "--label", noWatchtower, "-v", bad+":/data", "-v", flag+":/flag",
		"alpine:3.20", "sh", "-c", "test -e /flag/ok && exec sleep 600")
	m := startOut(t, "--include", good+","+bad)

	// 新服务器上什么都没有，bad 在新服务器上起不来。
	removeAll(good, bad)
	try("docker", "volume", "rm", good, bad)
	os.Remove(filepath.Join(flag, "ok"))
	home := t.TempDir()

	// 默认（不能提问）：全部删除。
	out, code := m.in(t, home, nil)
	if code == 0 {
		t.Fatal("应失败")
	}
	mustContain(t, out, "没能正常启动："+bad, "已经正常运行："+good, "全部删除这次迁入的内容", "迁入的内容已全部删除")
	if exists("container", good) || exists("container", bad) || exists("volume", good) || exists("volume", bad) {
		t.Error("全部删除后不应留下迁入的容器和卷")
	}
	noLeftovers(t, home)

	// --keep-succeeded：保留 good，只删除 bad。同时模拟保留下来的部分清理回滚资料失败：
	// 不能把记录一起删掉，下次运行要接着清理。
	out, code = m.in(t, home, []string{"DOCKHOP_TEST_FAIL=commit-cleanup"}, "--keep-succeeded")
	if code == 0 {
		t.Fatal("只迁入了一部分时不能报告成功")
	}
	mustContain(t, out, "⚠️ 只迁入了一部分", "已经迁入、正常运行："+good, "没有迁入（已删除）："+bad, "重新运行同一条迁入命令",
		"有些回滚资料没能删除", "下次运行迁入时会接着删除")
	if b, err := os.ReadFile(filepath.Join(home, "rollback", "current", "journal.jsonl")); err != nil || !strings.Contains(string(b), `"op":"commit"`) {
		t.Errorf("清理没完成时应保留标为已提交的日志：%v\n%s", err, b)
	}
	if s := state(t, good); s != "running" {
		t.Errorf("保留的服务应在运行：%s", s)
	}
	if got := volumeFiles(t, good); !strings.Contains(got, good) {
		t.Errorf("保留的服务的数据：\n%s", got)
	}
	if exists("container", bad) || exists("volume", bad) {
		t.Error("没启动的服务和它的数据应删除")
	}
	goodID := inspect(t, good)["Id"]
	m.out.waitFor(t, regexp.MustCompile("新服务器只迁入了一部分"), time.Minute)

	// 处理好问题后用同一个链接重新迁入：good 不动，只迁入 bad。
	os.WriteFile(filepath.Join(flag, "ok"), nil, 0o644)
	out, code = m.in(t, home, nil)
	if code != 0 {
		t.Fatalf("重新迁入应成功：%d", code)
	}
	mustContain(t, out, "上次已经迁入、这次跳过："+good, "✅ 迁入完成", "上次已经迁入："+good)
	if inspect(t, good)["Id"] != goodID {
		t.Error("已经迁入的服务不应重新创建")
	}
	if !exists("container", decoy) || inspect(t, decoy)["Id"] != decoyID || state(t, decoy) != "running" {
		t.Error("名字以 dockhop-helper- 开头、但不是 DockHop 创建的容器被删了")
	}
	if s := state(t, bad); s != "running" {
		t.Errorf("bad 应在运行：%s", s)
	}
	if got := volumeFiles(t, bad); !strings.Contains(got, bad) {
		t.Errorf("bad 的数据：\n%s", got)
	}
	m.waitOutSuccess(t)
	if left, _ := filepath.Glob(filepath.Join(home, "state", "*")); len(left) > 0 {
		t.Errorf("全部迁入后应删除记录：%v", left)
	}
	noLeftovers(t, home)
}

// 新服务器上的 docker compose 读不懂项目的配置文件时，在改动任何东西之前停下。
func TestInComposePrecheck(t *testing.T) {
	if exec.Command("docker", "compose", "version").Run() != nil {
		t.Skip("没有 docker compose 插件")
	}
	p := fmt.Sprintf("dmtest_cc%d", os.Getpid())
	project := p + "proj"
	t.Cleanup(func() { removeProject(project); removePrefixed(p) })
	sh(t, "docker", "pull", "-q", "alpine:3.20")
	proj := filepath.Join(t.TempDir(), "proj")
	os.MkdirAll(proj, 0o755)
	os.WriteFile(filepath.Join(proj, "compose.yml"), []byte(`services:
  app:
    image: alpine:3.20
    command: ["sleep", "600"]
    labels: {com.centurylinklabs.watchtower.enable: "false"}
`), 0o644)
	up := exec.Command("docker", "compose", "-p", project, "up", "-d")
	up.Dir = proj
	if b, err := up.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	m := startOut(t, "--include", containerByLabel(t, project, "app"))
	removeProject(project)
	os.RemoveAll(proj) // 新服务器上还没有项目目录

	// 假的 docker：compose config 一律报错，其他命令照常。
	real, err := exec.LookPath("docker")
	if err != nil {
		t.Fatal(err)
	}
	fake := t.TempDir()
	os.WriteFile(filepath.Join(fake, "docker"), []byte("#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = config ] && "+
		"{ echo 'services.app Additional property bogus is not allowed' >&2; exit 15; }; done\nexec "+real+" \"$@\"\n"), 0o755)
	home := t.TempDir()
	out, code := m.in(t, home, []string{"PATH=" + fake + ":" + os.Getenv("PATH")})
	if code == 0 {
		t.Fatal("应失败")
	}
	mustContain(t, out, "读不懂 Compose 项目 "+project+" 的配置文件", "bogus", "还没有改动新服务器")
	if c := try("docker", "ps", "-aq", "--filter", "label=com.docker.compose.project="+project); c != "" {
		t.Errorf("不应创建项目的容器：%s", c)
	}
	if _, err := os.Stat(proj); err == nil {
		t.Error("不应写入项目目录")
	}

	// 新服务器上的 docker compose 正常：迁入成功。
	if _, code = m.in(t, home, nil); code != 0 {
		t.Fatalf("应成功：%d", code)
	}
	app := containerByLabel(t, project, "app")
	if s := state(t, app); s != "running" {
		t.Errorf("app 应在运行：%s", s)
	}
	id := inspect(t, app)["Id"]
	again := exec.Command("docker", "compose", "-p", project, "up", "-d")
	again.Dir = proj
	if b, err := again.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	if inspect(t, containerByLabel(t, project, "app"))["Id"] != id {
		t.Error("迁入后 docker compose up 重建了容器")
	}
	m.waitOutSuccess(t)
	noLeftovers(t, home)
}

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
