package restore

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/ui"
)

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

// bindStream 把 files（相对挂载目录 dst 的路径 → 内容；以 / 结尾的是目录）写成迁移包里一个挂载目录
// 的数据（编号 b1），返回放数据的目录。mode 是 dst 本身的权限。
func bindStream(t *testing.T, dst string, mode int64, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	base := strings.TrimPrefix(dst, "/")
	tw.WriteHeader(&tar.Header{Name: base + "/", Typeflag: tar.TypeDir, Mode: mode})
	for _, n := range slices.Sorted(maps.Keys(files)) {
		if strings.HasSuffix(n, "/") {
			tw.WriteHeader(&tar.Header{Name: base + "/" + n, Typeflag: tar.TypeDir, Mode: 0o755})
			continue
		}
		tw.WriteHeader(&tar.Header{Name: base + "/" + n, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(files[n]))})
		io.WriteString(tw, files[n])
	}
	tw.Close()
	dir := t.TempDir()
	part := filepath.Join(dir, filepath.FromSlash(bundle.PartName("binds/b1", 1)))
	os.MkdirAll(filepath.Dir(part), 0o755)
	os.WriteFile(part, buf.Bytes(), 0o600)
	return dir
}

func dirNames(dir string) []string {
	var names []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func bindRun(t *testing.T, root, dataDir string) *run {
	t.Helper()
	r := &run{Deps: Deps{Screen: ui.New(io.Discard, false, nil), Home: filepath.Join(root, "home"), Getenv: func(string) string { return "" }},
		dataDir: dataDir}
	j, err := openJournal(filepath.Join(root, "rb"))
	if err != nil {
		t.Fatal(err)
	}
	r.j = j
	return r
}

// 解压用的临时目录在动手之前记进日志：程序被强制结束时它会留下来（在挂载目录旁边、隐藏的，可能
// 很大），下次回滚时要删掉它。
func TestRestoreBindJournalsStage(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "world", "data")
	os.MkdirAll(dst, 0o755)
	r := bindRun(t, root, bindStream(t, dst, 0o755, map[string]string{"new.txt": "migrated"}))
	if err := r.restoreBind(context.Background(), bundle.Bind{ID: "b1", Path: dst, Dir: true, Parts: 1}); err != nil {
		t.Fatal(err)
	}
	r.j.close()
	entries, err := readJournal(filepath.Join(root, "rb"))
	if err != nil {
		t.Fatal(err)
	}
	var ops []string
	for _, e := range entries {
		ops = append(ops, string(e.Op))
	}
	if strings.Join(ops, ",") != "start,stage,bind" || entries[1].Path != siblingName(dst, "stage") {
		t.Fatalf("日志：%+v", entries)
	}
}

// 留下来的临时目录：回滚时删掉，提交后清理回滚资料时也删掉。
func TestStageRemoved(t *testing.T) {
	for _, commit := range []bool{false, true} {
		stage := filepath.Join(t.TempDir(), ".data.dockhop-stage-x")
		os.MkdirAll(filepath.Join(stage, "srv", "data"), 0o700)
		os.WriteFile(filepath.Join(stage, "srv", "data", "big"), []byte("big"), 0o600)
		u := &undo{screen: ui.New(io.Discard, false, nil), logf: t.Logf, fail: func(string) bool { return false }}
		entries := []Entry{{Op: opStage, Path: stage}}
		if commit {
			if left := u.cleanupCommitted(context.Background(), entries); len(left) > 0 {
				t.Fatal(left)
			}
		} else if problems := u.rollback(context.Background(), entries); len(problems) > 0 {
			t.Fatal(problems)
		}
		if _, err := os.Lstat(stage); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("提交=%v：临时目录应删掉", commit)
		}
	}
}

// 挂载目录本身是单独挂载的磁盘时不能改名（Linux 会报“设备忙”），在原地换内容：迁移过来的解在
// 它里面（同一块盘上），原来的挪进它里面的备份目录，目录本身的权限换成迁移过来的。回滚后和
// 原来完全一样，再回滚一次也一样。
func TestRestoreBindIntoMountPoint(t *testing.T) {
	root := t.TempDir()
	world := filepath.Join(root, "world")
	dst := filepath.Join(world, "data")
	os.MkdirAll(filepath.Join(dst, "old-sub"), 0o755)
	os.WriteFile(filepath.Join(dst, "keep.txt"), []byte("old data"), 0o600)
	os.WriteFile(filepath.Join(dst, "same.txt"), []byte("old same"), 0o600)
	os.Chmod(dst, 0o750)
	before := snapshotTree(t, world)
	saved := isMountPoint
	isMountPoint = func(p string) bool { return p == dst }
	t.Cleanup(func() { isMountPoint = saved })

	r := bindRun(t, root, bindStream(t, dst, 0o700, map[string]string{"same.txt": "migrated", "new-sub/": ""}))
	if err := r.restoreBind(context.Background(), bundle.Bind{ID: "b1", Path: dst, Dir: true, Parts: 1}); err != nil {
		t.Fatal(err)
	}
	backups, _ := filepath.Glob(filepath.Join(dst, ".dockhop-old-*"))
	b, _ := os.ReadFile(filepath.Join(dst, "same.txt"))
	st, _ := os.Stat(dst)
	if len(backups) != 1 || len(dirNames(dst)) != 3 || string(b) != "migrated" || st.Mode().Perm() != 0o700 {
		t.Fatalf("应在原地换内容：%v，same.txt=%q，权限 %v", dirNames(dst), b, st.Mode())
	}
	if kept := dirNames(backups[0]); strings.Join(kept, ",") != "keep.txt,old-sub,same.txt" {
		t.Fatalf("原来的内容应挪进备份目录：%v", kept)
	}
	r.j.close()
	entries, err := readJournal(filepath.Join(root, "rb"))
	if err != nil {
		t.Fatal(err)
	}
	u := &undo{screen: r.Screen, logf: t.Logf, fail: func(string) bool { return false }}
	for i := 1; i <= 2; i++ {
		if problems := u.rollback(context.Background(), entries); len(problems) > 0 {
			t.Fatalf("第 %d 次回滚：%v", i, problems)
		}
		if after := snapshotTree(t, world); after != before {
			t.Fatalf("第 %d 次回滚后和原来不一样：\n原来：\n%s\n现在：\n%s", i, before, after)
		}
	}
}

