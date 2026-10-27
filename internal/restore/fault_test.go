package restore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/ui"
)

// faultyDocker 是一个假 Docker：handle 决定每个请求怎么回答（返回 false 时回答 500），
// 记下收到的每个请求。
func faultyDocker(t *testing.T, handle func(w http.ResponseWriter, r *http.Request) bool) (*docker.Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			w.Write([]byte(`{"Version":"29.0.0","ApiVersion":"1.44"}`))
			return
		}
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if !handle(w, r) {
			http.Error(w, `{"message":"模拟的 Docker 内部错误"}`, http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	dc, err := docker.New(context.Background(), docker.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")))
	if err != nil {
		t.Fatal(err)
	}
	return dc, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
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

// 读不到已有的 Compose 容器里有没有改动时，不能当作没有改动、直接删掉它。
func TestBackupProjectStopsWhenChangesFail(t *testing.T) {
	dc, calls := faultyDocker(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/changes"):
			return false
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json"):
			w.Write([]byte(`{"Id":"abcdef1234567890","Name":"/blog-app-1","State":{"Status":"running","Running":true},"Config":{"Image":"alpine"}}`))
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusNoContent)
		default:
			return false
		}
		return true
	})
	var out bytes.Buffer
	j, err := openJournal(filepath.Join(t.TempDir(), "rb"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.close()
	r := &run{Deps: Deps{Docker: dc, Screen: ui.New(&out, false, nil)}, j: j}
	err = r.backupProject(context.Background(), "blog", []docker.ContainerSummary{{ID: "abcdef1234567890", Names: []string{"/blog-app-1"}}})
	if err == nil || !strings.Contains(out.String(), "为了安全没有删除它") {
		t.Fatalf("应停下：%v\n%s", err, out.String())
	}
	for _, c := range calls() {
		if strings.HasPrefix(c, "DELETE") {
			t.Fatalf("不应删除旧容器：%s", c)
		}
	}
}

// 跨磁盘移动时复制到一半失败：不能留下一个看起来完整的备份，原件也不能动。
// 否则回滚只看备份在不在，会拿半份备份覆盖完整的原件。
func TestMoveOrCopyPartialFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 能读没有权限的文件，模拟不了复制失败")
	}
	rename = func(string, string) error { return &os.LinkError{Op: "rename", Err: syscall.EXDEV} }
	defer func() { rename = os.Rename }()
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	os.MkdirAll(src, 0o755)
	os.WriteFile(filepath.Join(src, "a.txt"), bytes.Repeat([]byte("a"), 16<<10), 0o644)
	os.WriteFile(filepath.Join(src, "b.txt"), []byte("b"), 0o000) // 读不了：复制到这里失败
	if err := moveOrCopy(src, dst); err == nil {
		t.Fatal("应失败")
	}
	if _, err := os.Lstat(dst); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("失败时不能留下备份：%v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(root, "*partial*")); len(left) > 0 {
		t.Errorf("临时文件没删：%v", left)
	}
	if b, _ := os.ReadFile(filepath.Join(src, "a.txt")); len(b) != 16<<10 {
		t.Fatal("原件不能动")
	}
	// 能读的时候：完整复制过去，删掉原来的。
	os.Chmod(filepath.Join(src, "b.txt"), 0o644)
	if err := moveOrCopy(src, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "a.txt")); len(b) != 16<<10 {
		t.Fatal("备份不完整")
	}
	if _, err := os.Lstat(src); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("原来的应删除")
	}
}

// 每种镜像都要给出能用来建容器的名字，恢复数据卷的临时容器靠它：以前按摘要固定的镜像
// 在线补齐成功后没给名字，只有这种镜像又有数据卷时，迁入在恢复数据卷时失败。
func TestLoadImagesChoosesHelper(t *testing.T) {
	digest := "alpine@sha256:" + strings.Repeat("a", 64)
	cases := []struct {
		name   string
		refs   []string
		pullOK bool
		want   string
	}{
		{"只有按摘要固定的、在线补齐", []string{digest}, true, digest},
		{"只有按摘要固定的、用迁移包里的", []string{digest}, false, "alpine:pinned-aaaaaaaaaaaa"},
		{"混合", []string{"nginx:1.27", digest}, true, digest},
		{"只有普通的", []string{"nginx:1.27"}, true, "nginx:1.27"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dc, _ := faultyDocker(t, func(w http.ResponseWriter, q *http.Request) bool {
				switch {
				case q.Method == "POST" && strings.HasSuffix(q.URL.Path, "/images/load"):
					io.Copy(io.Discard, q.Body)
					w.Write([]byte(`{"stream":"Loaded image"}`))
				case q.Method == "POST" && strings.HasSuffix(q.URL.Path, "/images/create"):
					return c.pullOK
				case q.Method == "GET" && strings.HasSuffix(q.URL.Path, "/json"):
					w.Write([]byte(`{"Id":"sha256:` + strings.Repeat("b", 64) + `"}`))
				case q.Method == "POST" && strings.HasSuffix(q.URL.Path, "/tag"):
					w.WriteHeader(http.StatusCreated)
				default:
					return false
				}
				return true
			})
			refs := map[string]string{}
			for _, ref := range c.refs {
				refs[ref] = "sha256:" + strings.Repeat("b", 64)
			}
			var out bytes.Buffer
			r := &run{Deps: Deps{Docker: dc, Screen: ui.New(&out, false, nil)}, dataDir: t.TempDir(), imageAs: map[string]string{},
				m: &bundle.Manifest{Images: bundle.Images{Refs: refs}}}
			if err := r.loadImages(context.Background()); err != nil {
				t.Fatalf("%v\n%s", err, out.String())
			}
			if r.helperImage != c.want {
				t.Errorf("临时容器用的镜像：%q，期望 %q", r.helperImage, c.want)
			}
		})
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
