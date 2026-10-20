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
