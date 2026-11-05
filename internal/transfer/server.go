// Package transfer 负责两台服务器之间的传输：旧服务器上的下载服务，新服务器上的
// 下载（断点续传）和迁入结果通知。
package transfer

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lx969788249/dockhop/internal/crypto"
)

// Status 是新服务器迁入结束后发给旧服务器的结果。
type Status struct {
	Result  string `json:"result"` // success、partial（只迁入了一部分）或 failed
	Message string `json:"message,omitempty"`
}

// EventKind 是下载服务上发生的事。
type EventKind int

const (
	// DownloadProgress：新服务器在下载迁移包，Position 是已下载到的位置。
	DownloadProgress EventKind = iota
	// DownloadComplete：迁移包已经完整下载过一次。
	DownloadComplete
	// StatusReceived：收到了新服务器的迁入结果。
	StatusReceived
)

// Event 是下载服务通知调用方的一件事。
type Event struct {
	Kind     EventKind
	Position int64
	Size     int64
	Status   Status
	Remote   string
}

// Server 是旧服务器上的下载服务：只提供同一个随机路径下的迁移包、镜像文件和迁移计划，
// 并接收新服务器发来的迁入结果。支持断点续传。
type Server struct {
	link       crypto.Link
	prefix     string
	bundlePath string
	planPath   string
	images     map[string]string // 镜像文件名 → 路径
	size       int64
	notify     func(Event)
	logf       func(string, ...any)
	srv        *http.Server
	ln         net.Listener

	mu       sync.Mutex
	position int64
	complete bool
}

// 新服务器下载时带上这两个请求头：这一轮里排在这个文件前面的文件合计多少字节、这一轮
// 一共要下载多少字节。旧服务器据此显示一条总的下载进度，而不是每个文件从 0 重新开始。
const (
	BeforeHeader = "X-DockHop-Before"
	TotalHeader  = "X-DockHop-Total"
)

// Listen 从 port 开始找一个空闲端口监听（同时监听 IPv4 和 IPv6），最多往后找 50 个。
func Listen(port int) (net.Listener, int, error) {
	var lastErr error
	for p := port; p < port+50 && p <= 65535; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
		if err == nil {
			return ln, p, nil
		}
		lastErr = err
	}
	return nil, 0, fmt.Errorf("端口 %d 往后的 50 个端口都被占用了（%v）", port, lastErr)
}

