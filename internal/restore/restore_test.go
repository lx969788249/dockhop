package restore

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/crypto"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/logging"
	"github.com/lx969788249/dockhop/internal/transfer"
	"github.com/lx969788249/dockhop/internal/ui"
)

func TestJournalTornLastLine(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rb")
	j, err := openJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	j.add(Entry{Op: opVolume, Name: "data"})
	j.close()
	// 写最后一条时断电：只写了一半。
	f, _ := os.OpenFile(journalPath(dir), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"op":"bind","path":"/sr`)
	f.Close()
	entries, err := readJournal(dir)
	if err != nil || len(entries) != 2 || entries[1].Op != opVolume || committed(entries) {
		t.Fatalf("应忽略写了一半的最后一行：%+v %v", entries, err)
	}
}

// snapshotTree 记下目录里每个文件的类型、内容和权限，用来比较还原前后是否完全一样。
func snapshotTree(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		line := rel + " " + info.Mode().String()
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			l, _ := os.Readlink(p)
			line += " -> " + l
		case info.Mode().IsRegular():
			b, _ := os.ReadFile(p)
			line += " " + string(b)
		}
		lines = append(lines, line)
		return nil
	})
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// 文件、目录、软链接的改动按日志倒着还原后，和改动前完全一样。
func TestFileChangesRollBack(t *testing.T) {
	root := t.TempDir()
	world := filepath.Join(root, "world")
	os.MkdirAll(filepath.Join(world, "srv", "data", "old-sub"), 0o755)
	os.WriteFile(filepath.Join(world, "srv", "data", "keep.txt"), []byte("old data"), 0o600)
	os.MkdirAll(filepath.Join(world, "proj"), 0o755)
	os.WriteFile(filepath.Join(world, "proj", "compose.yml"), []byte("old compose"), 0o644)
	os.WriteFile(filepath.Join(world, "proj", "local-only.txt"), []byte("not in bundle"), 0o644)
	before := snapshotTree(t, world)

	var out bytes.Buffer
	r := &run{Deps: Deps{Screen: ui.New(&out, false, nil), Home: filepath.Join(root, "home")}}
	j, err := openJournal(filepath.Join(root, "rb"))
	if err != nil {
		t.Fatal(err)
	}
	r.j = j

	// 目录整体替换。
	staged := filepath.Join(world, "srv", ".stage")
	os.MkdirAll(filepath.Join(staged, "new-sub"), 0o700)
	os.WriteFile(filepath.Join(staged, "new.txt"), []byte("migrated"), 0o640)
	if err := r.replaceDir(staged, filepath.Join(world, "srv", "data")); err != nil {
		t.Fatal(err)
	}
	// 新建的目录和软链接。
	if err := r.mkdirAll(filepath.Join(world, "opt", "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.restoreLink(filepath.Join(world, "opt", "app", "data"), filepath.Join(world, "srv", "data")); err != nil {
		t.Fatal(err)
	}
	// Compose 项目文件合并安装：覆盖已有的、新增没有的、不动包里没有的。
	stage := filepath.Join(root, "stage")
	os.MkdirAll(filepath.Join(stage, "web"), 0o755)
	os.WriteFile(filepath.Join(stage, "compose.yml"), []byte("new compose"), 0o644)
	os.WriteFile(filepath.Join(stage, ".env"), []byte("A=1"), 0o600)
	os.WriteFile(filepath.Join(stage, "web", "index.html"), []byte("hi"), 0o644)
	if err := r.installTree(stage, filepath.Join(world, "proj"), nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(world, "proj", "compose.yml")); string(b) != "new compose" {
		t.Fatalf("应覆盖：%s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(world, "proj", "local-only.txt")); string(b) != "not in bundle" {
		t.Fatal("包里没有的文件不应被动")
	}
	if b, _ := os.ReadFile(filepath.Join(world, "opt", "app", "data", "new.txt")); string(b) != "migrated" {
		t.Fatal("软链接应指向迁移过来的目录")
	}
	r.j.close()

	entries, err := readJournal(filepath.Join(root, "rb"))
	if err != nil {
		t.Fatal(err)
	}
	u := &undo{screen: r.Screen, logf: t.Logf, fail: func(string) bool { return false }}
	if problems := u.rollback(context.Background(), entries); len(problems) > 0 {
		t.Fatalf("还原有问题：%v", problems)
	}
	if after := snapshotTree(t, world); after != before {
		t.Fatalf("还原后和原来不一样：\n原来：\n%s\n现在：\n%s", before, after)
	}
	// 再还原一次也没问题（中断后下次运行会从头再还原一遍）。
	if problems := u.rollback(context.Background(), entries); len(problems) > 0 {
		t.Fatalf("重复还原：%v", problems)
	}
	if after := snapshotTree(t, world); after != before {
		t.Fatal("重复还原后不一样了")
	}
}

func TestDependencyOrder(t *testing.T) {
	mk := func(name, mode string, links ...string) *citem {
		c := &docker.Container{Name: "/" + name}
		c.HostConfig.NetworkMode = mode
		c.HostConfig.VolumesFrom = links
		return &citem{m: bundle.Container{Name: name}, c: c}
	}
	items := []*citem{mk("side", "container:abc123abc123"), mk("app", "bridge", "data"), mk("data", "bridge"), mk("web", "bridge")}
	nameOf := func(ref string) string {
		if strings.HasPrefix("abc123abc123ffff", ref) {
			return "web"
		}
		return ""
	}
	var got []string
	for _, it := range dependencyOrder(items, (*citem).name, func(it *citem) []string { return containerDeps(it.c, nameOf) }) {
		got = append(got, it.m.Name)
	}
	pos := map[string]int{}
	for i, n := range got {
		pos[n] = i
	}
	if pos["web"] > pos["side"] || pos["data"] > pos["app"] || len(got) != 4 {
		t.Errorf("被依赖的容器应先创建：%v", got)
	}
	// 循环依赖时不卡住。
	a, b := mk("a", "container:b"), mk("b", "container:a")
	if n := len(dependencyOrder([]*citem{a, b}, (*citem).name, func(it *citem) []string { return containerDeps(it.c, func(string) string { return "" }) })); n != 2 {
		t.Errorf("循环依赖：%d", n)
	}
}

func TestHealthTimeout(t *testing.T) {
	s := int64(time.Second)
	if got := healthTimeout(60*time.Second, 0, 2*s, 2*s, 1); got != 60*time.Second {
		t.Errorf("检查得勤时至少等 60 秒：%s", got)
	}
	// 1Panel 装的 Halo：启动后 30 秒内不算，之后每 30 秒一次、每次最多 5 秒，连续 5 次没通过才算
	// 不健康。Docker 最晚 30 + 5 ×（30 + 5）= 205 秒下结论，再多等 30 秒。以前只等 95 秒。
	if got := healthTimeout(60*time.Second, 30*s, 30*s, 5*s, 5); got != 235*time.Second {
		t.Errorf("应等到 Docker 下结论再多 30 秒：%s", got)
	}
	// 每次检查都要用满时间（检查很慢）时，每一轮都要算上：30 + 5 ×（30 + 30）+ 30 = 360 秒。
	if got := healthTimeout(60*time.Second, 30*s, 30*s, 30*s, 5); got != 360*time.Second {
		t.Errorf("每一轮检查的时间都要算上：%s", got)
	}
	// Vaultwarden：每 60 秒一次、每次最多 10 秒，没写次数（Docker 默认 3 次）。
	if got := healthTimeout(60*time.Second, 0, 60*s, 10*s, 0); got != 240*time.Second {
		t.Errorf("没写的按 Docker 的默认值：%s", got)
	}
	if got := healthTimeout(60*time.Second, 2000*s, 2000*s, 0, 3); got != time.Hour {
		t.Errorf("最多一小时：%s", got)
	}
}

func TestEntryJSON(t *testing.T) {
	e := Entry{Op: opRename, Name: "web", Networks: json.RawMessage(`{"appnet":{"IPAMConfig":{"IPv4Address":"10.0.0.2"}}}`)}
	b, _ := json.Marshal(e)
	var back Entry
	if err := json.Unmarshal(b, &back); err != nil || string(back.Networks) != string(e.Networks) {
		t.Errorf("日志记录应能原样读回：%s", b)
	}
}

// 有关联的服务分在同一组，互不相干的分开；临时停止的已有容器跟着它用到的数据走。
func TestBuildGroups(t *testing.T) {
	mk := func(name, project, js string) *citem {
		var c docker.Container
		if err := json.Unmarshal([]byte(js), &c); err != nil {
			t.Fatal(err)
		}
		c.Name = "/" + name
		return &citem{m: bundle.Container{Name: name, Project: project}, c: &c}
	}
	r := &run{m: &bundle.Manifest{
		Binds:    []bundle.Bind{{Path: "/srv/shared"}, {Path: "/srv/blog"}, {Path: "/var/run/docker.sock"}},
		Volumes:  []bundle.Volume{{Name: "dbdata"}, {Name: "cache"}},
		Projects: []bundle.Project{{Name: "blog", WorkingDir: "/srv/blog/app"}},
	}}
	r.projects = r.m.Projects
	r.items = []*citem{
		mk("web", "", `{"Mounts":[{"Type":"bind","Source":"/srv/shared/html"}],"NetworkSettings":{"Networks":{"bridge":{}}}}`),
		mk("nginx", "", `{"Mounts":[{"Type":"bind","Source":"/srv/shared"},{"Type":"bind","Source":"/var/run/docker.sock"}]}`),
		mk("blog-app-1", "blog", `{"NetworkSettings":{"Networks":{"blog_default":{}}}}`),
		mk("blog-db-1", "blog", `{"Mounts":[{"Type":"volume","Name":"dbdata"}]}`),
		mk("files", "", `{"Mounts":[{"Type":"bind","Source":"/srv/blog/uploads"}]}`),
		mk("redis", "", `{"Mounts":[{"Type":"volume","Name":"cache"}],"NetworkSettings":{"Networks":{"bridge":{}}}}`),
		mk("exporter", "", `{"HostConfig":{"NetworkMode":"container:redis"}}`),
		mk("alone", "", `{"Mounts":[{"Type":"bind","Source":"/var/run/docker.sock"}]}`),
	}
	var w docker.Container
	json.Unmarshal([]byte(`{"Mounts":[{"Type":"volume","Name":"cache"}]}`), &w)
	r.buildGroups([]writer{{summary: docker.ContainerSummary{Names: []string{"/backup"}}, full: &w}})

	var got []string
	for _, g := range r.groups {
		got = append(got, g.label())
	}
	want := []string{"web、nginx", "Compose 项目 blog、files", "redis、exporter", "alone"}
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("分组不对：\n现在：%v\n应该：%v", got, want)
	}
	if r.groupOf["v:dbdata"] != r.groupOf["p:blog"] || r.groupOf["b:/srv/blog"] != r.groupOf["p:blog"] {
		t.Error("项目用到的卷、项目目录所在的挂载目录应和项目同组")
	}
	if r.groupOf["w:backup"] != r.groupOf["c:redis"] {
		t.Error("临时停止的容器应和用同一个卷的服务同组")
	}
	if _, ok := r.groupOf["b:/var/run/docker.sock"]; ok {
		t.Error("宿主机系统路径不应把服务连到一起")
	}
}

// 只删除没启动的组时，日志里只留下这些组的改动（和开头的那条），之后再被强制结束也只会删它们。
func TestRewriteJournal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rb")
	j, err := openJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	j.group = "1"
	j.add(Entry{Op: opContainer, Name: "web"})
	j.group = "2"
	j.add(Entry{Op: opContainer, Name: "api"})
	j.add(Entry{Op: opVolume, Name: "keep", Group: "1"})
	j.close()
	entries, _ := readJournal(dir)
	var drop []Entry
	for _, e := range entries {
		if e.Op == opStart || e.Group == "2" {
			drop = append(drop, e)
		}
	}
	if len(drop) != 2 || entries[3].Group != "1" {
		t.Fatalf("组记录不对：%+v", entries)
	}
	if err := rewriteJournal(dir, drop); err != nil {
		t.Fatal(err)
	}
	back, _ := readJournal(dir)
	if len(back) != 2 || back[0].Op != opStart || back[1].Name != "api" || committed(back) {
		t.Fatalf("改写后的日志不对：%+v", back)
	}
}

