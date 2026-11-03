package restore

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
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
			u := &undo{dc: dc, screen: ui.New(&out, false, nil), logf: t.Logf, fail: func(string) bool { return false }}
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
			ref = strings.TrimSuffix(ref, "/json")
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
			d.tags[req.URL.Query().Get("repo")+":"+req.URL.Query().Get("tag")] = strings.TrimSuffix(ref, "/tag")
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
		case req.Method == "DELETE" && d.tags[ref] != "":
			id := d.tags[ref]
			delete(d.tags, ref)
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

// 回滚时镜像名改回迁入前的样子，只删这次迁入带来的镜像。迁入的镜像和这台服务器上原有的一模
// 一样（例如原来就拉过、另有名字指向它），或者两个镜像名互相换了指向，都不能删原有的镜像：
// 以前改回一个名字就按 ID 删它原来指向的镜像，会删掉原有的镜像，后面的名字也改不回去了。
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
