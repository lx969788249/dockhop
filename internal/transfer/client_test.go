package transfer

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lx969788249/dockhop/internal/crypto"
)

// 下载到一半连接断开：自动从断点继续，拼起来和原文件一致。
func TestDownloadResume(t *testing.T) {
	dir := t.TempDir()
	content := bytes.Repeat([]byte("0123456789abcdef"), 64*1024) // 1MB
	bundlePath := filepath.Join(dir, "bundle")
	os.WriteFile(bundlePath, content, 0o600)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	link, _ := crypto.NewLink("127.0.0.1", port)
	srv, err := NewServer(ln, link, bundlePath, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 第一次请求只发 300KB 就断开。
	var requests atomic.Int32
	inner := srv.srv.Handler
	srv.srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			hj, _ := w.(http.Hijacker)
			conn, buf, _ := hj.Hijack()
			buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: " + strconv.Itoa(len(content)) + "\r\n\r\n")
			buf.Write(content[:300*1024])
			buf.Flush()
			conn.Close()
			return
		}
		inner.ServeHTTP(w, r)
	})
	go srv.Serve()
	defer srv.Shutdown(context.Background())

	c := NewClient(5*time.Second, t.Logf)
	dst := filepath.Join(dir, "download")
	var last int64
	if err := c.Download(context.Background(), link, dst, func(done, total int64) { last = done }); err != nil {
		t.Fatalf("应自动续传成功：%v", err)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, content) || last != int64(len(content)) || requests.Load() != 2 {
		t.Fatalf("续传结果不对：%d 字节，请求 %d 次", len(got), requests.Load())
	}
	// 已经下载完整：再下载一次直接结束，不重新下载。
	if err := c.Download(context.Background(), link, dst, nil); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(dst); st.Size() != int64(len(content)) {
		t.Error("已下载完整的文件不应被改动")
	}
}

func TestConnectionErrors(t *testing.T) {
	// 没有程序监听的端口：拒绝连接。
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	link, _ := crypto.NewLink("127.0.0.1", port)
	c := NewClient(2*time.Second, nil)
	_, err := c.FetchPlan(context.Background(), link)
	var ce *ConnError
	if !errors.As(err, &ce) || !strings.Contains(ce.Reason, "拒绝连接") || !strings.Contains(ce.Hint, "ufw allow") {
		t.Errorf("拒绝连接：%v", err)
	}

	// 下载服务还在、但链接不对（迁出已经重新运行过）：404。
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "bundle"), []byte("x"), 0o600)
	ln, _ = net.Listen("tcp", "127.0.0.1:0")
	port = ln.Addr().(*net.TCPAddr).Port
	served, _ := crypto.NewLink("127.0.0.1", port)
	srv, _ := NewServer(ln, served, filepath.Join(dir, "bundle"), "", nil, nil)
	go srv.Serve()
	defer srv.Shutdown(context.Background())
	other, _ := crypto.NewLink("127.0.0.1", port)
	_, err = c.FetchPlan(context.Background(), other)
	if !errors.As(err, &ce) || !strings.Contains(ce.Reason, "找不到这次迁移") {
		t.Errorf("404：%v", err)
	}
	// 同一个迁移，但旧服务器没生成迁移计划：只是跳过检查。
	if _, err := c.FetchPlan(context.Background(), served); !errors.Is(err, ErrNoPlan) {
		t.Errorf("没有迁移计划：%v", err)
	}
}

func TestConnectTimeoutExplained(t *testing.T) {
	// 192.0.2.0/24 是文档专用地址，不会有人响应：连接超时。
	link, _ := crypto.NewLink("192.0.2.1", 18080)
	c := NewClient(500*time.Millisecond, nil)
	start := time.Now()
	_, err := c.FetchPlan(context.Background(), link)
	var ce *ConnError
	if !errors.As(err, &ce) {
		t.Skipf("这台机器上的网络不会超时（%v）", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("连不上时应很快说明原因，用了 %s", time.Since(start))
	}
	if !strings.Contains(ce.Reason, "超时") && !strings.Contains(ce.Reason, "不可达") {
		t.Errorf("超时：%v", err)
	}
	if !strings.Contains(ce.Hint, "安全组") && !strings.Contains(ce.Hint, "公网地址") {
		t.Errorf("建议里应提到安全组：%s", ce.Hint)
	}
}