// 原地换内容做到一半、或者回滚做到一半被打断时，再回滚也分得清哪些是原来的：日志里记着原来有
// 哪些东西，名字不在里面的、或者原来的那个已经挪进备份的，才是迁移过来的。
func TestUndoContentsResumes(t *testing.T) {
	want := map[string]string{"a": "old a", "same": "old same"}
	for _, c := range []struct {
		name            string
		inDst, inBackup map[string]string
	}{
		{"原来的挪了一半", map[string]string{"a": "old a"}, map[string]string{"same": "old same"}},
		{"迁移过来的放了一半", map[string]string{"same": "new same", "c": "new c"}, map[string]string{"a": "old a", "same": "old same"}},
		{"已经放回去了", want, nil},
	} {
		dst := filepath.Join(t.TempDir(), "data")
		backup := filepath.Join(dst, ".dockhop-old-x")
		os.MkdirAll(dst, 0o700)
		for n, s := range c.inDst {
			os.WriteFile(filepath.Join(dst, n), []byte(s), 0o600)
		}
		if c.inBackup != nil {
			os.Mkdir(backup, 0o750)
			for n, s := range c.inBackup {
				os.WriteFile(filepath.Join(backup, n), []byte(s), 0o600)
			}
		}
		u := &undo{screen: ui.New(io.Discard, false, nil), logf: t.Logf, fail: func(string) bool { return false }}
		e := Entry{Op: opBind, Path: dst, Existed: true, Backup: backup, Names: []string{"a", "same"}}
		for i := 1; i <= 2; i++ {
			if problems := u.rollback(context.Background(), []Entry{e}); len(problems) > 0 {
				t.Fatalf("%s，第 %d 次回滚：%v", c.name, i, problems)
			}
		}
		got := map[string]string{}
		for _, n := range dirNames(dst) {
			b, _ := os.ReadFile(filepath.Join(dst, n))
			got[n] = string(b)
		}
		if !maps.Equal(got, want) {
			t.Errorf("%s：回滚后是 %v", c.name, got)
		}
		if st, _ := os.Stat(dst); c.inBackup != nil && st.Mode().Perm() != 0o750 {
			t.Errorf("%s：目录本身的权限应改回原来的：%v", c.name, st.Mode())
		}
	}
}

// 回滚时 Docker 出错、没法确认新建的容器已经删掉：不能接着动数据（它可能还在写）。
func TestRollbackStopsWhenDockerErrors(t *testing.T) {
	dc, calls := faultyDocker(t, func(http.ResponseWriter, *http.Request) bool { return false })
	root := t.TempDir()
	dir, backup := filepath.Join(root, "data"), filepath.Join(root, ".data.old")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("migrated"), 0o644)
	os.MkdirAll(backup, 0o755)
	os.WriteFile(filepath.Join(backup, "old.txt"), []byte("original"), 0o644)
	var out bytes.Buffer
	u := &undo{dc: dc, screen: ui.New(&out, false, nil), logf: t.Logf, fail: func(string) bool { return false }}
	problems := u.rollback(context.Background(), []Entry{
		{Op: opStart},
		{Op: opBind, Path: dir, Existed: true, Backup: backup},
		{Op: opContainer, Name: "web"},
	})
	if len(problems) == 0 || !strings.Contains(strings.Join(problems, "；"), "没法确认") {
		t.Fatalf("应报告没法确认新容器已删除：%v", problems)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "new.txt")); string(b) != "migrated" {
		t.Error("不应动数据")
	}
	if _, err := os.Stat(filepath.Join(backup, "old.txt")); err != nil {
		t.Error("备份应保留")
	}
	for _, c := range calls() {
		if strings.HasPrefix(c, "DELETE") {
			t.Errorf("不应删除任何东西：%s", c)
		}
	}
}

