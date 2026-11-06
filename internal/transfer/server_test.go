package transfer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
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
	link, data, rec, _ := startServerLogs(t)
	return link, data, rec
}

// startServerLogs 开一个下载服务，另外返回下载服务写进日志的内容。
func startServerLogs(t *testing.T) (crypto.Link, []byte, *recorder, func() string) {
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
	var mu sync.Mutex
	var logs strings.Builder
	logf := func(format string, args ...any) {
		mu.Lock()
		fmt.Fprintf(&logs, format+"\n", args...)
		mu.Unlock()
	}
	s, err := NewServer(ln, link, filepath.Join(dir, "bundle"), filepath.Join(dir, "plan"), rec.add, logf)
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve()
	t.Cleanup(func() { s.Shutdown(context.Background()) })
	return link, data, rec, func() string {
		mu.Lock()
		defer mu.Unlock()
		return logs.String()
	}
}

// get 像新服务器一样下载：按链接核对旧服务器的证书。
func get(t *testing.T, link crypto.Link, url, rangeHeader string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	resp, err := httpClient(link, 5*time.Second).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

func TestServerDownloadAndResume(t *testing.T) {
	link, data, rec := startServer(t)
	resp, body := get(t, link, link.PlanURL(), "")
	if resp.StatusCode != 200 || string(body) != "PLAN" {
		t.Fatalf("迁移计划：%d %q", resp.StatusCode, body)
	}
	// 先下一部分，再从断点继续。
	resp, first := get(t, link, link.BundleURL(), "bytes=0-1048575")
	if resp.StatusCode != http.StatusPartialContent || len(first) != 1<<20 {
		t.Fatalf("分段下载：%d %d", resp.StatusCode, len(first))
	}
	if rec.has(DownloadComplete) {
		t.Fatal("没下完时不应报告下载完成")
	}
	resp, rest := get(t, link, link.BundleURL(), fmt.Sprintf("bytes=%d-", len(first)))
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(append(first, rest...), data) {
		t.Fatalf("续传后的内容不一致：%d", resp.StatusCode)
	}
	if !rec.has(DownloadComplete) {
		t.Fatal("下完后应报告下载完成")
	}
	// 随机路径不对时什么也拿不到。
	resp, _ = get(t, link, strings.Replace(link.BundleURL(), link.Token, "wrongtoken12", 1), "")
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
	// 拿着别的链接的新服务器：证书对不上，连结果都不发出去。
	other, _ := crypto.NewLink(link.Host, link.Port)
	other.Token = link.Token
	if err := SendStatus(context.Background(), other, Status{Result: "success"}); !errors.Is(err, crypto.ErrWrongServer) {
		t.Fatalf("证书对不上时不应发出结果：%v", err)
	}
	// 签名不对的结果被拒绝。
	post := func(body []byte, sig string) int {
		req, _ := http.NewRequest(http.MethodPost, link.StatusURL(), bytes.NewReader(body))
		req.Header.Set(StatusHeader, sig)
		resp, err := httpClient(link, 5*time.Second).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	body := []byte(`{"result":"success"}`)
	if code := post(body, hex.EncodeToString(other.SignStatus(body))); code != http.StatusForbidden {
		t.Fatalf("签名不对应返回 403：%d", code)
	}
	// 结果字段不对也会被拒绝。
	body = []byte(`{"result":"maybe"}`)
	if code := post(body, hex.EncodeToString(link.SignStatus(body))); code != http.StatusBadRequest {
		t.Fatalf("结果字段不对应返回 400：%d", code)
	}
}

// 只拿到链接 # 前面那部分（没有密钥）的人不核对证书硬连（像 curl -k）：迁移计划、迁移包、
// 镜像文件一个字节也拿不到，也发不了迁入结果，旧服务器不会把它当成新服务器在下载。
func TestServerRequiresKey(t *testing.T) {
	link, _, rec := startServer(t)
	insecure := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	try := func(method, url string, body []byte) {
		req, _ := http.NewRequest(method, url, bytes.NewReader(body))
		if resp, err := insecure.Do(req); err == nil {
			resp.Body.Close()
			t.Errorf("没有密钥也连上了：%s %s 返回 %d", method, strings.TrimPrefix(url, link.Base()), resp.StatusCode)
		}
	}
	for _, url := range []string{link.PlanURL(), link.BundleURL(), link.ImageURL("image-1")} {
		try(http.MethodGet, url, nil)
		try(http.MethodHead, url, nil)
	}
	try(http.MethodPost, link.StatusURL(), []byte(`{"result":"success"}`))
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.events) > 0 {
		t.Errorf("没有密钥的连接不应算作新服务器在下载或迁入：%+v", rec.events)
	}
}

// 下载服务走 HTTPS：不加密的请求拿不到任何东西，握手失败只写进日志，不打到屏幕上。
func TestServerRefusesPlainHTTP(t *testing.T) {
	link, data, _, logs := startServerLogs(t)
	plain := strings.Replace(link.BundleURL(), "https://", "http://", 1)
	resp, err := http.Get(plain)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || bytes.Contains(b, data[:64]) {
		t.Fatalf("不加密的请求应被拒绝：%d %q", resp.StatusCode, b)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs(), "下载服务：http: TLS handshake error") {
		if time.Now().After(deadline) {
			t.Fatalf("握手失败应写进日志：%q", logs())
		}
		time.Sleep(20 * time.Millisecond)
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
		resp, err := httpClient(link, 5*time.Second).Do(req)
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
		resp, _ := get(t, link, link.Base()+"image/"+name, "")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("没登记的镜像文件 %q 应返回 404：%d", name, resp.StatusCode)
		}
	}
}
