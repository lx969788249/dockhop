package restore

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/lx969788249/dockhop/internal/compose"
	"github.com/lx969788249/dockhop/internal/crypto"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/host"
	"github.com/lx969788249/dockhop/internal/logging"
	"github.com/lx969788249/dockhop/internal/preflight"
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

// 迁入的容器启动失败时，它最后的输出会写进日志、接在报错后面。输出里带着环境变量里的
// 连接串时，两处都要隐藏（报错会显示在结果里，用户可能整段复制去求助）。
func TestStartupFailureHidesEnvSecrets(t *testing.T) {
	const secret = "postgres://app:only-for-test@db/app"
	dc, _ := faultyDocker(t, func(w http.ResponseWriter, req *http.Request) bool {
		switch req.URL.Path {
		case "/v1.44/containers/json":
			io.WriteString(w, `[]`)
		case "/v1.44/containers/new-web/json":
			io.WriteString(w, `{"Id":"new-web","Name":"/web","State":{"Status":"exited"}}`)
		case "/v1.44/containers/new-web/logs":
			io.WriteString(w, "cannot connect using "+secret)
		default:
			return false
		}
		return true
	})
	root := t.TempDir()
	data := filepath.Join(root, "data")
	os.MkdirAll(filepath.Join(data, "containers"), 0o700)
	os.WriteFile(filepath.Join(data, "containers", "web.json"),
		[]byte(`{"Id":"old-web","Name":"/web","Config":{"Image":"alpine","Env":["DATABASE_URL=`+secret+`"]}}`), 0o600)
	rd := &logging.Redactor{}
	log, err := logging.Open(filepath.Join(root, "logs"), "in", rd)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Finish(false, true)
	var out bytes.Buffer
	r := &run{Deps: Deps{Docker: dc, Screen: ui.New(&out, false, nil), Log: log, Redactor: rd, Home: root, Getenv: func(string) string { return "" }},
		dataDir: data, rbDir: filepath.Join(root, "rollback"), undo: &undo{}, anonVolumes: map[string]bundle.Volume{},
		m: &bundle.Manifest{Containers: []bundle.Container{{Name: "web", Image: "alpine", State: "running"}}}}
	if err := r.prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.j.close()
	readyErr := r.waitReady(context.Background(), "web", "new-web")
	if readyErr == nil || !strings.Contains(readyErr.Error(), "cannot connect using") {
		t.Fatalf("报错里应带着容器最后的输出：%v", readyErr)
	}
	logText, _ := os.ReadFile(log.Path())
	if strings.Contains(readyErr.Error(), "only-for-test") || strings.Contains(string(logText), "only-for-test") {
		t.Fatalf("连接串没有隐藏：报错 %q\n日志 %s", readyErr, logText)
	}
}

// 上次回滚已经重建、启动了原来的 Compose 容器（新的 ID），之后又中断了。再次回滚时，要先
// 按名字找到它、停下，再放回数据；停不下来时不能动数据。
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
			vio: &volumeIO{dc: dc, image: "alpine", helperBin: "/unused"}}
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
	u := &undo{dc: dc, screen: ui.New(&out, false, nil), logf: t.Logf, fail: func(string) bool { return false }}
	// 原来是停着的：只重建，不启动。sidecar 排在前面，检查重建的顺序。
	u.rollback(context.Background(), []Entry{{Op: opComposeOld, Name: "stack", Old: []OldContainer{
		{ID: sideID, Name: "stack-sidecar-1", State: "exited", Raw: raw(sideID, "stack-sidecar-1", "container:"+vpnID)},
		{ID: vpnID, Name: "stack-vpn-1", State: "exited", Raw: raw(vpnID, "stack-vpn-1", "bridge")},
	}}})
	if strings.Join(created, ",") != "stack-vpn-1,stack-sidecar-1" || sideMode != "container:stack-vpn-1" {
		t.Fatalf("重建顺序 %v，sidecar 的网络 %q", created, sideMode)
	}
}

