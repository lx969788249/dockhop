package restore

import (
	"bytes"
	"context"
	"encoding/json"
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
