package restore

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/crypto"
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
	dc, _ := faultyDocker(t, func(w http.ResponseWriter, r *http.Request) bool {
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

	var out bytes.Buffer
	r := &run{Deps: Deps{Docker: dc, Screen: ui.New(&out, false, nil), Getenv: func(string) string { return "" }},
		opts: Options{Link: link}, workdir: t.TempDir(), m: &bundle.Manifest{ImageFiles: files}, j: testJournal(t)}
	r.decideImages(context.Background(), files)
	want := map[string]imageSource{"image-1": {local: "sha256:here"}, "image-2": {pull: true}, "image-3": {pull: true}, "image-5": {pull: true}}
	for _, f := range files {
		if r.images[f.File] != want[f.File] {
			t.Errorf("%s 应 %+v，实际 %+v", f.File, want[f.File], r.images[f.File])
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
	if left, _ := filepath.Glob(filepath.Join(r.workdir, "images", "*")); len(left) > 0 {
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