// sharedWriters 模拟提交时恢复临时停下的、共用数据的容器：b 恢复成功（又在运行了），
// a 恢复失败，于是整体回滚。stopOK 决定回滚时能不能把 b 停下。
func sharedWriters(t *testing.T, stopOK bool) (r *run, dataReplacedWhileRunning func() bool, dst string) {
	root := t.TempDir()
	dst, backup := filepath.Join(root, "data"), filepath.Join(root, "backup")
	os.Mkdir(dst, 0o700)
	os.Mkdir(backup, 0o700)
	os.WriteFile(filepath.Join(dst, "state"), []byte("new"), 0o600)
	os.WriteFile(filepath.Join(backup, "state"), []byte("old"), 0o600)
	var mu sync.Mutex
	runningB, replaced, replacedWhileRunning := false, false, false
	dc, _ := faultyDocker(t, func(w http.ResponseWriter, q *http.Request) bool {
		mu.Lock()
		defer mu.Unlock()
		// 数据是在两次请求之间被换掉的：第一次看到旧数据时，b 的状态就是换数据那一刻的状态。
		if b, _ := os.ReadFile(filepath.Join(dst, "state")); string(b) == "old" && !replaced {
			replaced, replacedWhileRunning = true, runningB
		}
		switch {
		case strings.HasSuffix(q.URL.Path, "/containers/b/json"):
			w.Write([]byte(`{"Id":"b","Name":"/b","State":{"Status":"running","Running":` + strconv.FormatBool(runningB) + `}}`))
		case strings.HasSuffix(q.URL.Path, "/containers/a/json"):
			w.Write([]byte(`{"Id":"a","Name":"/a","State":{"Status":"exited","Running":false}}`))
		case strings.HasSuffix(q.URL.Path, "/containers/b/start"):
			runningB = true
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(q.URL.Path, "/containers/b/stop") && stopOK:
			runningB = false
			w.WriteHeader(http.StatusNoContent)
		default:
			return false // a 启动失败；stopOK 为 false 时 b 停不下
		}
		return true
	})
	var out bytes.Buffer
	screen := ui.New(&out, false, nil)
	rb := filepath.Join(root, "rb")
	j, err := openJournal(rb)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(j.close)
	shared := []Entry{{Op: opStop, Name: "a", ID: "a", State: "running"}, {Op: opStop, Name: "b", ID: "b", State: "running"}}
	for _, e := range append(shared, Entry{Op: opBind, Path: dst, Existed: true, Backup: backup}) {
		if err := j.add(e); err != nil {
			t.Fatal(err)
		}
	}
	r = &run{Deps: Deps{Docker: dc, Screen: screen}, j: j, rbDir: rb, shared: shared,
		undo: &undo{dc: dc, screen: screen, logf: t.Logf, fail: func(string) bool { return false }}}
	if err := r.commit(context.Background()); err == nil {
		t.Fatal("a 恢复失败时提交应失败")
	}
	return r, func() bool { mu.Lock(); defer mu.Unlock(); return replacedWhileRunning }, dst
}

// 提交时已经重新启动的、共用数据的容器，回滚放回旧数据之前要再停下：不然它会在数据被换掉时继续读写。
func TestRollbackRestopsRestartedSharedWriter(t *testing.T) {
	r, replacedWhileRunning, dst := sharedWriters(t, true)
	r.rollback(context.Background())
	if replacedWhileRunning() {
		t.Error("放回旧数据时共用数据的容器 b 还在运行")
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "state")); string(b) != "old" {
		t.Errorf("应放回旧数据，实际：%q", b)
	}
}

// 停不下那个容器时，不能动数据，回滚资料全部保留。
func TestRollbackKeepsDataWhenWriterWontStop(t *testing.T) {
	r, replacedWhileRunning, dst := sharedWriters(t, false)
	r.rollback(context.Background())
	if replacedWhileRunning() {
		t.Error("容器停不下时不应动数据")
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "state")); string(b) != "new" {
		t.Errorf("数据不应被动过，实际：%q", b)
	}
	if issues := strings.Join(r.rollbackIssues, "；"); !strings.Contains(issues, "没能停下") || !strings.Contains(issues, "没有动数据") {
		t.Errorf("应说明为什么没动数据：%s", issues)
	}
}

// 上次回滚已经重建、启动了原来的 Compose 容器（新的 ID），但没记下来（例如网络没接好），之后
// 又中断了。再次回滚时，要先按名字找到它、停下，再放回数据；停不下来时不能动数据。
func TestRollbackStopsRebuiltOldCompose(t *testing.T) {
	for _, stopOK := range []bool{true, false} {
		running, clearedWhileRunning, cleared := true, false, false
		dc, _ := faultyDocker(t, func(w http.ResponseWriter, req *http.Request) bool {
			p := strings.TrimPrefix(req.URL.Path, "/v1.44")
			switch {
			case p == "/volumes/data":
				io.WriteString(w, `{"Name":"data","Driver":"local"}`)
			case p == "/containers/old-app/json":
				http.Error(w, `{"message":"No such container"}`, 404)
			case p == "/containers/stack-app-1/json":
				io.WriteString(w, `{"Id":"rebuilt-app","Name":"/stack-app-1","State":{"Running":`+strconv.FormatBool(running)+`}}`)
			case p == "/containers/rebuilt-app/stop":
				if !stopOK {
					return false
				}
				running = false
				w.WriteHeader(204)
			case strings.HasPrefix(p, "/images/"):
				io.WriteString(w, `{"Id":"sha256:original"}`)
			case p == "/containers/create":
				if strings.HasPrefix(req.URL.Query().Get("name"), "dockhop-helper-") {
					io.WriteString(w, `{"Id":"helper"}`)
				} else {
					io.WriteString(w, `{"Id":"rebuilt-again"}`)
				}
			case p == "/containers/helper/start":
				cleared = true
				clearedWhileRunning = clearedWhileRunning || running
				w.WriteHeader(204)
			case p == "/containers/helper/wait":
				io.WriteString(w, `{"StatusCode":0}`)
			case p == "/containers/helper/archive":
				io.Copy(io.Discard, req.Body)
			case p == "/containers/rebuilt-again/json":
				io.WriteString(w, `{"Id":"rebuilt-again","State":{"Running":false}}`)
			case req.Method == "DELETE", p == "/containers/rebuilt-again/start":
				w.WriteHeader(204)
			default:
				return false
			}
			return true
		})
		backup := filepath.Join(t.TempDir(), "data.tar")
		f, _ := os.Create(backup)
		tw := tar.NewWriter(f)
		tw.WriteHeader(&tar.Header{Name: "dockhop-data/", Typeflag: tar.TypeDir, Mode: 0o755})
		tw.Close()
		f.Close()
		var out bytes.Buffer
		u := &undo{dc: dc, screen: ui.New(&out, false, nil), logf: t.Logf, fail: func(string) bool { return false },
			record: func(Entry) error { return nil }, vio: &volumeIO{dc: dc, image: "alpine", helperBin: "/unused"}}
		problems := u.rollback(context.Background(), []Entry{
			{Op: opComposeOld, Name: "stack", Old: []OldContainer{{ID: "old-app", Name: "stack-app-1", State: "running",
				Raw: json.RawMessage(`{"Id":"old-app","Name":"/stack-app-1","Image":"sha256:original","Config":{"Image":"alpine"},"HostConfig":{}}`)}}},
			{Op: opVolume, Name: "data", Existed: true, Backup: backup},
		})
		switch {
		case clearedWhileRunning:
			t.Fatalf("stopOK=%v：重建过的原容器还在运行，就放回了数据卷", stopOK)
		case stopOK && (len(problems) > 0 || !cleared):
			t.Fatalf("应该停下它、放回数据、再重建：%v", problems)
		case !stopOK && (cleared || len(problems) == 0):
			t.Fatalf("停不下来时不能动数据，要报告：cleared=%v %v", cleared, problems)
		}
	}
}