// 日志中间有一条记录损坏时，不知道漏掉的是哪一处改动，不能当它不存在。
func TestJournalDamagedMiddleLine(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rb")
	j, err := openJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	j.add(Entry{Op: opVolume, Name: "data"})
	j.close()
	f, _ := os.OpenFile(journalPath(dir), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString("{\"op\":\"bind\",\"pa\x00\x00\n")
	f.WriteString(`{"op":"container","name":"web"}` + "\n")
	f.Close()
	if _, err := readJournal(dir); !errors.Is(err, ErrJournalDamaged) {
		t.Fatalf("中间损坏应报错：%v", err)
	}
	// 读得懂但不是日志记录的行也算损坏。
	os.WriteFile(journalPath(dir), []byte(`{"op":"start"}`+"\n"+`{"x":1}`+"\n"+`{"op":"commit"}`+"\n"), 0o600)
	if _, err := readJournal(dir); !errors.Is(err, ErrJournalDamaged) {
		t.Fatalf("没有 op 的记录应算损坏：%v", err)
	}
}

// 副本按编号排好，才能和新服务器上的容器一一对应（编号可能不连续）。
func TestReplicasOrder(t *testing.T) {
	mk := func(name, svc, num string) *citem {
		c := &docker.Container{}
		c.Config.Labels = map[string]string{"com.docker.compose.container-number": num}
		return &citem{m: bundle.Container{Name: name, Service: svc}, c: c}
	}
	items := []*citem{mk("p-web-3", "web", "3"), mk("p-db-1", "db", "1"), mk("p-web-1", "web", "1"), mk("p-web-10", "web", "10")}
	var got []string
	for _, it := range replicas(items, "web") {
		got = append(got, it.m.Name)
	}
	if strings.Join(got, ",") != "p-web-1,p-web-3,p-web-10" {
		t.Errorf("副本顺序：%v", got)
	}
}

// 写到同一处的项目必须分在同一组：只删除没启动的那组时，不能删掉或换回保留下来的项目还在用的文件。
func TestBuildGroupsSharedPaths(t *testing.T) {
	cases := []struct {
		name     string
		projects []bundle.Project
		together bool
	}{
		{"同一个目录", []bundle.Project{{Name: "a", WorkingDir: "/srv/app"}, {Name: "b", WorkingDir: "/srv/app"}}, true},
		{"一个在另一个里面", []bundle.Project{{Name: "a", WorkingDir: "/srv/app"}, {Name: "b", WorkingDir: "/srv/app/admin"}}, true},
		{"目录外的文件在另一个项目里", []bundle.Project{{Name: "a", WorkingDir: "/srv/a", Outside: []string{"/srv/b/.env"}}, {Name: "b", WorkingDir: "/srv/b"}}, true},
		{"共用目录外的同一个文件", []bundle.Project{{Name: "a", WorkingDir: "/srv/a", Outside: []string{"/etc/app.env"}}, {Name: "b", WorkingDir: "/srv/b", Outside: []string{"/etc/app.env"}}}, true},
		{"互不相干", []bundle.Project{{Name: "a", WorkingDir: "/srv/a"}, {Name: "b", WorkingDir: "/srv/ab"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &run{m: &bundle.Manifest{Projects: c.projects}, projects: c.projects}
			for _, p := range c.projects {
				r.items = append(r.items, &citem{m: bundle.Container{Name: p.Name + "-web-1", Project: p.Name}, c: &docker.Container{}})
			}
			r.buildGroups(nil)
			if together := r.groupOf["p:a"] == r.groupOf["p:b"]; together != c.together {
				t.Errorf("同组：%v，应该：%v（%d 组）", together, c.together, len(r.groups))
			}
		})
	}
}

// 父目录是软链接时，写法不同的两个路径可能是同一处（还不存在的末级目录、文件也一样），
// 要按实际位置分到同一组。
func TestBuildGroupsSymlinkParent(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "real"), 0o755)
	os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "alias"))
	for name, projects := range map[string][]bundle.Project{
		"项目目录": {{Name: "a", WorkingDir: filepath.Join(root, "alias", "app")}, {Name: "b", WorkingDir: filepath.Join(root, "real", "app")}},
		"目录外的文件": {{Name: "a", WorkingDir: filepath.Join(root, "a"), Outside: []string{filepath.Join(root, "alias", "shared.env")}},
			{Name: "b", WorkingDir: filepath.Join(root, "b"), Outside: []string{filepath.Join(root, "real", "shared.env")}}},
	} {
		r := &run{m: &bundle.Manifest{Projects: projects}, projects: projects}
		for _, p := range projects {
			r.items = append(r.items, &citem{m: bundle.Container{Name: p.Name + "-web-1", Project: p.Name}, c: &docker.Container{}})
		}
		r.buildGroups(nil)
		if r.groupOf["p:a"] != r.groupOf["p:b"] {
			t.Errorf("%s：经过软链接写到同一处，应在同一组", name)
		}
	}
}

