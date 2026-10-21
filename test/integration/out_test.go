//go:build integration

package integration

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/crypto"
	"github.com/lx969788249/dockhop/internal/transfer"
)

var linkPattern = regexp.MustCompile(`http://127\.0\.0\.1:\d+/[A-Za-z0-9]+/#[0-9a-z]{58}`)

// fixture 是一组覆盖各种情况的测试容器。
type fixture struct {
	prefix  string
	dir     string
	project string
	web     string // 独立容器：卷、挂载目录（含软链接）、固定 IP、停止等待时间、可写层改动
	side    string // 借用 web 网络的容器（没有选中，应自动加入）
	reader  string // 没选中、但和 web 共用卷的容器（需要临时停止）
	app     string // Compose 服务：命名卷 + 匿名卷 + 项目目录以外的 env_file
	worker  string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	p := fmt.Sprintf("dmtest_it%d", os.Getpid())
	f := &fixture{prefix: p, dir: t.TempDir(), project: p + "proj"}
	f.web, f.side, f.reader = p+"_web", p+"_side", p+"_reader"
	octet := 100 + os.Getpid()%100
	t.Cleanup(func() { f.teardown() })

	sh(t, "docker", "pull", "-q", "alpine:3.20")
	sh(t, "docker", "network", "create", "--subnet", fmt.Sprintf("172.31.%d.0/24", octet), p+"_net")
	sh(t, "docker", "volume", "create", p+"_vol")
	sh(t, "docker", "run", "--rm", "-v", p+"_vol:/v", "alpine:3.20", "sh", "-c",
		"echo volume-data >/v/data.txt && chown 1234:5678 /v/data.txt && chmod 640 /v/data.txt && ln -s data.txt /v/link")
	bind := filepath.Join(f.dir, "bind")
	os.MkdirAll(filepath.Join(bind, "sub"), 0o755)
	os.WriteFile(filepath.Join(bind, "sub", "file.txt"), []byte("bind-data"), 0o644)
	real := filepath.Join(f.dir, "realdir")
	os.MkdirAll(real, 0o755)
	os.WriteFile(filepath.Join(real, "r.txt"), []byte("through-symlink"), 0o644)
	os.Symlink(real, filepath.Join(f.dir, "linked"))

	sh(t, "docker", "run", "-dit", "--name", f.web, "--stop-timeout", "30",
		"--network", p+"_net", "--ip", fmt.Sprintf("172.31.%d.10", octet),
		"-v", p+"_vol:/data", "-v", bind+":/bind", "-v", filepath.Join(f.dir, "linked")+":/linked",
		"-v", "/etc/localtime:/etc/localtime:ro", "-p", "127.0.0.1::80",
		"alpine:3.20", "sh")
	sh(t, "docker", "exec", f.web, "sh", "-c", "echo changed >/etc/dockhop-changed")
	sh(t, "docker", "run", "-d", "--name", f.side, "--network", "container:"+f.web, "alpine:3.20", "sleep", "600")
	sh(t, "docker", "run", "-d", "--name", f.reader, "-v", p+"_vol:/shared:ro", "alpine:3.20", "sleep", "600")

	proj := filepath.Join(f.dir, "proj")
	os.MkdirAll(filepath.Join(proj, "web"), 0o755)
	outside := filepath.Join(f.dir, "outside.env")
	os.WriteFile(outside, []byte("FROM_OUTSIDE=yes\n"), 0o644)
	os.WriteFile(filepath.Join(proj, ".env"), []byte("MODE=prod\n"), 0o644)
	os.WriteFile(filepath.Join(proj, "web", "index.html"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(proj, "compose.yml"), []byte(`services:
  app:
    image: alpine:3.20
    command: ["sleep", "600"]
    env_file: ["`+outside+`"]
    environment: {MODE: "${MODE}"}
    volumes: ["data:/data", "/anon"]
  worker:
    image: alpine:3.20
    command: ["sleep", "600"]
volumes:
  data: {}
`), 0o644)
	c := composeCmd(t)
	sh(t, c[0], append(c[1:], "-p", f.project, "-f", filepath.Join(proj, "compose.yml"), "--project-directory", proj, "up", "-d")...)
	f.app = containerByLabel(t, f.project, "app")
	f.worker = containerByLabel(t, f.project, "worker")
	sh(t, "docker", "exec", f.app, "sh", "-c", "echo anon-data >/anon/a.txt && echo named-data >/data/n.txt")
	return f
}

func (f *fixture) teardown() {
	for _, n := range []string{f.web, f.side, f.reader} {
		try("docker", "rm", "-f", n)
	}
	ids := try("docker", "ps", "-aq", "--filter", "label=com.docker.compose.project="+f.project)
	for _, id := range strings.Fields(ids) {
		try("docker", "rm", "-fv", id)
	}
	for _, v := range strings.Fields(try("docker", "volume", "ls", "-q", "--filter", "name="+f.prefix)) {
		try("docker", "volume", "rm", v)
	}
	for _, n := range strings.Fields(try("docker", "network", "ls", "-q", "--filter", "name="+f.prefix)) {
		try("docker", "network", "rm", n)
	}
	for _, img := range strings.Fields(try("docker", "images", "-q", "dockhop-image")) {
		try("docker", "rmi", "-f", img)
	}
}

func freePort() int { return 30000 + os.Getpid()%5000 }

func TestOutBundleContents(t *testing.T) {
	f := setup(t)
	home := t.TempDir()
	env := []string{"DOCKHOP_HOME=" + home, "DOCKHOP_KEEP_LOG=1"}
	p := start(t, env, "out", "--include", f.web+","+f.app, "--stop-shared", "--host", "127.0.0.1", "--port", fmt.Sprint(freePort()))
	link, err := crypto.ParseLink(p.waitFor(t, linkPattern, 5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	out := p.out()
	for _, want := range []string{"[1/7] 检查环境 ✓", "整个项目一起迁移", "共用数据或网络", "[4/7] 停机并打包数据 ✓", "[7/7] 启动下载服务 ✓"} {
		if !strings.Contains(out, want) {
			t.Errorf("输出里缺少「%s」", want)
		}
	}
	if strings.Contains(out, "对不上") {
		t.Error("项目文件没改过（只是用了 env_file），不应报告配置对不上")
	}
	sharedLine := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "共用数据或网络") {
			sharedLine = l
		}
	}
	if !strings.Contains(sharedLine, f.side) || !strings.Contains(sharedLine, f.reader) {
		t.Errorf("借用网络的 side 和共用卷的 reader 都应临时停止：%s", sharedLine)
	}
	// 打包完成后源容器已经恢复原状，下载期间不停机。
	for _, n := range []string{f.web, f.side, f.reader, f.app, f.worker} {
		if s := state(t, n); s != "running" {
			t.Errorf("提供下载时 %s 应已恢复运行，实际 %s", n, s)
		}
	}

	// 像新服务器一样：先拿迁移计划，再下载迁移包、解开。
	plan := download(t, link.PlanURL())
	pl, err := bundle.DecryptPlan(strings.NewReader(plan), link)
	if err != nil {
		t.Fatalf("迁移计划解不开：%v", err)
	}
	dst := t.TempDir()
	resp, err := http.Get(link.BundleURL())
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.Extract(resp.Body, link, dst, nil); err != nil {
		t.Fatalf("迁移包解不开：%v", err)
	}
	resp.Body.Close()
	m, err := bundle.ReadManifest(dst)
	if err != nil {
		t.Fatal(err)
	}

	names := map[string]bundle.Container{}
	for _, c := range m.Containers {
		names[c.Name] = c
	}
	for _, n := range []string{f.web, f.app, f.worker} {
		if _, ok := names[n]; !ok {
			t.Errorf("迁移包里缺少容器 %s", n)
		}
	}
	for _, n := range []string{f.reader, f.side} {
		if _, ok := names[n]; ok {
			t.Errorf("没选中的容器 %s 只应临时停止，不应迁移", n)
		}
	}
	if ch := names[f.web].Changes; ch == nil || strings.Join(ch.Paths, ",") != "/etc/dockhop-changed" {
		t.Errorf("web 改过 /etc，应只迁移改动的文件：%+v", ch)
	} else if e := tarEntries(t, bundle.OpenStream(dst, "changes/"+f.web, ch.Parts))["etc/dockhop-changed"]; e.content != "changed\n" {
		t.Errorf("容器里改动的文件不对：%+v", e)
	}
	if names[f.worker].Changes != nil || names[f.app].Changes != nil {
		t.Error("worker、app 没有改动（挂载点不算），不应迁移改动")
	}
	if names[f.app].Project != f.project {
		t.Errorf("app 应属于项目 %s：%+v", f.project, names[f.app])
	}

	// 容器配置原样保存（包括命令行翻译会丢掉的停止等待时间和 -it）。
	raw, _ := os.ReadFile(filepath.Join(dst, "containers", f.web+".json"))
	var webCfg struct {
		Config struct {
			StopTimeout int  `json:"StopTimeout"`
			Tty         bool `json:"Tty"`
			OpenStdin   bool `json:"OpenStdin"`
		}
	}
	json.Unmarshal(raw, &webCfg)
	if webCfg.Config.StopTimeout != 30 || !webCfg.Config.Tty || !webCfg.Config.OpenStdin {
		t.Errorf("容器配置没有原样保存：%+v", webCfg.Config)
	}

	// 卷：内容、属主、权限、软链接都在。
	var vol, anon *bundle.Volume
	for i := range m.Volumes {
		switch {
		case m.Volumes[i].Name == f.prefix+"_vol":
			vol = &m.Volumes[i]
		case m.Volumes[i].Anonymous:
			anon = &m.Volumes[i]
		}
	}
	if vol == nil || anon == nil {
		t.Fatalf("卷不全：%+v", m.Volumes)
	}
	entries := tarEntries(t, bundle.OpenStream(dst, "volumes/"+vol.Name, vol.Parts))
	if e := entries["dockhop-data/data.txt"]; e.content != "volume-data\n" || e.uid != 1234 || e.gid != 5678 || e.mode != 0o640 {
		t.Errorf("卷里的文件不对：%+v", e)
	}
	if e := entries["dockhop-data/link"]; e.link != "data.txt" {
		t.Errorf("卷里的软链接不对：%+v", e)
	}
	if e := tarEntries(t, bundle.OpenStream(dst, "volumes/"+anon.Name, anon.Parts))["dockhop-data/a.txt"]; e.content != "anon-data\n" {
		t.Errorf("匿名卷的数据不对：%+v", e)
	}

	// 挂载目录：普通目录和软链接目录都在，系统路径不迁移。
	var sawBind, sawLinked bool
	for _, b := range m.Binds {
		es := tarEntries(t, bundle.OpenStream(dst, "binds/"+b.ID, b.Parts))
		if strings.HasSuffix(b.Path, "/bind") && es[strings.TrimPrefix(b.Path, "/")+"/sub/file.txt"].content == "bind-data" {
			sawBind = true
		}
		if strings.HasSuffix(b.Link, "/linked") && es[strings.TrimPrefix(b.Path, "/")+"/r.txt"].content == "through-symlink" {
			sawLinked = true
		}
		if strings.HasPrefix(b.Path, "/etc") {
			t.Errorf("系统路径不应迁移：%s", b.Path)
		}
	}
	if !sawBind || !sawLinked {
		t.Errorf("挂载目录不全：%+v", m.Binds)
	}

	// Compose 项目：整个目录（含 .env 和子目录），以及项目目录以外的 env_file。
	if len(m.Projects) != 1 || m.Projects[0].Name != f.project || len(m.Projects[0].Services) != 2 {
		t.Fatalf("Compose 项目不对：%+v", m.Projects)
	}
	mp := m.Projects[0]
	dir := tarEntries(t, bundle.OpenStream(dst, "compose/"+mp.Name+"/dir", mp.DirParts))
	for _, n := range []string{"compose.yml", ".env", "web/index.html"} {
		if _, ok := dir[n]; !ok {
			t.Errorf("项目目录里缺少 %s", n)
		}
	}
	{
		outside := tarEntries(t, bundle.OpenStream(dst, "compose/"+mp.Name+"/outside", mp.OutParts))
		if outside[strings.TrimPrefix(filepath.Join(f.dir, "outside.env"), "/")].content != "FROM_OUTSIDE=yes\n" {
			t.Errorf("项目目录以外的 env_file 没有迁移：%v", mp.Outside)
		}
	}

	// 镜像：docker save 的格式。
	imgs := tarEntries(t, bundle.OpenStream(dst, "images", m.Images.Parts))
	if _, ok := imgs["manifest.json"]; !ok {
		t.Error("镜像导出不完整")
	}
	if len(m.Images.Refs) != 1 {
		t.Errorf("都用 alpine:3.20，只应导出一个镜像：%v", m.Images.Refs)
	}

	// 迁移计划：端口、固定 IP、项目都在。
	var webPlan *bundle.PlanContainer
	for i := range pl.Containers {
		if pl.Containers[i].Name == f.web {
			webPlan = &pl.Containers[i]
		}
	}
	if webPlan == nil || len(webPlan.Networks) == 0 || webPlan.Networks[0].IP == "" {
		t.Errorf("迁移计划里 web 的网络不对：%+v", webPlan)
	}

	// 新服务器报告迁入成功后，旧服务器自动结束、清理干净。
	if err := transfer.SendStatus(context.Background(), link, transfer.Status{Result: "success"}); err != nil {
		t.Fatal(err)
	}
	if code := p.wait(t, time.Minute); code != 0 {
		t.Fatalf("迁出应成功结束，退出码 %d：\n%s", code, p.out())
	}
	out = p.out()
	for _, want := range []string{"新服务器已迁入成功", "✅ 迁出完成", "源容器：已恢复 5/5"} {
		if !strings.Contains(out, want) {
			t.Errorf("结束时输出里缺少「%s」：\n%s", want, out)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(home, "work", "*")); len(left) > 0 {
		t.Errorf("工作目录没有清理：%v", left)
	}
	if imgs := try("docker", "images", "-q", "dockhop-image"); imgs != "" {
		t.Errorf("临时镜像名没有删除：%s", imgs)
	}
	if h := try("docker", "ps", "-aq", "--filter", "label=dockhop.helper=1"); h != "" {
		t.Errorf("临时容器没有删除：%s", h)
	}
	if strings.Contains(readLogs(home), link.Secret()) {
		t.Error("日志里不应出现链接密钥")
	}
}

// 停机期间程序被强制结束：容器停着，下次运行时恢复原状。
func TestOutKilledWhileStopped(t *testing.T) {
	f := setup(t)
	home := t.TempDir()
	env := []string{"DOCKHOP_HOME=" + home, "DOCKHOP_TEST_PAUSE=after-stop"}
	p := start(t, env, "out", "--include", f.web, "--stop-shared", "--host", "127.0.0.1", "--port", fmt.Sprint(freePort()+1))
	waitFile(t, filepath.Join(home, "test-paused-after-stop"), 5*time.Minute)
	if s := state(t, f.web); s != "exited" {
		t.Fatalf("停机期间 web 应已停止，实际 %s", s)
	}
	_ = p.cmd.Process.Kill() // 相当于 kill -9
	p.wait(t, time.Minute)
	for _, n := range []string{f.web, f.side, f.reader} {
		if s := state(t, n); s != "exited" {
			t.Fatalf("被强制结束后 %s 应仍停着（用来验证恢复），实际 %s", n, s)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "state", "out-stopped.json")); err != nil {
		t.Fatalf("停机记录应留在磁盘上：%v", err)
	}
	// 下次运行（这里故意给一个不存在的容器，恢复应发生在报错之前）。
	cmd := exec.Command(bin, "out", "--include", "no-such-container")
	cmd.Env = append(os.Environ(), "DOCKHOP_HOME="+home)
	b, _ := cmd.CombinedOutput()
	out := string(b)
	if !strings.Contains(out, "上次迁出") || !strings.Contains(out, "已恢复 3 个容器") {
		t.Fatalf("应提示并恢复上次停下的容器：\n%s", out)
	}
	for _, n := range []string{f.web, f.side, f.reader} {
		if s := state(t, n); s != "running" {
			t.Errorf("%s 应已恢复运行，实际 %s", n, s)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "state", "out-stopped.json")); !os.IsNotExist(err) {
		t.Error("恢复后应删除停机记录")
	}
}

// 磁盘空间不够时在停机之前就停下，不碰任何容器。
func TestOutDiskCheckBeforeStop(t *testing.T) {
	small := filepath.Join(t.TempDir(), "small")
	os.MkdirAll(small, 0o755)
	if err := exec.Command("sudo", "-n", "mount", "-t", "tmpfs", "-o", "size=1m", "tmpfs", small).Run(); err != nil {
		t.Skip("无法挂载小磁盘做测试：", err)
	}
	defer exec.Command("sudo", "-n", "umount", small).Run()
	f := setup(t)
	cmd := exec.Command(bin, "out", "--include", f.web, "--stop-shared", "--workdir", small, "--port", fmt.Sprint(freePort()+2))
	cmd.Env = append(os.Environ(), "DOCKHOP_HOME="+t.TempDir())
	b, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(b), "磁盘空间可能不够") || !strings.Contains(string(b), "--workdir") {
		t.Fatalf("磁盘不够时应停下并提示：%v\n%s", err, b)
	}
	if strings.Contains(string(b), "[4/7]") {
		t.Fatal("磁盘不够时不应进入停机步骤")
	}
	if s := state(t, f.web); s != "running" {
		t.Fatalf("磁盘不够时不应停止任何容器，web：%s", s)
	}
}

// 不停机打包时，和别的容器共用数据就无法保证一致，直接拒绝。
func TestOutNoStopWithSharedData(t *testing.T) {
	f := setup(t)
	cmd := exec.Command(bin, "out", "--include", f.web, "--no-stop", "--port", fmt.Sprint(freePort()+3))
	cmd.Env = append(os.Environ(), "DOCKHOP_HOME="+t.TempDir())
	b, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(b), "不停机打包无法保证数据一致") {
		t.Fatalf("应拒绝：%v\n%s", err, b)
	}
}