// 回滚重建、启动了原来的 Compose 容器之后，还没删除回滚资料就被结束（比如断电）。下次再回滚
// 要认得重建的容器：它就是原来的服务，这期间可能已经运行、写过东西，不能当成这次迁入的新容器
// 删掉再重建。
func TestRollbackAgainKeepsRebuiltOldCompose(t *testing.T) {
	type ctr struct {
		name    string
		running bool
	}
	var mu sync.Mutex
	ctrs := map[string]*ctr{"new-app": {name: "stack-app-1", running: true}} // 这次迁入的新容器
	var removed, created []string
	find := func(ref string) (string, *ctr) {
		for id, c := range ctrs {
			if id == ref || c.name == ref {
				return id, c
			}
		}
		return "", nil
	}
	dc, _ := faultyDocker(t, func(w http.ResponseWriter, req *http.Request) bool {
		mu.Lock()
		defer mu.Unlock()
		p := strings.TrimPrefix(req.URL.Path, "/v1.44")
		ref, _, _ := strings.Cut(strings.TrimPrefix(p, "/containers/"), "/")
		switch {
		case p == "/containers/json": // 按 Compose 项目列出：这里的容器都是这个项目的
			var list []map[string]any
			for id, c := range ctrs {
				list = append(list, map[string]any{"Id": id, "Names": []string{"/" + c.name}})
			}
			json.NewEncoder(w).Encode(list)
		case p == "/containers/create":
			id := fmt.Sprintf("rebuilt-%d", len(created)+1)
			created = append(created, req.URL.Query().Get("name"))
			ctrs[id] = &ctr{name: req.URL.Query().Get("name")}
			fmt.Fprintf(w, `{"Id":%q}`, id)
		case strings.HasPrefix(p, "/containers/"):
			id, c := find(ref)
			switch {
			case c == nil:
				http.Error(w, `{"message":"No such container"}`, 404)
			case req.Method == "GET":
				fmt.Fprintf(w, `{"Id":%q,"Name":"/%s","State":{"Running":%v}}`, id, c.name, c.running)
			case req.Method == "DELETE":
				removed = append(removed, id)
				delete(ctrs, id)
				w.WriteHeader(204)
			default: // start、stop
				c.running = strings.HasSuffix(p, "/start")
				w.WriteHeader(204)
			}
		case strings.HasPrefix(p, "/images/"):
			io.WriteString(w, `{"Id":"sha256:original"}`)
		case p == "/networks":
			io.WriteString(w, `[]`)
		default:
			return false
		}
		return true
	})
	j, err := openJournal(filepath.Join(t.TempDir(), "rb"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []Entry{
		{Op: opComposeOld, Name: "stack", Old: []OldContainer{{ID: "old-app", Name: "stack-app-1", State: "running",
			Raw: json.RawMessage(`{"Id":"old-app","Name":"/stack-app-1","Image":"sha256:original","Config":{"Image":"alpine"},"HostConfig":{}}`)}}},
		{Op: opOldImages, Name: "stack"},
		{Op: opComposeUp, Name: "stack"},
	} {
		j.add(e)
	}
	j.close()
	var out bytes.Buffer
	u := &undo{dc: dc, screen: ui.New(&out, false, nil), logf: t.Logf, fail: func(string) bool { return false },
		record: func(e Entry) error { return appendJournal(j.dir, e) }}
	for i := 1; i <= 2; i++ { // 第二次：上次回滚做完了，回滚资料还没删
		entries, err := readJournal(j.dir)
		if err != nil {
			t.Fatal(err)
		}
		if problems := u.rollback(context.Background(), entries); len(problems) > 0 {
			t.Fatalf("第 %d 次回滚：%v", i, problems)
		}
	}
	id, c := find("stack-app-1")
	if strings.Join(removed, ",") != "new-app" || len(created) != 1 || c == nil || !c.running {
		t.Fatalf("删掉了 %v，重建了 %v，现在的 stack-app-1：%s %+v", removed, created, id, c)
	}
}

// 重建的原来的 Compose 容器没能记进事务日志（比如磁盘满了），不算回滚失败：原来的服务已经回来了。
// 算失败的话回滚资料会留下，下次运行又回滚一遍，反而会把它删掉重建。
func TestRollbackRebuiltNotRecorded(t *testing.T) {
	dc, _ := faultyDocker(t, func(w http.ResponseWriter, req *http.Request) bool {
		p := strings.TrimPrefix(req.URL.Path, "/v1.44")
		switch {
		case p == "/containers/old-app/json", p == "/containers/stack-app-1/json":
			http.Error(w, `{"message":"No such container"}`, 404)
		case strings.HasPrefix(p, "/images/"):
			io.WriteString(w, `{"Id":"sha256:original"}`)
		case p == "/containers/create":
			io.WriteString(w, `{"Id":"rebuilt"}`)
		case p == "/containers/rebuilt/json":
			io.WriteString(w, `{"Id":"rebuilt","State":{"Running":false}}`)
		case p == "/containers/rebuilt/start":
			w.WriteHeader(204)
		default:
			return false
		}
		return true
	})
	var out bytes.Buffer
	u := &undo{dc: dc, screen: ui.New(&out, false, nil), logf: t.Logf, fail: func(string) bool { return false },
		record: func(Entry) error { return errors.New("磁盘满了") }}
	problems := u.rollback(context.Background(), []Entry{{Op: opComposeOld, Name: "stack", Old: []OldContainer{{ID: "old-app", Name: "stack-app-1", State: "running",
		Raw: json.RawMessage(`{"Id":"old-app","Name":"/stack-app-1","Image":"sha256:original","Config":{"Image":"alpine"},"HostConfig":{}}`)}}}})
	if len(problems) > 0 {
		t.Fatalf("原来的服务已经回来了，不该算回滚失败：%v", problems)
	}
}

// 放回数据卷之后备份就删掉：上次回滚放回了数据、还没删除回滚资料就被结束时，再回滚不会把这之后
// 写进卷里的数据又改回去。
func TestRollbackRestoresVolumeOnce(t *testing.T) {
	writes := 0
	dc, _ := faultyDocker(t, func(w http.ResponseWriter, req *http.Request) bool {
		p := strings.TrimPrefix(req.URL.Path, "/v1.44")
		switch {
		case p == "/volumes/data":
			io.WriteString(w, `{"Name":"data","Driver":"local"}`)
		case p == "/containers/create":
			io.WriteString(w, `{"Id":"helper"}`)
		case p == "/containers/helper/wait":
			io.WriteString(w, `{"StatusCode":0}`)
		case p == "/containers/helper/archive":
			writes++
			io.Copy(io.Discard, req.Body)
		case req.Method == "DELETE", p == "/containers/helper/start":
			w.WriteHeader(204)
		default:
			return false
		}
		return true
	})
	backup := filepath.Join(t.TempDir(), "volume-0001")
	f, _ := os.Create(backup)
	tw := tar.NewWriter(f)
	tw.WriteHeader(&tar.Header{Name: "dockhop-data/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.Close()
	f.Close()
	var out bytes.Buffer
	u := &undo{dc: dc, screen: ui.New(&out, false, nil), logf: t.Logf, fail: func(string) bool { return false },
		vio: &volumeIO{dc: dc, image: "alpine", helperBin: "/unused"}}
	for i := 1; i <= 2; i++ {
		if problems := u.rollback(context.Background(), []Entry{{Op: opVolume, Name: "data", Existed: true, Backup: backup}}); len(problems) > 0 {
			t.Fatalf("第 %d 次回滚：%v", i, problems)
		}
	}
	if writes != 1 {
		t.Fatalf("数据卷放回了 %d 次", writes)
	}
}

// 原来的 Compose 项目里，sidecar 借用 vpn 的网络（container:<vpn 的 ID>）。两个都删除后
// 回滚重建：vpn 要先建，sidecar 要改为按名字引用它（旧 ID 已经不存在了）。
func TestRollbackOldComposeNamespace(t *testing.T) {
	vpnID, sideID := strings.Repeat("a", 64), strings.Repeat("b", 64)
	var created []string
	sideMode := ""
	dc, _ := faultyDocker(t, func(w http.ResponseWriter, req *http.Request) bool {
		p := strings.TrimPrefix(req.URL.Path, "/v1.44")
		switch {
		case strings.HasPrefix(p, "/images/"):
			io.WriteString(w, `{"Id":"sha256:original"}`)
		case req.Method == "GET" && strings.HasPrefix(p, "/containers/"):
			http.Error(w, `{"message":"No such container"}`, 404)
		case p == "/containers/create":
			var body struct{ HostConfig struct{ NetworkMode string } }
			json.NewDecoder(req.Body).Decode(&body)
			name := req.URL.Query().Get("name")
			created = append(created, name)
			if name == "stack-sidecar-1" {
				sideMode = body.HostConfig.NetworkMode
			}
			io.WriteString(w, `{"Id":"new-`+name+`"}`)
		default:
			return false
		}
		return true
	})
	raw := func(id, name, mode string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"Id": id, "Name": "/" + name, "Image": "sha256:original",
			"Config": map[string]any{"Image": "alpine"}, "HostConfig": map[string]any{"NetworkMode": mode}})
		return b
	}
	var out bytes.Buffer
	u := &undo{dc: dc, screen: ui.New(&out, false, nil), logf: t.Logf, fail: func(string) bool { return false },
		record: func(Entry) error { return nil }}
	// 原来是停着的：只重建，不启动。sidecar 排在前面，检查重建的顺序。
	u.rollback(context.Background(), []Entry{{Op: opComposeOld, Name: "stack", Old: []OldContainer{
		{ID: sideID, Name: "stack-sidecar-1", State: "exited", Raw: raw(sideID, "stack-sidecar-1", "container:"+vpnID)},
		{ID: vpnID, Name: "stack-vpn-1", State: "exited", Raw: raw(vpnID, "stack-vpn-1", "bridge")},
	}}})
	if strings.Join(created, ",") != "stack-vpn-1,stack-sidecar-1" || sideMode != "container:stack-vpn-1" {
		t.Fatalf("重建顺序 %v，sidecar 的网络 %q", created, sideMode)
	}
}

// 回滚把改名备份的旧容器改回原名后、接回网络前被中断：再回滚一次时它已经叫回原名了，
// 也要把网络（和固定 IP）接回去。记下了还没改名的，网络本来就在，不用重复接。
func TestUndoRenameResumes(t *testing.T) {
	for _, connected := range []bool{false, true} {
		var connects []string
		dc, _ := faultyDocker(t, func(w http.ResponseWriter, req *http.Request) bool {
			p := strings.TrimPrefix(req.URL.Path, "/v1.44")
			switch {
			case p == "/containers/web.dockhop-backup-x/json":
				http.Error(w, `{"message":"No such container"}`, 404)
			case p == "/containers/web/json", p == "/containers/old-web/json":
				nets := `{}`
				if connected {
					nets = `{"front":{}}`
				}
				io.WriteString(w, `{"Id":"old-web","Name":"/web","NetworkSettings":{"Networks":`+nets+`}}`)
			case strings.HasSuffix(p, "/connect"):
				b, _ := io.ReadAll(req.Body)
				connects = append(connects, p+" "+string(b))
				w.WriteHeader(200)
			default:
				return false
			}
			return true
		})
		var out bytes.Buffer
		u := &undo{dc: dc, screen: ui.New(&out, false, nil), logf: t.Logf, fail: func(string) bool { return false }}
		problems := u.rollback(context.Background(), []Entry{{Op: opRename, Name: "web", ID: "old-web", Backup: "web.dockhop-backup-x",
			Networks: json.RawMessage(`{"front":{"IPAMConfig":{"IPv4Address":"172.30.9.10"},"Aliases":["web"]}}`)}})
		if len(problems) > 0 {
			t.Fatalf("回滚有问题：%v", problems)
		}
		switch {
		case connected && len(connects) > 0:
			t.Errorf("已经接着的网络不用重复接：%v", connects)
		case !connected && (len(connects) != 1 || !strings.Contains(connects[0], "172.30.9.10")):
			t.Errorf("应把网络和固定 IP 接回去：%v", connects)
		}
	}
}

// 回滚时镜像名改回迁入前指向的镜像，原来没有的名字删掉；迁入进来、已经没有名字也没有容器在
// 用的镜像跟着删掉。
func TestUndoTag(t *testing.T) {
	var tagged, removed []string
	dc, _ := faultyDocker(t, func(w http.ResponseWriter, req *http.Request) bool {
		p := strings.TrimPrefix(req.URL.Path, "/v1.44")
		switch {
		case req.Method == "GET" && p == "/images/nginx:1.27/json":
			io.WriteString(w, `{"Id":"sha256:migrated"}`)
		case req.Method == "GET" && p == "/images/app:dev/json":
			io.WriteString(w, `{"Id":"sha256:migrated-app"}`)
		case req.Method == "GET" && p == "/images/same:1/json":
			io.WriteString(w, `{"Id":"sha256:same"}`)
		case req.Method == "GET" && p == "/images/sha256:migrated/json":
			io.WriteString(w, `{"Id":"sha256:migrated","RepoTags":[]}`) // 名字改回去之后，它没有名字了
		case req.Method == "POST" && strings.HasSuffix(p, "/tag"):
			tagged = append(tagged, p+"?"+req.URL.RawQuery)
			w.WriteHeader(201)
		case req.Method == "DELETE":
			removed = append(removed, p)
			io.WriteString(w, `[]`)
		default:
			return false
		}
		return true
	})
	var out bytes.Buffer
	u := &undo{dc: dc, screen: ui.New(&out, false, nil), logf: t.Logf, fail: func(string) bool { return false }}
	problems := u.rollback(context.Background(), []Entry{
		{Op: opTag, Name: "nginx:1.27", ID: "sha256:original"}, // 原来指向别的镜像
		{Op: opTag, Name: "app:dev"},                           // 原来没有这个名字
		{Op: opTag, Name: "same:1", ID: "sha256:same"},         // 没来得及改
	})
	if len(problems) > 0 {
		t.Fatalf("回滚有问题：%v", problems)
	}
	if strings.Join(tagged, ",") != "/images/sha256:original/tag?repo=nginx&tag=1.27" {
		t.Errorf("改回原来的镜像：%v", tagged)
	}
	if strings.Join(removed, ",") != "/images/app:dev,/images/sha256:migrated" {
		t.Errorf("删掉的：%v", removed)
	}
}

// 原来的两个 Compose 项目：zside 的 sidecar 借用 avpn 的 vpn 的网络。回滚时不管它们在日志里谁先
// 谁后，都要先重建、启动 vpn，再重建、启动 sidecar：Docker 要求被借用网络的容器先存在、先在运行。
func TestRollbackRestoresAcrossProjects(t *testing.T) {
	vpnID, sideID := strings.Repeat("a", 64), strings.Repeat("b", 64)
	raw := func(id, name, mode string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"Id": id, "Name": "/" + name, "Image": "sha256:original",
			"Config": map[string]any{"Image": "alpine"}, "HostConfig": map[string]any{"NetworkMode": mode}})
		return b
	}
	old := map[string]OldContainer{
		"avpn":  {ID: vpnID, Name: "avpn-vpn-1", State: "running", Raw: raw(vpnID, "avpn-vpn-1", "bridge")},
		"zside": {ID: sideID, Name: "zside-sidecar-1", State: "running", Raw: raw(sideID, "zside-sidecar-1", "container:"+vpnID)},
	}
	for _, order := range [][]string{{"avpn", "zside"}, {"zside", "avpn"}} {
		t.Run(strings.Join(order, "-"), func(t *testing.T) {
			type box struct {
				mode    string
				running bool
			}
			live := map[string]*box{} // 名字 → 新服务器上的容器
			var events []string
			dc, _ := faultyDocker(t, func(w http.ResponseWriter, req *http.Request) bool {
				p := strings.TrimPrefix(req.URL.Path, "/v1.44")
				ref := strings.Split(strings.TrimPrefix(p, "/containers/"), "/")[0]
				switch {
				case strings.HasPrefix(p, "/images/"):
					io.WriteString(w, `{"Id":"sha256:original"}`)
				case p == "/containers/create":
					var body struct{ HostConfig struct{ NetworkMode string } }
					json.NewDecoder(req.Body).Decode(&body)
					if peer, ok := strings.CutPrefix(body.HostConfig.NetworkMode, "container:"); ok && live[peer] == nil {
						http.Error(w, `{"message":"No such container: `+peer+`"}`, 404)
						return true
					}
					name := req.URL.Query().Get("name")
					live[name] = &box{mode: body.HostConfig.NetworkMode}
					events = append(events, "建 "+name)
					io.WriteString(w, `{"Id":"`+name+`"}`)
				case live[ref] == nil:
					http.Error(w, `{"message":"No such container"}`, 404)
				case req.Method == "GET":
					fmt.Fprintf(w, `{"Id":%q,"Name":"/%s","State":{"Running":%t}}`, ref, ref, live[ref].running)
				case strings.HasSuffix(p, "/start"):
					if peer, ok := strings.CutPrefix(live[ref].mode, "container:"); ok && (live[peer] == nil || !live[peer].running) {
						http.Error(w, `{"message":"cannot join network of a non running container"}`, http.StatusConflict)
						return true
					}
					live[ref].running = true
					events = append(events, "启动 "+ref)
					w.WriteHeader(http.StatusNoContent)
				default:
					return false
				}
				return true
			})
			var entries []Entry
			for _, p := range order {
				entries = append(entries, Entry{Op: opComposeOld, Name: p, Old: []OldContainer{old[p]}})
			}
			var out bytes.Buffer
			u := &undo{dc: dc, screen: ui.New(&out, false, nil), logf: t.Logf, fail: func(string) bool { return false },
				record: func(Entry) error { return nil }}
			problems := u.rollback(context.Background(), entries)
			if len(problems) > 0 || strings.Join(events, "，") != "建 avpn-vpn-1，建 zside-sidecar-1，启动 avpn-vpn-1，启动 zside-sidecar-1" {
				t.Fatalf("顺序：%v；问题：%v", events, problems)
			}
		})
	}
}

