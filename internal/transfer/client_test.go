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

	c := NewClient(link, 5*time.Second, t.Logf)
	dst := filepath.Join(dir, "download")
	var last int64
	if err := c.Download(context.Background(), Part{URL: link.BundleURL()}, dst, func(done, total int64) { last = done }); err != nil {
		t.Fatalf("应自动续传成功：%v", err)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, content) || last != int64(len(content)) || requests.Load() != 2 {
		t.Fatalf("续传结果不对：%d 字节，请求 %d 次", len(got), requests.Load())
	}
	// 已经下载完整：再下载一次直接结束，不重新下载。
	if err := c.Download(context.Background(), Part{URL: link.BundleURL()}, dst, nil); err != nil {
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
	_, err := NewClient(link, 2*time.Second, nil).FetchPlan(context.Background())
	var ce *ConnError
	if !errors.As(err, &ce) || !strings.Contains(ce.Reason, "拒绝连接") || !strings.Contains(ce.Hint, "ufw allow") {
		t.Errorf("拒绝连接：%v", err)
	}

	// 下载服务还在、但链接不对（迁出已经重新运行过）：证书和链接里的密钥对不上，什么也不下载。
	served := serveBundleOnly(t)
	other, _ := crypto.NewLink("127.0.0.1", served.Port)
	_, err = NewClient(other, 2*time.Second, nil).FetchPlan(context.Background())
	if !errors.As(err, &ce) || !errors.Is(err, crypto.ErrWrongServer) || !strings.Contains(ce.Reason, "连上的不是这次迁出") ||
		!strings.Contains(ce.Hint, "最新显示的链接") {
		t.Errorf("链接不对：%v", err)
	}
	// 同一个迁移，但旧服务器没生成迁移计划：不能迁入。
	if _, err := NewClient(served, 2*time.Second, nil).FetchPlan(context.Background()); !errors.Is(err, ErrNoPlan) {
		t.Errorf("没有迁移计划：%v", err)
	}
	// 密钥对、下载口令不对（复制时改了字符）：404。
	altered := served
	altered.Token = "wrongtoken12"
	_, err = NewClient(altered, 2*time.Second, nil).FetchPlan(context.Background())
	if !errors.As(err, &ce) || !strings.Contains(ce.Reason, "下载地址不对") {
		t.Errorf("404：%v", err)
	}

	// 端口上是别的、不加密的服务：也不是这次的迁出。
	plain := httptestServer(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hello")) })
	wrong, _ := crypto.NewLink("127.0.0.1", plain.port)
	_, err = NewClient(wrong, 2*time.Second, nil).FetchPlan(context.Background())
	if !errors.As(err, &ce) || !strings.Contains(ce.Reason, "连上的不是这次迁出") {
		t.Errorf("对方不是 HTTPS：%v", err)
	}
}

// serveBundleOnly 开一个只有迁移包、没有迁移计划的下载服务，返回它的链接。
func serveBundleOnly(t *testing.T) crypto.Link {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "bundle"), []byte("x"), 0o600)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	link, _ := crypto.NewLink("127.0.0.1", ln.Addr().(*net.TCPAddr).Port)
	srv, err := NewServer(ln, link, filepath.Join(dir, "bundle"), "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	return link
}

func TestConnectTimeoutExplained(t *testing.T) {
	// 192.0.2.0/24 是文档专用地址，不会有人响应：连接超时。
	link, _ := crypto.NewLink("192.0.2.1", 18080)
	c := NewClient(link, 500*time.Millisecond, nil)
	start := time.Now()
	_, err := c.FetchPlan(context.Background())
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

// 文件大小以迁移计划为准：说的大小不对，或者发来的数据比应有的多（没完没了地发），都不是要下载
// 的文件，马上停下、删掉，不能一直往磁盘里写，也不重试。大小对的照常下载。
func TestDownloadSizeLimit(t *testing.T) {
	content := bytes.Repeat([]byte("x"), 100<<10)
	for _, c := range []struct {
		name   string
		size   int64
		header bool // 是否写 Content-Length
		ok     bool
	}{
		{"大小对", int64(len(content)), true, true},
		{"说的大小不对", int64(len(content)) - 1, true, false},
		{"不说大小、发得比应有的多", 10 << 10, false, false},
	} {
		var requests atomic.Int32
		srv := httptestServer(t, func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			if c.header {
				w.Header().Set("Content-Length", strconv.Itoa(len(content)))
			}
			w.Write(content)
		})
		link, _ := crypto.NewLink("127.0.0.1", srv.port)
		dst := filepath.Join(t.TempDir(), "bundle")
		err := NewClient(link, 5*time.Second, t.Logf).Download(context.Background(), Part{URL: srv.url, Size: c.size}, dst, nil)
		if c.ok != (err == nil) {
			t.Errorf("%s：%v", c.name, err)
		}
		if !c.ok {
			if _, serr := os.Stat(dst); !os.IsNotExist(serr) {
				t.Errorf("%s：不对的文件应删掉", c.name)
			}
			if !errors.Is(err, errSize) || requests.Load() != 1 {
				t.Errorf("%s：应马上停下、不重试（请求了 %d 次）：%v", c.name, requests.Load(), err)
			}
		}
	}
}

type testServer struct {
	url  string
	port int
}

func httptestServer(t *testing.T, h http.HandlerFunc) testServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &http.Server{Handler: h}
	go s.Serve(ln)
	t.Cleanup(func() { s.Close() })
	port := ln.Addr().(*net.TCPAddr).Port
	return testServer{url: "http://127.0.0.1:" + strconv.Itoa(port) + "/file", port: port}
}