type entry struct {
	content  string
	link     string
	uid, gid int
	mode     int64
}

func tarEntries(t *testing.T, rc io.ReadCloser) map[string]entry {
	t.Helper()
	defer rc.Close()
	out := map[string]entry{}
	tr := tar.NewReader(rc)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("tar 读不了：%v", err)
		}
		e := entry{link: h.Linkname, uid: h.Uid, gid: h.Gid, mode: h.Mode & 0o7777}
		if h.Typeflag == tar.TypeReg && h.Size < 1<<20 {
			b, _ := io.ReadAll(tr)
			e.content = string(b)
		}
		out[strings.TrimSuffix(h.Name, "/")] = e
	}
}

func download(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("%s 返回 %d", url, resp.StatusCode)
	}
	return string(b)
}

func readLogs(home string) string {
	var b strings.Builder
	files, _ := filepath.Glob(filepath.Join(home, "logs", "*.log"))
	for _, f := range files {
		c, _ := os.ReadFile(f)
		b.Write(c)
	}
	return b.String()
}

// 启动时用了 shell 里临时设置的变量：迁出时应提醒，迁过去会按文件里的值启动。
func TestOutComposeDrift(t *testing.T) {
	if composeLegacy() {
		t.Skip("老的 docker-compose 不支持核对")
	}
	f := setup(t)
	proj := filepath.Join(f.dir, "proj")
	c := composeCmd(t)
	up := exec.Command(c[0], append(c[1:], "-p", f.project, "-f", filepath.Join(proj, "compose.yml"), "--project-directory", proj, "up", "-d", "app")...)
	up.Env = append(os.Environ(), "MODE=dev") // 覆盖 .env 里的 MODE=prod
	if b, err := up.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	home := t.TempDir()
	p := start(t, []string{"DOCKHOP_HOME=" + home}, "out", "--include", f.app, "--host", "127.0.0.1", "--port", fmt.Sprint(freePort()+4))
	p.waitFor(t, linkPattern, 5*time.Minute)
	if !strings.Contains(p.out(), "app（环境变量 MODE）") {
		t.Errorf("应提醒 app 的 MODE 和文件里不一致：\n%s", p.out())
	}
	_ = p.cmd.Process.Signal(os.Interrupt)
	if code := p.wait(t, time.Minute); code != 0 {
		t.Errorf("按 Ctrl+C 结束传输应正常退出，退出码 %d", code)
	}
}