// prepareWith 用要写入目录 shared 的容器 new-app（和一个不用数据的 kept）调用 prepare。新服务器
// 上的已有容器由 handle 回答（shared 是那个目录的完整路径）。返回屏幕上的输出（prepare 失败时
// 有原因）和停过的容器。
func prepareWith(t *testing.T, d preflight.Decisions, handle func(w http.ResponseWriter, p, shared string) bool) (string, []string) {
	t.Helper()
	home := t.TempDir()
	shared := filepath.Join(home, "shared")
	var stopped []string
	dc, _ := faultyDocker(t, func(w http.ResponseWriter, req *http.Request) bool {
		p := strings.TrimPrefix(req.URL.Path, "/v1.44")
		if strings.HasSuffix(p, "/stop") {
			stopped = append(stopped, p)
			w.WriteHeader(204)
			return true
		}
		return handle(w, p, shared)
	})
	data := filepath.Join(home, "unpacked")
	os.MkdirAll(filepath.Join(data, "containers"), 0o700)
	os.WriteFile(filepath.Join(data, "containers", "new-app.json"),
		[]byte(`{"Id":"source","Name":"/new-app","Config":{"Image":"alpine"},"Mounts":[{"Type":"bind","Source":"`+shared+`","Destination":"/data"}]}`), 0o600)
	os.WriteFile(filepath.Join(data, "containers", "kept.json"), []byte(`{"Id":"source-kept","Name":"/kept","Config":{"Image":"alpine"}}`), 0o600)
	var out bytes.Buffer
	screen := ui.New(&out, false, nil)
	r := &run{Deps: Deps{Docker: dc, Home: home, Screen: screen, Getenv: func(string) string { return "" }}, d: d,
		m:       &bundle.Manifest{Containers: []bundle.Container{{Name: "kept"}, {Name: "new-app"}}, Binds: []bundle.Bind{{Path: shared}}},
		dataDir: data, rbDir: filepath.Join(home, "rollback", "current"), anonVolumes: map[string]bundle.Volume{},
		undo: &undo{dc: dc, screen: screen, logf: t.Logf, fail: func(string) bool { return false }}}
	err := r.prepare(context.Background())
	if r.j != nil {
		r.j.close()
	}
	if err == nil {
		return "", stopped
	}
	return out.String(), stopped
}

// 选择保留的已有容器（停着的也算）和要迁入的容器共用目录：不能覆盖它的数据，改动之前停下。
func TestPrepareProtectsKeptData(t *testing.T) {
	failed, stopped := prepareWith(t, preflight.Decisions{SkipContainers: []string{"kept"}}, func(w http.ResponseWriter, p, shared string) bool {
		switch p {
		case "/containers/json":
			io.WriteString(w, `[{"Id":"kept-id","Names":["/kept"],"State":"exited"}]`)
		case "/containers/kept-id/json":
			io.WriteString(w, `{"Id":"kept-id","Name":"/kept","Mounts":[{"Type":"bind","Source":"`+shared+`"}]}`)
		default:
			return false
		}
		return true
	})
	if !strings.Contains(failed, "保留的已有容器 kept") || len(stopped) > 0 {
		t.Fatalf("应在改动之前停下：%q，停过 %v", failed, stopped)
	}
}

// 正在运行的已有容器读不到配置：没法确认它有没有在用要写入的数据，不能继续；
// Docker 明确说容器不存在（刚好被删掉）时照常继续。
func TestPrepareUnreadableContainer(t *testing.T) {
	for _, code := range []int{500, 404} {
		failed, _ := prepareWith(t, preflight.Decisions{}, func(w http.ResponseWriter, p, _ string) bool {
			switch p {
			case "/containers/json":
				io.WriteString(w, `[{"Id":"other-id","Names":["/other"],"State":"running"}]`)
			case "/containers/other-id/json":
				http.Error(w, `{"message":"读不到"}`, code)
			default:
				return false
			}
			return true
		})
		if strings.Contains(failed, "没法确认它有没有在用") != (code == 500) || code == 404 && failed != "" {
			t.Errorf("Docker 回答 %d 时：%q", code, failed)
		}
	}
}

