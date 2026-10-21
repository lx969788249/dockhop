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
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/crypto"
)

// 事务和安全场景（设计文档附录 C）：新服务器上已经有同名的服务、数据时，迁入失败或
// 被中断都要把新服务器还原到迁入前的样子；不安全的情况在改动任何东西之前停下。

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
	mustContain(t, out, "❌ 迁入失败，已安全还原", "原来的服务和数据都已还原", "重新运行时不用重新下载")
	f.targetIntact(t, oldID)
	noLeftovers(t, home)

	// 附录 C-12：数据写入失败时准确报告并还原。
	out, code = m.in(t, home, []string{"DOCKHOP_TEST_FAIL=volume:" + f.vol})
	if code == 0 {
		t.Fatal("模拟写入失败时迁入应失败")
	}
	mustContain(t, out, "数据卷 "+f.vol+" 写入失败", "已安全还原")
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
	mustContain(t, p.out(), "⚠️ 迁入已中断，已安全还原")
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
	mustContain(t, out, "上次迁入", "已还原上次迁入之前的状态", "✅ 迁入完成")
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
	mustContain(t, out, "自动还原没有完全成功", "回滚资料：")
	if strings.Contains(out, "已安全还原") {
		t.Error("回滚没有完全成功时不能说已安全还原")
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
	if strings.Contains(out, "还原到迁入前") && strings.Contains(out, "迁入没有完成") {
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
	mustContain(t, out, "健康检查没有通过", "已安全还原")
	checkOld()

	// 附录 C-8：没有健康检查的 web 启动后马上退出——判为失败，旧容器保留。
	os.WriteFile(filepath.Join(appFlag, "ok"), nil, 0o644)
	os.Remove(filepath.Join(webFlag, "ok"))
	out, code = m.in(t, home, nil)
	if code == 0 {
		t.Fatal("应失败")
	}
	mustContain(t, out, "启动后马上退出", "已安全还原")
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