// 容器里没挂载出来的改动很大时（真机实测：waline 容器里积攒了 6.6GB 崩溃转储），
// 要在停机之前提醒：这些都得在停机期间打包。
func TestOutLargeLayerWarns(t *testing.T) {
	name := fmt.Sprintf("dmtest_big%d", os.Getpid())
	t.Cleanup(func() { try("docker", "rm", "-f", name) })
	sh(t, "docker", "pull", "-q", "alpine:3.20")
	sh(t, "docker", "run", "-d", "--name", name, "--label", "com.centurylinklabs.watchtower.enable=false", "alpine:3.20", "sleep", "600")
	sh(t, "docker", "exec", name, "sh", "-c", "head -c 120000000 /dev/urandom >/core.1234")
	p := start(t, []string{"DOCKHOP_HOME=" + t.TempDir()}, "out", "--include", name, "--host", "127.0.0.1", "--port", fmt.Sprint(freePort()+5))
	p.waitFor(t, linkPattern, 5*time.Minute)
	out := p.out()
	warn := strings.Index(out, "里没挂载出来的改动有 114MB")
	stop := strings.Index(out, "[4/7]")
	if warn < 0 || warn > stop {
		t.Errorf("应在停机之前提醒容器里的改动很大：\n%s", out)
	}
	_ = p.cmd.Process.Signal(os.Interrupt)
	p.wait(t, time.Minute)
}
