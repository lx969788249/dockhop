// Package dockertest 是测试用的假 Docker：本机上的一个 HTTP 服务，按 Docker 接口回答，记下收到的请求。
// 只有测试引用它，不会编进发布的程序。
package dockertest

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/lx969788249/dockhop/internal/docker"
)

// Server 是一个假 Docker。
type Server struct {
	t      testing.TB
	api    string
	socket bool
	host   string
	mux    *http.ServeMux
	handle func(http.ResponseWriter, *http.Request) bool
	mu     sync.Mutex
	calls  []string
}

// Option 是假 Docker 的设置。
type Option func(*Server)

// API 设定接口版本，默认 1.44。
func API(v string) Option { return func(s *Server) { s.api = v } }

// Socket 让它监听 Unix 套接字（Host 是 unix://…），连上来的程序当作 Docker 在本机；默认监听本机的 TCP
// 端口，当作 Docker 不在本机。
func Socket() Option { return func(s *Server) { s.socket = true } }

// NotFound 回答 404（要找的东西不存在）。给 New 当 handle 时，没登记的请求都这样回答。
func NotFound(w http.ResponseWriter, r *http.Request) bool {
	http.NotFound(w, r)
	return true
}

// New 启动一个假 Docker，测试结束时关掉。它自己回答 /version；别的请求先按 Reply 登记的回答，没登记的交给
// handle，handle 为空或者返回 false 时回答 500（模拟 Docker 内部错误）。交给它们的路径去掉了接口版本的前缀
// （/v1.44 这样，前缀里的版本见 Version），记下的请求也不带。
func New(t testing.TB, handle func(w http.ResponseWriter, r *http.Request) bool, opts ...Option) *Server {
	t.Helper()
	s := &Server{t: t, api: "1.44", mux: http.NewServeMux(), handle: handle}
	for _, o := range opts {
		o(s)
	}
	if !s.socket {
		srv := httptest.NewServer(http.HandlerFunc(s.serve))
		t.Cleanup(srv.Close)
		s.host = "tcp://" + strings.TrimPrefix(srv.URL, "http://")
		return s
	}
	// Unix 套接字的路径不能太长（macOS 上最多 104 字节），t.TempDir() 的太长。
	dir, err := os.MkdirTemp("", "dh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(s.serve)}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	s.host = "unix://" + sock
	return s
}

var versionPrefix = regexp.MustCompile(`^/v([0-9]+\.[0-9]+)/`)

type versionKey struct{}

// Version 是请求路径里去掉的接口版本（例如 1.51），没有时为空。客户端有时特意用较老的版本（见
// docker.InspectForRebuild），测试据此分辨。
func Version(r *http.Request) string {
	v, _ := r.Context().Value(versionKey{}).(string)
	return v
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/version" {
		fmt.Fprintf(w, `{"Version":"29.0.0","ApiVersion":%q}`, s.api)
		return
	}
	if m := versionPrefix.FindStringSubmatch(r.URL.Path); m != nil {
		r = r.WithContext(context.WithValue(r.Context(), versionKey{}, m[1]))
		r.URL.Path = "/" + strings.TrimPrefix(r.URL.Path, m[0])
		if raw := versionPrefix.FindString(r.URL.RawPath); raw != "" {
			r.URL.RawPath = "/" + strings.TrimPrefix(r.URL.RawPath, raw)
		}
	}
	s.mu.Lock()
	s.calls = append(s.calls, r.Method+" "+r.URL.Path)
	s.mu.Unlock()
	if h, pattern := s.mux.Handler(r); pattern != "" {
		h.ServeHTTP(w, r)
		return
	}
	if s.handle == nil || !s.handle(w, r) {
		http.Error(w, `{"message":"模拟的 Docker 内部错误"}`, http.StatusInternalServerError)
	}
}

// Reply 登记一个固定的回答：pattern 用 net/http 的写法（例如 "GET /containers/{id}/json"，路径不带接口版本的
// 前缀），回答 200，内容是 body。
func (s *Server) Reply(pattern, body string) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, body) })
}

// Client 是连到这个假 Docker 的客户端。
func (s *Server) Client() *docker.Client {
	s.t.Helper()
	dc, err := docker.New(context.Background(), docker.WithHost(s.host))
	if err != nil {
		s.t.Fatal(err)
	}
	return dc
}

// Host 是它的地址，可以设成 DOCKER_HOST。
func (s *Server) Host() string { return s.host }

// Calls 是收到过的请求（“方法 路径”，不含 /version），按收到的先后。
func (s *Server) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}