// 分在不同组的项目互不影响：只删除一组时，另一组的文件原样保留。
func TestPartialRollbackKeepsOtherGroup(t *testing.T) {
	root := t.TempDir()
	stage := filepath.Join(root, "stage")
	os.Mkdir(stage, 0o700)
	os.WriteFile(filepath.Join(stage, "compose.yml"), []byte("services: {}"), 0o600)
	projects := []bundle.Project{{Name: "a", WorkingDir: filepath.Join(root, "a")}, {Name: "b", WorkingDir: filepath.Join(root, "b")}}
	var out bytes.Buffer
	r := &run{Deps: Deps{Screen: ui.New(&out, false, nil)}, m: &bundle.Manifest{Projects: projects}, projects: projects}
	for _, p := range projects {
		r.items = append(r.items, &citem{m: bundle.Container{Name: p.Name + "-web-1", Project: p.Name}, c: &docker.Container{}})
	}
	r.buildGroups(nil)
	j, err := openJournal(filepath.Join(root, "rb"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.close()
	r.j = j
	for _, p := range projects {
		r.in("p:" + p.Name)
		if err := r.installTree(stage, p.WorkingDir, nil); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := readJournal(j.dir)
	if err != nil {
		t.Fatal(err)
	}
	var drop []Entry
	for _, e := range entries {
		if e.Group == r.groupOf["p:a"] {
			drop = append(drop, e)
		}
	}
	u := &undo{screen: r.Screen, logf: t.Logf, fail: func(string) bool { return false }}
	if problems := u.rollback(context.Background(), drop); len(problems) > 0 {
		t.Fatal(problems)
	}
	if _, err := os.Stat(filepath.Join(root, "a", "compose.yml")); !os.IsNotExist(err) {
		t.Errorf("删除的那组应删掉：%v", err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "b", "compose.yml")); err != nil || string(b) != "services: {}" {
		t.Errorf("保留的那组应原样保留：%q %v", b, err)
	}
}

// 迁入要写到的地方包括 Compose 项目的目录和项目目录外的文件：新服务器上用着它们的已有容器
// 要先停下，保留的服务则不能迁入（不然项目文件会盖掉它正在用的配置）。
func TestWriteSetIncludesProjectFiles(t *testing.T) {
	r := &run{m: &bundle.Manifest{}, projects: []bundle.Project{{Name: "blog", WorkingDir: "/srv/blog", Outside: []string{"/etc/blog.env"}}}}
	vols, dirs := r.writeSet()
	if len(vols) != 0 || fmt.Sprint(dirs) != "[{p:blog /srv/blog} {p:blog /etc/blog.env}]" {
		t.Fatalf("要写的地方：%v %v", vols, dirs)
	}
	c := &docker.Container{Mounts: []docker.Mount{{Type: "bind", Source: "/srv/blog/config"}}}
	if !usesData(c, vols, dirs) {
		t.Error("挂载了项目目录里的子目录，也算在用要写入的数据")
	}
}

// 检查 Compose 配置时，报错说的是临时目录里没有的文件（迁入时会放回去），不算数；说的是
// 临时目录里有的文件（读不懂它），算数。
func TestMissingIn(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "compose.yml"), []byte("x"), 0o600)
	if !missingIn("env file "+dir+"/conf/app.env not found: stat "+dir+"/conf/app.env: no such file or directory", dir) {
		t.Error("没有的文件应算作缺文件")
	}
	if missingIn(dir+"/compose.yml: yaml: line 3: mapping values are not allowed", dir) {
		t.Error("有的文件读不懂，不是缺文件")
	}
}

// 下载的文件和迁移计划里记的 SHA-256 对不上（被换成了同一次迁移的另一个文件也一样）：删掉，
// 不能用。对得上、或者旧版本的计划没有记录时照常用。
func TestVerifyDownload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image-1")
	os.WriteFile(path, []byte("image-2 的内容"), 0o600)
	var out bytes.Buffer
	r := &run{Deps: Deps{Screen: ui.New(&out, false, nil)}}
	want, _ := fileSHA256(path)
	if err := r.verifyDownload(path, want, "镜像文件"); err != nil {
		t.Fatal(err)
	}
	if err := r.verifyDownload(path, "", "镜像文件"); err != nil {
		t.Fatal(err)
	}
	if err := r.verifyDownload(path, strings.Repeat("0", 64), "镜像文件"); err == nil || !strings.Contains(out.String(), "对不上") {
		t.Fatalf("对不上时应失败：%v\n%s", err, out.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("对不上的文件应删掉")
	}
}

// 旧服务器有迁移包、没有迁移计划：不能迁入。迁入前检查和核对下载的文件都靠它，
// 以前没有它时会跳过检查，直接替换同名的服务。
func TestRefusesWithoutPlan(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	link, _ := crypto.NewLink("127.0.0.1", ln.Addr().(*net.TCPAddr).Port)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "bundle"), []byte("bundle"), 0o600)
	srv, err := transfer.NewServer(ln, link, filepath.Join(dir, "bundle"), "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	log, err := logging.Open(filepath.Join(dir, "logs"), "in", &logging.Redactor{})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Finish(logging.Failed, true)
	var out bytes.Buffer
	r := &run{Deps: Deps{Screen: ui.New(&out, false, nil), Log: log, Getenv: func(string) string { return "" }}, opts: Options{Link: link}}
	if err := r.fetchPlan(context.Background()); err == nil || r.plan != nil || !strings.Contains(out.String(), "没有提供迁移计划") {
		t.Fatalf("没有迁移计划时应拒绝迁入：%v\n%s", err, out.String())
	}
}

