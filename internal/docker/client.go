// Package docker 是一个精简的 Docker 接口客户端。
//
// 不用 Docker 官方的 Go 客户端：它依赖多、体积大，而且它的类型定义会丢掉比它
// 新的 Docker 版本才有的字段。DockHop 要把容器配置原样搬到新服务器，所以容器
// 配置一律按原始 JSON 读写，只有少数需要读取的字段才用结构体。
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// MinAPIVersion 是支持的最低接口版本（Docker 20.10）。
	MinAPIVersion = "1.41"
	// MaxAPIVersion 是测试过的最高接口版本（Docker 29）。比它新的 Docker 用这个版本通信。
	MaxAPIVersion = "1.53"
	defaultSocket = "/var/run/docker.sock"
)

// Client 连接本机或 DOCKER_HOST 指定的 Docker。
type Client struct {
	http   *http.Client
	base   string // http://docker 或 http://host:port
	api    string // 协商后的接口版本
	server Version
	local  bool // Docker 是否在本机（unix socket）
	host   string
	logf   func(format string, args ...any)
}

// Version 是 Docker 的版本信息。
type Version struct {
	Version       string `json:"Version"`
	APIVersion    string `json:"ApiVersion"`
	MinAPIVersion string `json:"MinAPIVersion"`
	Os            string `json:"Os"`
	Arch          string `json:"Arch"`
	KernelVersion string `json:"KernelVersion"`
}

// Error 是 Docker 返回的错误。
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("Docker 返回 HTTP %d", e.Status)
	}
	return e.Message
}

// IsNotFound 表示要找的容器、镜像、卷或网络不存在。
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

// ErrNotRunning 表示连不上 Docker。
var ErrNotRunning = errors.New("连不上 Docker")

// Option 调整客户端。
type Option func(*Client)

// WithLogger 把每个请求记进日志。
func WithLogger(logf func(format string, args ...any)) Option {
	return func(c *Client) { c.logf = logf }
}

// WithHost 指定 Docker 地址（unix:///path 或 tcp://host:port），优先于 DOCKER_HOST。
func WithHost(host string) Option {
	return func(c *Client) { c.host = host }
}

// New 连接 Docker 并协商接口版本。
func New(ctx context.Context, opts ...Option) (*Client, error) {
	c := &Client{logf: func(string, ...any) {}, host: os.Getenv("DOCKER_HOST")}
	for _, o := range opts {
		o(c)
	}
	if c.host == "" {
		c.host = "unix://" + defaultSocket
	}
	if err := c.setHost(c.host); err != nil {
		return nil, err
	}
	if err := c.negotiate(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Client) setHost(host string) error {
	u, err := url.Parse(host)
	if err != nil {
		return fmt.Errorf("DOCKER_HOST 格式不对：%s", host)
	}
	switch u.Scheme {
	case "unix":
		sock := u.Path
		c.local = true
		c.base = "http://docker"
		c.http = &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
			MaxIdleConns:    4,
			IdleConnTimeout: 30 * time.Second,
		}}
	case "tcp", "http":
		c.local = false
		c.base = "http://" + u.Host
		c.http = &http.Client{Transport: &http.Transport{MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second}}
	default:
		return fmt.Errorf("不支持这种 DOCKER_HOST：%s（只支持 unix:// 和 tcp://）", host)
	}
	return nil
}

func (c *Client) negotiate(ctx context.Context) error {
	var v Version
	// /version 不带版本前缀，任何版本的 Docker 都能回答。
	if err := c.do(ctx, http.MethodGet, "/version", nil, nil, "", &v); err != nil {
		var e *Error
		if errors.As(err, &e) {
			return err
		}
		return fmt.Errorf("%w：%v", ErrNotRunning, err)
	}
	if v.APIVersion == "" {
		return errors.New("没有从 Docker 读到接口版本")
	}
	if CompareVersions(v.APIVersion, MinAPIVersion) < 0 {
		return fmt.Errorf("这台服务器的 Docker %s 太旧了（接口版本 %s），DockHop 需要 Docker 20.10 或更新的版本", v.Version, v.APIVersion)
	}
	c.server = v
	c.api = v.APIVersion
	if CompareVersions(c.api, MaxAPIVersion) > 0 {
		c.api = MaxAPIVersion
	}
	c.logf("Docker %s · 接口版本 %s（使用 %s）", v.Version, v.APIVersion, c.api)
	return nil
}

// Server 返回 Docker 的版本信息。
func (c *Client) Server() Version { return c.server }

// APIVersion 返回协商后的接口版本。
func (c *Client) APIVersion() string { return c.api }

// AtLeast 表示协商后的接口版本是否不低于 v，例如创建时连接多个网络需要 1.44。
func (c *Client) AtLeast(v string) bool { return CompareVersions(c.api, v) >= 0 }

// Local 表示 Docker 是否在本机。宿主机目录只能在本机直接读写。
func (c *Client) Local() bool { return c.local }

// CompareVersions 比较“1.41”这样的版本号。
func CompareVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// do 发出请求。path 以 / 开头且不含版本前缀时原样请求；body 为 io.Reader 时
// 原样发送，否则编码成 JSON；out 为 *json.RawMessage、*[]byte 或其他结构体。
func (c *Client) do(ctx context.Context, method, path string, q url.Values, body any, contentType string, out any) error {
	resp, err := c.send(ctx, method, path, q, body, contentType)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	switch o := out.(type) {
	case *[]byte:
		*o, err = io.ReadAll(resp.Body)
		return err
	default:
		return json.NewDecoder(resp.Body).Decode(out)
	}
}

func (c *Client) send(ctx context.Context, method, path string, q url.Values, body any, contentType string) (*http.Response, error) {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var r io.Reader
	switch b := body.(type) {
	case nil:
	case io.Reader:
		r = b
	case json.RawMessage:
		r = bytes.NewReader(b)
		contentType = "application/json"
	default:
		buf, err := json.Marshal(b)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(buf)
		contentType = "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	c.logf("Docker 接口：%s %s", method, path)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		var m struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &m)
		if m.Message == "" {
			m.Message = strings.TrimSpace(string(raw))
		}
		e := &Error{Status: resp.StatusCode, Message: m.Message}
		c.logf("  → HTTP %d：%s", e.Status, e.Message)
		return nil, e
	}
	return resp, nil
}

// versioned 给接口路径加上协商后的版本前缀。
func (c *Client) versioned(path string) string { return "/v" + c.api + path }

// Get 请求一个接口并把结果解码到 out。
func (c *Client) Get(ctx context.Context, path string, q url.Values, out any) error {
	return c.do(ctx, http.MethodGet, c.versioned(path), q, nil, "", out)
}

// Post 发送 body（结构体、json.RawMessage 或 nil）并把结果解码到 out（可以为 nil）。
func (c *Client) Post(ctx context.Context, path string, q url.Values, body, out any) error {
	return c.do(ctx, http.MethodPost, c.versioned(path), q, body, "", out)
}

// Delete 删除一个对象。
func (c *Client) Delete(ctx context.Context, path string, q url.Values) error {
	return c.do(ctx, http.MethodDelete, c.versioned(path), q, nil, "", nil)
}

// Stream 发出请求并返回响应内容，用于镜像导出、文件归档这类大数据流。调用方负责关闭。
func (c *Client) Stream(ctx context.Context, method, path string, q url.Values, body io.Reader, contentType string) (io.ReadCloser, error) {
	resp, err := c.send(ctx, method, c.versioned(path), q, body, contentType)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}
