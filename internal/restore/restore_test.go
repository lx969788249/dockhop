package restore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/docker"
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
	for _, it := range dependencyOrder(items, nameOf) {
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
	if n := len(dependencyOrder([]*citem{a, b}, func(string) string { return "" })); n != 2 {
		t.Errorf("循环依赖：%d", n)
	}
}

func TestHealthTimeout(t *testing.T) {
	s := int64(time.Second)
	if got := healthTimeout(60*time.Second, 0, 2*s, 2*s); got != 60*time.Second {
		t.Errorf("检查间隔短时用默认的 60 秒：%s", got)
	}
	// Vaultwarden：每 60 秒检查一次、超时 10 秒。
	if got := healthTimeout(60*time.Second, 0, 60*s, 10*s); got != 100*time.Second {
		t.Errorf("要够第一次检查跑完：%s", got)
	}
	if got := healthTimeout(60*time.Second, 2000*s, 2000*s, 0); got != time.Hour {
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