// 解开后发现迁移包里的内容有问题：下载的文件重新运行也用不上，删掉。只有解密发现被改坏、而迁移
// 计划没有记 SHA-256（旧版本）时，才可能是下载时出的错，提示重新下载；其余只能在旧服务器重新迁出。
func TestBadBundleDropsDownload(t *testing.T) {
	cases := []struct {
		name              string
		corrupt, verified bool
		redownload        bool
	}{
		{"内容不合规、核对过", false, true, false},
		{"内容不合规、计划没记校验值", false, false, false}, // 解密通过就是旧服务器打包的内容
		{"被改坏、核对过", true, true, false},         // 旧服务器上的文件本身就坏了
		{"被改坏、计划没记校验值", true, false, true},
	}
	for _, c := range cases {
		dir := t.TempDir()
		link, _ := crypto.NewLink("127.0.0.1", 1)
		path := filepath.Join(dir, "bundle")
		f, _ := os.Create(path)
		w, _ := bundle.NewWriter(f, link)
		var tb bytes.Buffer
		tw := tar.NewWriter(&tb)
		tw.WriteHeader(&tar.Header{Name: "dockhop-data/", Typeflag: tar.TypeDir, Mode: 0o755})
		tw.WriteHeader(&tar.Header{Name: "dockhop-data/etc", Typeflag: tar.TypeSymlink, Linkname: "/etc"})
		tw.WriteHeader(&tar.Header{Name: "dockhop-data/etc/evil", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4})
		tw.Write([]byte("evil"))
		tw.Close()
		parts, n, _ := w.AddStream("volumes/v", &tb)
		w.AddJSON("manifest.json", bundle.Manifest{Format: bundle.Format, Version: bundle.Version,
			Volumes: []bundle.Volume{{Name: "v", Driver: "local", Bytes: n, Parts: parts}}})
		w.Close()
		f.Close()
		if c.corrupt {
			b, _ := os.ReadFile(path)
			b[len(b)-20] ^= 0xff
			os.WriteFile(path, b, 0o600)
		}
		plan := &bundle.Plan{}
		if c.verified {
			plan.BundleSHA256, _ = fileSHA256(path)
		}
		var out bytes.Buffer
		r := &run{Deps: Deps{Screen: ui.New(&out, false, nil), Home: dir}, opts: Options{Link: link}, plan: plan,
			downloaded: true, bundlePath: path, dataDir: filepath.Join(dir, "data")}
		_, err := r.unpack(context.Background())
		got := r.Screen.Failure()
		if err == nil || got == nil || !strings.Contains(got.Hint, "在旧服务器重新运行迁出") || strings.Contains(got.Hint, "重新下载") != c.redownload {
			t.Errorf("%s：%v %+v", c.name, err, got)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s：下载的迁移包应删掉", c.name)
		}
	}
}

