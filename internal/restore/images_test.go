package restore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/crypto"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/docker/dockertest"
	"github.com/lx969788249/dockhop/internal/transfer"
	"github.com/lx969788249/dockhop/internal/ui"
)

// 每个镜像从哪里来：本机已有的直接用；能按摘要拉到的从镜像仓库拉；拉取出错、卡住，或者
// 仓库里查不到、本来就没有仓库摘要（自己构建的）的，从旧服务器下载。下载来的镜像加载进
// Docker 的内容和旧服务器导出的一模一样，用完就删。
func TestImageSources(t *testing.T) {
	pullWindow, pullMinBytes = 300*time.Millisecond, 1<<20
	t.Cleanup(func() { pullWindow, pullMinBytes = time.Minute, 3<<20 })

	// 旧服务器：导出好的镜像文件和下载服务。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	link, _ := crypto.NewLink("127.0.0.1", ln.Addr().(*net.TCPAddr).Port)
	files := []bundle.ImageFile{
		{File: "image-1", ID: "sha256:here", Refs: []string{"local:1"}, Pull: "local@sha256:11"},
		{File: "image-2", ID: "sha256:pulled", Refs: []string{"web:1.25", "web:latest"}, Pull: "web@sha256:22"},
		{File: "image-3", ID: "sha256:limited", Refs: []string{"db:16"}, Pull: "db@sha256:33"},
		{File: "image-4", ID: "sha256:built", Refs: []string{"mine:dev"}},
		{File: "image-5", ID: "sha256:stuck", Refs: []string{"slow:1"}, Pull: "slow@sha256:55"},
		{File: "image-6", ID: "sha256:gone", Refs: []string{"gone:1"}, Pull: "gone@sha256:66"},
	}
	dir := t.TempDir()
	saves := map[string]string{} // 导出的内容 → 文件名
	served := map[string]string{}
	for i := range files {
		f := &files[i]
		data := []byte(f.File + strings.Repeat(" docker save", 20000))
		path := filepath.Join(dir, f.File)
		out, _ := os.Create(path)
		if err := bundle.WriteImageFile(out, link, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		out.Close()
		st, _ := os.Stat(path)
		f.Bytes, f.Size = st.Size(), int64(len(data))
		saves[string(data)] = f.File
		served[f.File] = path
	}
	os.WriteFile(filepath.Join(dir, "bundle"), []byte("bundle"), 0o600)
	srv, err := transfer.NewServer(ln, link, filepath.Join(dir, "bundle"), "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetImages(served)
	go srv.Serve()
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	// 新服务器上的 Docker。
	var mu sync.Mutex
	pulled := map[string]bool{}
	var loaded, tags []string
	fake := dockertest.New(t, func(w http.ResponseWriter, r *http.Request) bool {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/images/sha256:here/json"):
			io.WriteString(w, `{"Id":"sha256:here"}`)
		case strings.HasSuffix(p, "/images/web@sha256:22/json"):
			mu.Lock()
			ok := pulled["web@sha256:22"]
			mu.Unlock()
			if !ok {
				http.Error(w, `{"message":"No such image"}`, http.StatusNotFound)
				break
			}
			io.WriteString(w, `{"Id":"sha256:pulled-here"}`)
		case strings.Contains(p, "/images/") && strings.HasSuffix(p, "/json"):
			http.Error(w, `{"message":"No such image"}`, http.StatusNotFound)
		case strings.Contains(p, "/distribution/gone@"):
			http.Error(w, `{"message":"manifest unknown"}`, http.StatusNotFound)
		case strings.Contains(p, "/distribution/"):
			io.WriteString(w, `{}`)
		case strings.HasSuffix(p, "/images/create"):
			ref := r.URL.Query().Get("fromImage")
			switch {
			case strings.HasPrefix(ref, "web@"):
				io.WriteString(w, `{"status":"Downloading","id":"l1","progressDetail":{"current":100,"total":200}}`+"\n")
				io.WriteString(w, `{"status":"Pull complete","id":"l1"}`+"\n")
				mu.Lock()
				pulled[ref] = true
				mu.Unlock()
			case strings.HasPrefix(ref, "db@"):
				io.WriteString(w, `{"errorDetail":{"message":"toomanyrequests: 拉取次数到上限了"},"error":"toomanyrequests"}`+"\n")
			case strings.HasPrefix(ref, "slow@"):
				io.WriteString(w, `{"status":"Downloading","id":"l1","progressDetail":{"current":10,"total":99999999}}`+"\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done() // 卡住不动
			default:
				return false
			}
		case strings.HasSuffix(p, "/images/load"):
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			loaded = append(loaded, saves[string(b)])
			mu.Unlock()
			io.WriteString(w, `{"stream":"Loaded image"}`+"\n")
		case strings.HasSuffix(p, "/tag"):
			id := p[strings.Index(p, "/images/")+len("/images/") : len(p)-len("/tag")]
			mu.Lock()
			tags = append(tags, id+" → "+r.URL.Query().Get("repo")+":"+r.URL.Query().Get("tag"))
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		default:
			return false
		}
		return true
	})
	dc := fake.Client()

	var out bytes.Buffer
	r := testMigrate(t, Deps{Docker: dc, Screen: ui.New(&out, false, nil)}, Options{Link: link})
	r.pkg.workdir = t.TempDir()
	r.pkg.m = &bundle.Manifest{ImageFiles: files}
	r.j = testJournal(t)
	r.chk.images = r.decideImages(context.Background(), files)
	want := map[string]imageSource{"image-1": {local: "sha256:here"}, "image-2": {pull: true}, "image-3": {pull: true}, "image-5": {pull: true}}
	for _, f := range files {
		if r.chk.images[f.File] != want[f.File] {
			t.Errorf("%s 应 %+v，实际 %+v", f.File, want[f.File], r.chk.images[f.File])
		}
	}
	if err := r.loadImageFiles(context.Background()); err != nil {
		t.Fatalf("加载镜像失败：%v\n%s", err, out.String())
	}

	sort.Strings(loaded)
	if strings.Join(loaded, ",") != "image-3,image-4,image-5,image-6" {
		t.Errorf("应从旧服务器下载并原样加载的镜像不对：%v", loaded)
	}
	sort.Strings(tags)
	if strings.Join(tags, "；") != "sha256:here → local:1；sha256:pulled-here → web:1.25；sha256:pulled-here → web:latest" {
		t.Errorf("加的名字不对：%v", tags)
	}
	if left, _ := filepath.Glob(filepath.Join(r.pkg.workdir, "images", "*")); len(left) > 0 {
		t.Errorf("加载完应删掉下载的镜像文件：%v", left)
	}
	// 每个名字改动之前都记进了事务日志（原来指向哪个镜像；没有的为空），失败时改回去。
	entries, err := readJournal(r.j.dir)
	if err != nil {
		t.Fatal(err)
	}
	var noted []string
	for _, e := range entries {
		if e.Op == opTag {
			noted = append(noted, e.Name+"="+e.ID)
		}
	}
	sort.Strings(noted)
	if got := strings.Join(noted, " "); got != "db:16= gone:1= local:1= mine:dev= slow:1= web:1.25= web:latest=" {
		t.Errorf("记下的镜像名：%s", got)
	}
}

// 按摘要固定的镜像：在线补齐成功时容器直接按摘要创建；补不齐、改用迁移包里的那份时，加载的
// 镜像没有仓库清单、按摘要找不到，容器改用加上的名字创建。普通的镜像名照原样用。
func TestLoadImagesNamesPinned(t *testing.T) {
	digest := "alpine@sha256:" + strings.Repeat("a", 64)
	cases := []struct {
		name   string
		pullOK bool
		want   string // 按摘要固定的镜像改用的名字；为空表示照原样用
	}{
		{"在线补齐", true, ""},
		{"用迁移包里的", false, "alpine:pinned-aaaaaaaaaaaa"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pulled := false
			fake := dockertest.New(t, func(w http.ResponseWriter, q *http.Request) bool {
				switch {
				case q.Method == "POST" && strings.HasSuffix(q.URL.Path, "/images/load"):
					io.Copy(io.Discard, q.Body)
					w.Write([]byte(`{"stream":"Loaded image"}`))
				case q.Method == "POST" && strings.HasSuffix(q.URL.Path, "/images/create"):
					pulled = c.pullOK
					return c.pullOK
				case q.Method == "GET" && strings.HasSuffix(q.URL.Path, "/images/json"):
					w.Write([]byte(`[]`))
				case q.Method == "GET" && strings.Contains(q.URL.Path, "@sha256:") && !pulled:
					http.Error(w, `{"message":"No such image"}`, http.StatusNotFound)
				case q.Method == "GET" && strings.HasSuffix(q.URL.Path, "/json"):
					w.Write([]byte(`{"Id":"sha256:` + strings.Repeat("b", 64) + `"}`))
				case q.Method == "POST" && strings.HasSuffix(q.URL.Path, "/tag"):
					w.WriteHeader(http.StatusCreated)
				default:
					return false
				}
				return true
			})
			dc := fake.Client()
			id := "sha256:" + strings.Repeat("b", 64)
			var out bytes.Buffer
			r := testMigrate(t, Deps{Docker: dc, Screen: ui.New(&out, false, nil)}, Options{})
			r.pkg.dataDir = t.TempDir()
			r.pkg.m = &bundle.Manifest{Images: bundle.Images{Refs: map[string]string{digest: id, "nginx:1.27": id}}}
			r.j = testJournal(t)
			if err := r.loadImages(context.Background()); err != nil {
				t.Fatalf("%v\n%s", err, out.String())
			}
			if got := r.imageAs[digest]; got != c.want {
				t.Errorf("按摘要固定的镜像改用：%q，期望 %q", got, c.want)
			}
			if got := r.imageAs["nginx:1.27"]; got != "" {
				t.Errorf("普通的镜像名被改成了 %q", got)
			}
		})
	}
}

// 读写数据卷的临时容器用本机任何一个镜像来建（新服务器上可能一个容器都没有）：优先用名字，
// 其次用“名字@摘要”，都没有时用 ID。以前只按摘要固定的镜像在线补齐以后没有名字，只有这种
// 镜像又有数据卷时，迁入在恢复数据卷时失败。
func TestAnyImage(t *testing.T) {
	digest := "alpine@sha256:" + strings.Repeat("a", 64)
	id := "sha256:" + strings.Repeat("b", 64)
	untagged := docker.Image{ID: id, RepoTags: []string{"<none>:<none>"}, RepoDigests: []string{"<none>@sha256:" + strings.Repeat("c", 64)}}
	cases := []struct {
		name string
		list []docker.Image
		want string
	}{
		{"名字优先", []docker.Image{{ID: id, RepoDigests: []string{digest}}, {ID: id, RepoTags: []string{"nginx:1.27"}}}, "nginx:1.27"},
		{"只有按摘要固定的", []docker.Image{untagged, {ID: id, RepoDigests: []string{digest}}}, digest},
		{"没有名字也没有摘要", []docker.Image{untagged}, id},
		{"一个镜像都没有", nil, ""},
	}
	for _, c := range cases {
		if got := anyImage(c.list); got != c.want {
			t.Errorf("%s：%q，期望 %q", c.name, got, c.want)
		}
	}
}

// testJournal 是测试用的事务日志，测试结束时关闭。
func testJournal(t *testing.T) *Journal {
	t.Helper()
	j, err := openJournal(filepath.Join(t.TempDir(), "rollback"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(j.close)
	return j
}

// 加载镜像时，镜像名记在用它的那组名下（只保留一部分服务时据此改回去，见 commitPartial）；
// 几组都用的算整次迁入共用。看的是名字，不是镜像：两组用同一个镜像的不同名字时各归各的，
// 没有容器用的名字跟着用这个镜像的容器。加载之前记下已有的镜像，回滚时不删它们。
func TestImageNamesFollowGroups(t *testing.T) {
	d := &images{tags: map[string]string{"bad:1": "sha256:old-bad"}, ids: map[string]bool{"sha256:old-bad": true,
		"sha256:good": true, "sha256:bad": true, "sha256:base": true, "sha256:pair": true}}
	var out bytes.Buffer
	r := testMigrate(t, Deps{Docker: d.docker(t), Screen: ui.New(&out, false, nil)}, Options{})
	r.pkg.m = &bundle.Manifest{Images: bundle.Images{Refs: map[string]string{}}}
	r.chk.images = map[string]imageSource{}
	// good、bad 各用自己的镜像；base:1 两个互不相干（分在不同组）的容器都用；left、right 用
	// 同一个镜像的两个名字，这个镜像还有一个谁都没用的名字 pair:latest。
	for _, x := range [][3]string{{"good", "good:1"}, {"bad", "bad:1"}, {"tool-a", "base:1"}, {"tool-b", "base:1"},
		{"left", "left:1", "sha256:pair"}, {"right", "right:1", "sha256:pair"}} {
		it := &citem{m: bundle.Container{Name: x[0], ImageID: x[2]}, c: &docker.Container{}}
		it.c.Config.Image = x[1]
		r.sc.items = append(r.sc.items, it)
	}
	r.sc.buildGroups(r.chk, r.pkg, t.Logf)
	for i, refs := range [][]string{{"good:1"}, {"bad:1"}, {"base:1"}, {"left:1", "right:1", "pair:latest"}} {
		id := "sha256:" + strings.TrimSuffix(refs[0], ":1")
		if len(refs) > 1 {
			id = "sha256:pair"
		}
		f := bundle.ImageFile{File: fmt.Sprint(i), ID: id, Refs: refs}
		r.pkg.m.ImageFiles = append(r.pkg.m.ImageFiles, f)
		for _, ref := range refs {
			r.pkg.m.Images.Refs[ref] = id
		}
		r.chk.images[f.File] = imageSource{local: id} // 本机已经有，只加名字
	}
	j, err := openJournal(filepath.Join(t.TempDir(), "rb"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.close()
	r.j = j
	if err := r.loadImages(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	entries, _ := readJournal(j.dir)
	got := map[string]string{}
	for _, e := range entries {
		switch e.Op {
		case opTag:
			got[e.Name] = e.Group
		case opImages:
			got["已有"] = strings.Join(slices.Sorted(slices.Values(e.Names)), ",")
		}
	}
	want := map[string]string{"good:1": r.sc.groupOf["c:good"], "bad:1": r.sc.groupOf["c:bad"], "base:1": "",
		"left:1": r.sc.groupOf["c:left"], "right:1": r.sc.groupOf["c:right"], "pair:latest": "",
		"已有": "sha256:bad,sha256:base,sha256:good,sha256:old-bad,sha256:pair"}
	if !maps.Equal(got, want) || want["good:1"] == want["bad:1"] || want["left:1"] == want["right:1"] {
		t.Fatalf("实际 %v，应为 %v", got, want)
	}
}

// Docker 认为是同一个名字的不同写法，fullTag 都一样：没写标签的是 latest，没写仓库地址的是
// docker.io，Docker Hub 的官方镜像在 library/ 下面。
func TestFullTag(t *testing.T) {
	for want, refs := range map[string][]string{
		"docker.io/library/app:latest": {"app", "app:latest", "library/app", "docker.io/app", "docker.io/library/app:latest", "index.docker.io/library/app"},
		"docker.io/user/app:1":         {"user/app:1", "docker.io/user/app:1"},
		"localhost:5000/app:latest":    {"localhost:5000/app", "localhost:5000/app:latest"},
		"ghcr.io/o/app:2":              {"ghcr.io/o/app:2"},
		"app@sha256:abc":               {"app@sha256:abc"},
	} {
		for _, ref := range refs {
			if got := fullTag(ref); got != want {
				t.Errorf("fullTag(%q) = %q，应为 %q", ref, got, want)
			}
		}
	}
}