// NewServer 在 ln 上用 HTTPS 提供下载（证书见 crypto.Link.ServerTLS）。notify 收到下载进度
// 和迁入结果，可以为 nil。
func NewServer(ln net.Listener, link crypto.Link, bundlePath, planPath string, notify func(Event), logf func(string, ...any)) (*Server, error) {
	st, err := os.Stat(bundlePath)
	if err != nil {
		return nil, err
	}
	if notify == nil {
		notify = func(Event) {}
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	cfg, err := link.ServerTLS()
	if err != nil {
		return nil, err
	}
	ln = tls.NewListener(ln, cfg)
	prefix := "/" + link.Token + "/"
	s := &Server{link: link, prefix: prefix, bundlePath: bundlePath, planPath: planPath, size: st.Size(), notify: notify, logf: logf, ln: ln}
	mux := http.NewServeMux()
	mux.HandleFunc(prefix+"bundle", s.serveBundle)
	mux.HandleFunc(prefix+"image/", s.serveImage)
	mux.HandleFunc(prefix+"plan", s.servePlan)
	mux.HandleFunc(prefix+"status", s.receiveStatus)
	s.srv = &http.Server{Handler: s.logged(mux), ReadHeaderTimeout: 30 * time.Second,
		// 连接层面的错误（扫描端口的程序常常握手到一半就断开）只写进日志，不打到屏幕上。
		ErrorLog: log.New(logWriter(logf), "", 0)}
	return s, nil
}

// logWriter 把下载服务自己的错误写进 DockHop 的日志。
type logWriter func(string, ...any)

func (f logWriter) Write(p []byte) (int, error) {
	f("下载服务：%s", strings.TrimSpace(string(p)))
	return len(p), nil
}

// Serve 开始提供下载，直到 Shutdown。
func (s *Server) Serve() error {
	err := s.srv.Serve(s.ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown 停止下载服务。
func (s *Server) Shutdown(ctx context.Context) error { return s.srv.Shutdown(ctx) }

// Size 是迁移包的大小。
func (s *Server) Size() int64 { return s.size }

// logged 记录每个请求，日志里把随机路径替换掉。
func (s *Server) logged(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.ReplaceAll(r.URL.Path, s.link.Token, "<口令>")
		s.logf("下载服务：%s %s %s Range=%q", r.RemoteAddr, r.Method, path, r.Header.Get("Range"))
		h.ServeHTTP(w, r)
	})
}

// SetImages 设置可以下载的镜像文件（文件名 → 路径），必须在 Serve 之前调用。
func (s *Server) SetImages(files map[string]string) { s.images = files }

func (s *Server) serveBundle(w http.ResponseWriter, r *http.Request) {
	s.serveFile(w, r, s.bundlePath, true)
}

func (s *Server) serveImage(w http.ResponseWriter, r *http.Request) {
	path, ok := s.images[strings.TrimPrefix(r.URL.Path, s.prefix+"image/")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.serveFile(w, r, path, false)
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, path string, bundle bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "", http.StatusMethodNotAllowed)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/octet-stream")
	// 记录下载到的位置：从请求的起点开始，加上已经发出的字节。
	start := rangeStart(r.Header.Get("Range"))
	before, _ := strconv.ParseInt(r.Header.Get(BeforeHeader), 10, 64)
	total, _ := strconv.ParseInt(r.Header.Get(TotalHeader), 10, 64)
	cw := &countingWriter{ResponseWriter: w, onWrite: func(n int64) {
		s.advance(bundle, start+n, st.Size(), before, total, r.RemoteAddr)
	}}
	http.ServeContent(cw, r, "bundle", time.Time{}, f)
}

// advance 记录下载到了文件的 pos 处，通知调用方总的下载进度；迁移包下载完时通知下载完成。
func (s *Server) advance(bundle bool, pos, size, before, total int64, remote string) {
	done := false
	if bundle {
		s.mu.Lock()
		if pos > s.position {
			s.position = pos
		}
		pos = s.position
		if pos >= s.size && !s.complete {
			s.complete, done = true, true
		}
		s.mu.Unlock()
	}
	if total > 0 {
		pos, size = before+pos, total
	}
	s.notify(Event{Kind: DownloadProgress, Position: pos, Size: size, Remote: remote})
	if done {
		s.notify(Event{Kind: DownloadComplete, Position: pos, Size: size, Remote: remote})
	}
}

func rangeStart(h string) int64 {
	var start int64
	if _, err := fmt.Sscanf(h, "bytes=%d-", &start); err != nil {
		return 0
	}
	return start
}

type countingWriter struct {
	http.ResponseWriter
	written int64
	onWrite func(total int64)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.written += int64(n)
	c.onWrite(c.written)
	return n, err
}

func (s *Server) servePlan(w http.ResponseWriter, r *http.Request) {
	if s.planPath == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, s.planPath)
}

// StatusHeader 是迁入结果签名所在的请求头。
const StatusHeader = "X-DockHop-Signature"

func (s *Server) receiveStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil {
		http.Error(w, "", http.StatusBadRequest)
		return
	}
	sig, err := hex.DecodeString(r.Header.Get(StatusHeader))
	if err != nil || !s.link.VerifyStatus(body, sig) {
		s.logf("下载服务：收到签名不对的迁入结果，已忽略")
		http.Error(w, "", http.StatusForbidden)
		return
	}
	var st Status
	if err := json.Unmarshal(body, &st); err != nil || (st.Result != "success" && st.Result != "partial" && st.Result != "failed") {
		http.Error(w, "", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
	s.notify(Event{Kind: StatusReceived, Status: st, Remote: r.RemoteAddr})
}

// SendStatus 由新服务器调用：把迁入结果通知旧服务器。旧服务器已经关闭或不认识
// 这个地址时返回错误，调用方忽略即可，不影响迁入。
func SendStatus(ctx context.Context, link crypto.Link, st Status) error {
	body, err := json.Marshal(st)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, link.StatusURL(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(StatusHeader, hex.EncodeToString(link.SignStatus(body)))
	resp, err := httpClient(link, 10*time.Second).Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("旧服务器返回 HTTP %d", resp.StatusCode)
	}
	return nil
}