// 解开后按迁移包里实际要写的目录再查一遍：目录或者它的软链接盖住 DockHop 自己的目录时不迁，
// 提示换 DOCKHOP_HOME（和迁入前检查同一条规则）。
func TestHomeClash(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "dockhop")
	app := filepath.Join(root, "app")
	r := &run{Deps: Deps{Home: home}, m: &bundle.Manifest{Binds: []bundle.Bind{{Path: app}}}}
	if is := r.homeClash(); is != nil {
		t.Fatalf("不重叠时不应拦下：%s", is.Title)
	}
	r.m.Binds[0].Links = []bundle.BindLink{{Link: filepath.Join(home, "cfg"), Target: app}}
	if is := r.homeClash(); is == nil || !strings.Contains(is.Title, filepath.Join(home, "cfg")) || !strings.Contains(is.Hint, "DOCKHOP_HOME") {
		t.Fatalf("软链接在 DockHop 的目录里时应拦下：%+v", is)
	}
}

// 换了新链接时，以前的链接留下的下载和记录用不上了，删掉；这个链接自己的（重试用）和迁出的不动。
func TestDropStale(t *testing.T) {
	home := t.TempDir()
	link, _ := crypto.NewLink("127.0.0.1", 1)
	cur := "in-" + link.Token
	for _, p := range []string{"work/in-old/bundle", "work/" + cur + "/bundle", "work/out-abc/bundle",
		"state/in-old.json", "state/" + cur + ".json", "state/out-stopped.json"} {
		full := filepath.Join(home, p)
		os.MkdirAll(filepath.Dir(full), 0o700)
		os.WriteFile(full, []byte("x"), 0o600)
	}
	r := &run{Deps: Deps{Home: home}, opts: Options{Link: link}}
	r.dropStale()
	for p, keep := range map[string]bool{"work/in-old": false, "state/in-old.json": false, "work/" + cur: true,
		"state/" + cur + ".json": true, "work/out-abc": true, "state/out-stopped.json": true} {
		if _, err := os.Stat(filepath.Join(home, p)); (err == nil) != keep {
			t.Errorf("%s：留着=%v，应为 %v", p, err == nil, keep)
		}
	}
}