// images 是一个按 Docker 的规则管镜像名的假 Docker：按 ID 删镜像时，它只剩一个名字就连名字一起删，
// 有几个名字就拒绝；按名字删时去掉这个名字，镜像没有名字了就一起删。
type images struct {
	mu      sync.Mutex
	tags    map[string]string // 名字 → 镜像 ID
	ids     map[string]bool
	deleted []string
}

func (d *images) docker(t *testing.T) *docker.Client {
	dc, _ := faultyDocker(t, func(w http.ResponseWriter, req *http.Request) bool {
		ref, ok := strings.CutPrefix(strings.TrimPrefix(req.URL.Path, "/v1.44"), "/images/")
		if !ok {
			return false
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		namesOf := func(id string) []string {
			var out []string
			for name, x := range d.tags {
				if x == id {
					out = append(out, name)
				}
			}
			return out
		}
		switch {
		case req.Method == "GET" && ref == "json":
			var list []map[string]string
			for id := range d.ids {
				list = append(list, map[string]string{"Id": id})
			}
			b, _ := json.Marshal(list)
			w.Write(b)
		case req.Method == "GET":
			ref = dockerName(strings.TrimSuffix(ref, "/json"))
			id := d.tags[ref]
			if id == "" && d.ids[ref] {
				id = ref
			}
			if id == "" {
				http.Error(w, `{"message":"No such image"}`, 404)
				return true
			}
			b, _ := json.Marshal(map[string]any{"Id": id, "RepoTags": namesOf(id)})
			w.Write(b)
		case req.Method == "POST" && strings.HasSuffix(ref, "/tag"):
			id := strings.TrimSuffix(ref, "/tag")
			if !d.ids[id] {
				http.Error(w, `{"message":"No such image"}`, 404)
				return true
			}
			d.tags[req.URL.Query().Get("repo")+":"+req.URL.Query().Get("tag")] = id
			w.WriteHeader(201)
		case req.Method == "DELETE" && d.ids[ref]:
			if len(namesOf(ref)) > 1 {
				http.Error(w, `{"message":"conflict: image is referenced in multiple repositories"}`, http.StatusConflict)
				return true
			}
			for _, name := range namesOf(ref) {
				delete(d.tags, name)
			}
			delete(d.ids, ref)
			d.deleted = append(d.deleted, ref)
		case req.Method == "DELETE" && d.tags[dockerName(ref)] != "":
			id := d.tags[dockerName(ref)]
			delete(d.tags, dockerName(ref))
			if len(namesOf(id)) == 0 {
				delete(d.ids, id)
				d.deleted = append(d.deleted, id)
			}
		default:
			return false
		}
		return true
	})
	return dc
}

// dockerName 是 Docker 认的镜像名：没写标签的就是 latest，app 和 app:latest 是同一个名字。
func dockerName(ref string) string {
	if strings.Contains(ref, "@") || strings.Contains(ref[strings.LastIndex(ref, "/")+1:], ":") {
		return ref
	}
	return ref + ":latest"
}

// 回滚时镜像名改回迁入前的样子，只删这次迁入带来的镜像。迁入的镜像和这台服务器上原有的一模
// 一样（例如原来就拉过、另有名字指向它），或者两个镜像名互相换了指向，都不能删原有的镜像：
// 以前改回一个名字就按 ID 删它原来指向的镜像，会删掉原有的镜像，后面的名字也改不回去了。
// 删掉镜像最后一个名字时 Docker 会连镜像一起删，所以原来没有的名字也一样：先删它会删掉后面
// 要改回去的名字正要用的镜像；它是原有的、本来没有名字的镜像仅剩的名字时，删了镜像就没了。
func TestRollbackKeepsImagesThatWereThere(t *testing.T) {
	cases := []struct {
		name    string
		had     []string          // 加载镜像之前已有的镜像
		now     map[string]string // 回滚开始时的镜像名
		entries []Entry
		want    map[string]string
		deleted string
	}{
		{"迁入的镜像原来就有", []string{"sha256:old", "sha256:cached"},
			map[string]string{"app:1": "sha256:cached", "cache:stable": "sha256:cached"},
			[]Entry{{Op: opTag, Name: "app:1", ID: "sha256:old"}},
			map[string]string{"app:1": "sha256:old", "cache:stable": "sha256:cached"}, ""},
		{"两个名字换了指向", []string{"sha256:A", "sha256:B"},
			map[string]string{"a:1": "sha256:B", "b:1": "sha256:A"},
			[]Entry{{Op: opTag, Name: "a:1", ID: "sha256:A"}, {Op: opTag, Name: "b:1", ID: "sha256:B"}},
			map[string]string{"a:1": "sha256:A", "b:1": "sha256:B"}, ""},
		{"迁入带来的镜像", []string{"sha256:old"},
			map[string]string{"app:1": "sha256:new", "app:latest": "sha256:new"},
			[]Entry{{Op: opTag, Name: "app:1", ID: "sha256:old"}, {Op: opTag, Name: "app:latest"}},
			map[string]string{"app:1": "sha256:old"}, "sha256:new"},
		{"新名字加在要改回去的镜像上", []string{"sha256:Y"},
			map[string]string{"a:1": "sha256:Y", "c:1": "sha256:Z"},
			[]Entry{{Op: opTag, Name: "c:1", ID: "sha256:Y"}, {Op: opTag, Name: "a:1"}},
			map[string]string{"c:1": "sha256:Y"}, "sha256:Z"},
		{"原有的镜像本来没有名字", []string{"sha256:cached"},
			map[string]string{"app:1": "sha256:cached"},
			[]Entry{{Op: opTag, Name: "app:1"}},
			map[string]string{"app:1": "sha256:cached"}, ""},
		{"原有的镜像另有名字", []string{"sha256:cached"},
			map[string]string{"app:1": "sha256:cached", "cache:stable": "sha256:cached"},
			[]Entry{{Op: opTag, Name: "app:1"}},
			map[string]string{"cache:stable": "sha256:cached"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &images{tags: c.now, ids: map[string]bool{}}
			for _, id := range append(slices.Clone(c.had), slices.Collect(maps.Values(c.now))...) {
				d.ids[id] = true
			}
			var out bytes.Buffer
			u := &undo{dc: d.docker(t), screen: ui.New(&out, false, nil), logf: t.Logf, fail: func(string) bool { return false }}
			problems := u.rollback(context.Background(), append([]Entry{{Op: opImages, Names: c.had}}, c.entries...))
			if len(problems) > 0 || !maps.Equal(d.tags, c.want) || strings.Join(d.deleted, ",") != c.deleted {
				t.Fatalf("镜像名 %v，删掉的 %v，问题 %v", d.tags, d.deleted, problems)
			}
		})
	}
}

// 提交之后，被替换的旧容器连同它的匿名卷一起删掉，原有 Compose 容器为了回滚留着的匿名卷也删掉：
// 以前留着，成了没人用的卷。还挂在别的容器上的卷不是没人用的，不删，也不算没删掉的回滚资料。
func TestCleanupRemovesOldAnonymousVolumes(t *testing.T) {
	var deleted []string
	dc, _ := faultyDocker(t, func(w http.ResponseWriter, req *http.Request) bool {
		if req.Method != "DELETE" {
			return false
		}
		p := strings.TrimPrefix(req.URL.Path, "/v1.44")
		if req.URL.RawQuery != "" {
			p += "?" + req.URL.RawQuery
		}
		deleted = append(deleted, p)
		if p == "/volumes/shared" {
			http.Error(w, `{"message":"remove shared: volume is in use - [other]"}`, http.StatusConflict)
			return true
		}
		w.WriteHeader(204)
		return true
	})
	u := &undo{dc: dc, screen: ui.New(io.Discard, false, nil), logf: t.Logf, fail: func(string) bool { return false }}
	entries := []Entry{
		{Op: opRename, Name: "web", ID: "old-web", Backup: "web.dockhop-backup-1"},
		{Op: opComposeOld, Name: "stack", Old: []OldContainer{
			{ID: "old-app", Name: "stack-app-1", Volumes: map[string]string{"/data": "anon1"}},
			{ID: "old-db", Name: "stack-db-1", Volumes: map[string]string{"/var/lib/db": "shared"}}}},
		{Op: opOldImages, Name: "stack", Images: map[string]string{"old-app": "dockhop-rollback:1"}},
	}
	if left := u.cleanupCommitted(context.Background(), entries); len(left) > 0 {
		t.Fatalf("不应有没删掉的：%v", left)
	}
	want := "/containers/old-web?force=1&v=1 /volumes/anon1 /volumes/shared /images/dockhop-rollback:1?noprune=0"
	if got := strings.Join(deleted, " "); got != want {
		t.Fatalf("删掉了：%s\n应为：%s", got, want)
	}
}
