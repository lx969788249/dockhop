package transfer

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lx969788249/dockhop/internal/crypto"
)

type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) add(e Event) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func (r *recorder) has(kind EventKind) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

func startServer(t *testing.T) (crypto.Link, []byte, *recorder) {
	t.Helper()
	dir := t.TempDir()
	data := make([]byte, 3<<20)
	rand.Read(data)
	os.WriteFile(filepath.Join(dir, "bundle"), data, 0o600)
	os.WriteFile(filepath.Join(dir, "plan"), []byte("PLAN"), 0o600)
	ln, port, err := Listen(28000 + int(time.Now().UnixNano()%2000))
	if err != nil {
		t.Fatal(err)
	}
	link, _ := crypto.NewLink("127.0.0.1", port)
	rec := &recorder{}
	s, err := NewServer(ln, link, filepath.Join(dir, "bundle"), filepath.Join(dir, "plan"), rec.add, nil)
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve()
	t.Cleanup(func() { s.Shutdown(context.Background()) })
	return link, data, rec
}

func get(t *testing.T, url, rangeHeader string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

func TestServerDownloadAndResume(t *testing.T) {
	link, data, rec := startServer(t)
	resp, body := get(t, link.PlanURL(), "")
	if resp.StatusCode != 200 || string(body) != "PLAN" {
		t.Fatalf("迁移计划：%d %q", resp.StatusCode, body)
	}
	// 先下一部分，再从断点继续。
	resp, first := get(t, link.BundleURL(), "bytes=0-1048575")
	if resp.StatusCode != http.StatusPartialContent || len(first) != 1<<20 {
		t.Fatalf("分段下载：%d %d", resp.StatusCode, len(first))
	}
	if rec.has(DownloadComplete) {
		t.Fatal("没下完时不应报告下载完成")
	}
	resp, rest := get(t, link.BundleURL(), fmt.Sprintf("bytes=%d-", len(first)))
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(append(first, rest...), data) {
		t.Fatalf("续传后的内容不一致：%d", resp.StatusCode)
	}
	if !rec.has(DownloadComplete) {
		t.Fatal("下完后应报告下载完成")
	}
	// 随机路径不对时什么也拿不到。
	resp, _ = get(t, strings.Replace(link.BundleURL(), link.Token, "wrongtoken12", 1), "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("错误的路径应返回 404：%d", resp.StatusCode)
	}
}

func TestServerStatus(t *testing.T) {
	link, _, rec := startServer(t)
	if err := SendStatus(context.Background(), link, Status{Result: "success"}); err != nil {
		t.Fatal(err)
	}
	if !rec.has(StatusReceived) {
		t.Fatal("应收到迁入结果")
	}
	// 用别的密钥签名的结果会被拒绝。
	other, _ := crypto.NewLink(link.Host, link.Port)
	other.Token = link.Token
	if err := SendStatus(context.Background(), other, Status{Result: "success"}); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("签名不对应被拒绝：%v", err)
	}
	// 结果字段不对也会被拒绝。
	body := []byte(`{"result":"maybe"}`)
	req, _ := http.NewRequest(http.MethodPost, link.StatusURL(), bytes.NewReader(body))
	req.Header.Set(StatusHeader, hex.EncodeToString(link.SignStatus(body)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("结果字段不对应返回 400：%v %v", resp, err)
	}
}

func TestListenSkipsBusyPort(t *testing.T) {
	busy, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	port := busy.Addr().(*net.TCPAddr).Port
	ln, got, err := Listen(port)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	if got == port {
		t.Fatalf("被占用的端口 %d 应自动往后找", port)
	}
}

func TestIsPublic(t *testing.T) {
	cases := map[string]bool{"8.8.8.8": true, "10.0.0.1": false, "192.168.1.1": false, "100.64.0.1": false, "127.0.0.1": false, "2001:4860::1": true, "fd00::1": false, "fe80::1": false}
	for s, want := range cases {
		if got := isPublic(net.ParseIP(s)); got != want {
			t.Errorf("isPublic(%s) = %v", s, got)
		}
	}
}

// 镜像文件和迁移包一样可以下载、续传；新服务器带上这一轮的总大小时，旧服务器的进度是
// 合起来的一条，迁移包下完才算下载完成。没登记的文件名拿不到任何东西。
func TestServerImagesAndTotalProgress(t *testing.T) {
	dir := t.TempDir()
	bundleData, imageData := make([]byte, 1<<20), make([]byte, 512<<10)
	rand.Read(bundleData)
	rand.Read(imageData)
	os.WriteFile(filepath.Join(dir, "bundle"), bundleData, 0o600)
	os.WriteFile(filepath.Join(dir, "image-1"), imageData, 0o600)
	ln, port, err := Listen(30000 + int(time.Now().UnixNano()%2000))
	if err != nil {
		t.Fatal(err)
	}
	link, _ := crypto.NewLink("127.0.0.1", port)
	rec := &recorder{}
	s, err := NewServer(ln, link, filepath.Join(dir, "bundle"), "", rec.add, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.SetImages(map[string]string{"image-1": filepath.Join(dir, "image-1")})
	go s.Serve()
	t.Cleanup(func() { s.Shutdown(context.Background()) })

	total := int64(len(imageData) + len(bundleData))
	fetch := func(url string, before int64, rangeHeader string) []byte {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.Header.Set(BeforeHeader, fmt.Sprint(before))
		req.Header.Set(TotalHeader, fmt.Sprint(total))
		if rangeHeader != "" {
			req.Header.Set("Range", rangeHeader)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return b
	}
	first := fetch(link.ImageURL("image-1"), 0, "bytes=0-1023")
	rest := fetch(link.ImageURL("image-1"), 0, "bytes=1024-")
	if !bytes.Equal(append(first, rest...), imageData) {
		t.Fatal("镜像文件续传后的内容不一致")
	}
	if rec.has(DownloadComplete) {
		t.Fatal("迁移包还没下载，不应报告下载完成")
	}
	if b := fetch(link.BundleURL(), int64(len(imageData)), ""); !bytes.Equal(b, bundleData) {
		t.Fatal("迁移包内容不一致")
	}
	rec.mu.Lock()
	var last Event
	for _, e := range rec.events {
		if e.Kind == DownloadProgress {
			if e.Size != total || e.Position > total {
				t.Errorf("进度应按这一轮的总大小算：%+v", e)
			}
			last = e
		}
	}
	rec.mu.Unlock()
	if last.Position != total || !rec.has(DownloadComplete) {
		t.Fatalf("全部下完时进度应到头并报告下载完成：%+v", last)
	}
	for _, name := range []string{"image-2", "bundle", "plan"} {
		resp, _ := get(t, link.Base()+"image/"+name, "")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("没登记的镜像文件 %q 应返回 404：%d", name, resp.StatusCode)
		}
	}
}
