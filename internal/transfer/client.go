package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/lx969788249/dockhop/internal/crypto"
)

// Client 是新服务器上的下载客户端。
type Client struct {
	http *http.Client
	// ConnectTimeout 是连接旧服务器的超时时间；连不上时要很快说明原因，不能重试好几分钟。
	ConnectTimeout time.Duration
	// Retries 是下载中断后自动续传的次数。
	Retries int
	logf    func(string, ...any)
}

// NewClient 创建下载客户端。
func NewClient(connectTimeout time.Duration, logf func(string, ...any)) *Client {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	c := &Client{ConnectTimeout: connectTimeout, Retries: 20, logf: logf}
	c.http = &http.Client{Transport: &http.Transport{
		Proxy:                 nil, // 两台服务器直连，不走代理
		DialContext:           (&net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}).DialContext,
		ResponseHeaderTimeout: 2 * time.Minute,
		IdleConnTimeout:       30 * time.Second,
	}}
	return c
}

// ConnError 说明为什么连不上旧服务器，以及怎么处理。
type ConnError struct {
	Reason string
	Hint   string
	Err    error
}

func (e *ConnError) Error() string { return e.Reason }
func (e *ConnError) Unwrap() error { return e.Err }

// ErrNoPlan 表示旧服务器没有提供迁移计划（生成失败），只能跳过迁入前检查。
var ErrNoPlan = errors.New("旧服务器没有提供迁移计划")

// explain 把连接错误换成大白话。
func explain(link crypto.Link, err error) error {
	where := fmt.Sprintf("旧服务器 %s 的端口 %d", link.Host, link.Port)
	firewall := fmt.Sprintf("在旧服务器的云服务器安全组和防火墙里放行 TCP %d 端口（例如 ufw allow %d/tcp），然后重新运行", link.Port, link.Port)
	var ne net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return err
	case errors.Is(err, syscall.ECONNREFUSED):
		return &ConnError{Reason: "连接被拒绝：" + where + " 拒绝连接",
			Hint: "确认旧服务器上的 DockHop 迁出还在运行（没有按回车结束），并" + firewall, Err: err}
	case errors.As(err, &ne) && ne.Timeout():
		return &ConnError{Reason: "连接超时：连接" + where + " 超时",
			Hint: firewall + "；也请确认链接里的地址是旧服务器的公网地址", Err: err}
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return &ConnError{Reason: "网络不通：到旧服务器 " + link.Host + " 的网络不可达",
			Hint: "确认链接里的地址是旧服务器的公网地址，两台服务器之间网络是通的", Err: err}
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return &ConnError{Reason: "找不到旧服务器的地址 " + link.Host, Hint: "确认链接完整、地址没有写错", Err: err}
	}
	return &ConnError{Reason: fmt.Sprintf("连接旧服务器 %s 的端口 %d 失败：%v", link.Host, link.Port, err), Hint: firewall, Err: err}
}

func statusError(link crypto.Link, code int) error {
	if code == http.StatusNotFound {
		return &ConnError{Reason: "旧服务器上找不到这次迁移（返回 404）",
			Hint: "旧服务器上的迁出可能已经结束或重新运行过；请在旧服务器重新运行迁出，用新的链接"}
	}
	return &ConnError{Reason: fmt.Sprintf("旧服务器 %s 返回 HTTP %d", link.Host, code), Hint: "确认链接完整，并且旧服务器上的迁出还在运行"}
}

// FetchPlan 下载加密的迁移计划（很小）。旧服务器没有提供时返回 ErrNoPlan。
func (c *Client) FetchPlan(ctx context.Context, link crypto.Link) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link.PlanURL(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, explain(link, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// 区分“计划没生成”和“整个迁移不存在”：迁移包还在就只是没有计划。
		if c.bundleExists(ctx, link) {
			return nil, ErrNoPlan
		}
		return nil, statusError(link, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(link, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

func (c *Client) bundleExists(ctx context.Context, link crypto.Link) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, link.BundleURL(), nil)
	if err != nil {
		return false
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Download 把迁移包下载到 path。path 已经有一部分时从断点继续；中途断开会自动
// 续传，最多 Retries 次。progress 收到已经下载的总字节数和总大小。
func (c *Client) Download(ctx context.Context, link crypto.Link, path string, progress func(done, total int64)) error {
	failures := 0
	for {
		done, total, err := c.downloadOnce(ctx, link, path, progress)
		if err == nil && done == total {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ce *ConnError
		if errors.As(err, &ce) && done == 0 {
			return err // 一开始就连不上：马上说明原因
		}
		failures++
		if failures > c.Retries {
			if err == nil {
				err = fmt.Errorf("下载不完整（%d/%d 字节）", done, total)
			}
			return fmt.Errorf("下载多次中断，已放弃：%w", err)
		}
		c.logf("下载中断（已下载 %d 字节）：%v，%d 秒后从断点继续", done, err, failures)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(min(failures, 10)) * time.Second):
		}
	}
}

func (c *Client) downloadOnce(ctx context.Context, link crypto.Link, path string, progress func(done, total int64)) (int64, int64, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	have, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link.BundleURL(), nil)
	if err != nil {
		return have, 0, err
	}
	if have > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(have, 10)+"-")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return have, 0, explain(link, err)
	}
	defer resp.Body.Close()
	var total int64
	switch resp.StatusCode {
	case http.StatusOK:
		// 服务器不认断点（或是从头开始）：从头写。
		if err := f.Truncate(0); err != nil {
			return 0, 0, err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return 0, 0, err
		}
		have = 0
		total = resp.ContentLength
	case http.StatusPartialContent:
		var start, end int64
		if _, err := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total); err != nil || start != have {
			return have, 0, fmt.Errorf("旧服务器返回的续传位置不对：%q", resp.Header.Get("Content-Range"))
		}
	case http.StatusRequestedRangeNotSatisfiable:
		// 已经下载完整了（或者本地文件比服务器上的还大，说明不是同一个包）。
		var size int64
		if _, err := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes */%d", &size); err == nil && size == have {
			if progress != nil {
				progress(have, have)
			}
			return have, have, nil
		}
		if err := f.Truncate(0); err != nil {
			return 0, 0, err
		}
		return 0, 0, errors.New("本地已有的下载文件和旧服务器上的迁移包对不上，重新下载")
	default:
		return have, 0, statusError(link, resp.StatusCode)
	}
	if progress != nil {
		progress(have, total)
	}
	buf := make([]byte, 256<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				return have, total, err // 写盘失败（磁盘满等），续传也没用
			}
			have += int64(n)
			if progress != nil {
				progress(have, total)
			}
		}
		if rerr == io.EOF {
			return have, total, nil
		}
		if rerr != nil {
			return have, total, rerr
		}
	}
}

// SendStatusQuiet 通知旧服务器迁入结果，最多等几秒；旧服务器已经关闭时忽略。
func SendStatusQuiet(ctx context.Context, link crypto.Link, st Status) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return SendStatus(ctx, link, st)
}