// 同一个服务一个副本在运行、一个停着：docker compose start web 会把两个都启动，所以要按
// 容器只启动原来在运行的那个；它依赖的 db 先启动、就绪后再启动它。
func TestStartProjectKeepsReplicaStates(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	os.MkdirAll(bin, 0o700)
	cmdlog := filepath.Join(home, "compose.log")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_COMPOSE_LOG", cmdlog)
	os.WriteFile(filepath.Join(bin, "docker"), []byte(`#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_COMPOSE_LOG"
case " $* " in
 *" version "*) echo 2.38.0 ;;
 *" config "*) echo '{"services":{"web":{"image":"alpine","scale":2,"depends_on":{"db":{"condition":"service_healthy"}}},"db":{"image":"alpine"}}}' ;;
esac
`), 0o700)
	tool, err := compose.Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var started []string
	dc, _ := faultyDocker(t, func(w http.ResponseWriter, req *http.Request) bool {
		p := strings.TrimPrefix(req.URL.Path, "/v1.44")
		switch {
		case p == "/networks":
			io.WriteString(w, `[]`)
		case p == "/containers/json":
			io.WriteString(w, `[{"Id":"new-web-1","Labels":{"com.docker.compose.service":"web","com.docker.compose.container-number":"1"}},`+
				`{"Id":"new-web-2","Labels":{"com.docker.compose.service":"web","com.docker.compose.container-number":"2"}},`+
				`{"Id":"new-db-1","Labels":{"com.docker.compose.service":"db","com.docker.compose.container-number":"1"}}]`)
		case strings.HasSuffix(p, "/start"):
			mu.Lock()
			started = append(started, strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/start"))
			mu.Unlock()
			w.WriteHeader(204)
		case strings.HasSuffix(p, "/json"):
			io.WriteString(w, `{"Id":"x","State":{"Status":"running","Running":true},"Config":{"Image":"alpine"}}`)
		default:
			return false
		}
		return true
	})
	member := func(name, svc, number string, running bool) *citem {
		c := &docker.Container{}
		c.Config.Image = "alpine"
		c.Config.Labels = map[string]string{"com.docker.compose.container-number": number}
		c.State.Running = running
		return &citem{m: bundle.Container{Name: name, Project: "stack", Service: svc}, c: c}
	}
	j, err := openJournal(filepath.Join(home, "rollback"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.close()
	var out bytes.Buffer
	r := &run{Deps: Deps{Docker: dc, Screen: ui.New(&out, false, nil), Home: home, Getenv: func(k string) string {
		if k == "DOCKHOP_TEST_STARTUP_GRACE" {
			return "0s"
		}
		return ""
	}}, tool: tool, j: j, workdir: home, items: []*citem{
		member("stack-web-1", "web", "1", true), member("stack-web-2", "web", "2", false), member("stack-db-1", "db", "1", true)}}
	if err := r.startProject(context.Background(), bundle.Project{Name: "stack", WorkingDir: home}); err != nil {
		t.Fatal(err)
	}
	log, _ := os.ReadFile(cmdlog)
	if strings.Join(started, ",") != "new-db-1,new-web-1" || strings.Contains(string(log), " start") {
		t.Fatalf("启动了 %v；compose 命令：\n%s", started, log)
	}
}

// 迁出正在运行（拿着 out.lock）时，它的临时容器在读数据卷：迁入（包括只检查）不能把它
// 当作遗留的删掉。这里是老版本的临时容器（没记是谁的）；只检查时根本不动 Docker。
func TestRestoreKeepsRunningExportHelper(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // 不调用这台机器上真的 docker compose
	for _, checkOnly := range []bool{false, true} {
		home := t.TempDir()
		lock, err := host.TryLock(filepath.Join(home, "locks", "out.lock"))
		if err != nil {
			t.Fatal(err)
		}
		var deleted []string
		dc, _ := faultyDocker(t, func(w http.ResponseWriter, req *http.Request) bool {
			switch {
			case strings.HasSuffix(req.URL.Path, "/containers/json"):
				io.WriteString(w, `[{"Id":"export-helper","Names":["/dockhop-helper-x-1"],"Labels":{"dockhop.helper":"1"}}]`)
			case req.Method == http.MethodDelete:
				deleted = append(deleted, req.URL.Path)
				w.WriteHeader(204)
			default:
				return false
			}
			return true
		})
		link, _ := crypto.NewLink("127.0.0.1", 1)
		file := filepath.Join(home, "broken.bundle")
		os.WriteFile(file, []byte("不是迁移包"), 0o600)
		var out bytes.Buffer
		Run(context.Background(), Deps{Docker: dc, Home: home, Screen: ui.New(&out, false, nil), Prompt: ui.NewPrompter(nil, &out),
			Getenv: func(string) string { return "" }}, Options{Link: link, File: file, CheckOnly: checkOnly})
		lock.Unlock()
		if len(deleted) > 0 {
			t.Errorf("checkOnly=%v：删除了 %v", checkOnly, deleted)
		}
	}
}
