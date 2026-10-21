package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// 迁入用到的写操作：加载和拉取镜像、建网络和卷、改名、连接网络、等待容器结束。

// RenameContainer 给容器改名。
func (c *Client) RenameContainer(ctx context.Context, id, name string) error {
	return c.Post(ctx, "/containers/"+url.PathEscape(id)+"/rename", url.Values{"name": {name}}, nil, nil)
}

// WaitContainer 等容器结束，返回退出码。
func (c *Client) WaitContainer(ctx context.Context, id string) (int, error) {
	var out struct {
		StatusCode int `json:"StatusCode"`
		Error      *struct {
			Message string `json:"Message"`
		} `json:"Error"`
	}
	if err := c.Post(ctx, "/containers/"+url.PathEscape(id)+"/wait", nil, nil, &out); err != nil {
		return -1, err
	}
	if out.Error != nil && out.Error.Message != "" {
		return out.StatusCode, errors.New(out.Error.Message)
	}
	return out.StatusCode, nil
}

// ContainerLogs 返回容器最后几行输出（用于失败时说明原因）。
func (c *Client) ContainerLogs(ctx context.Context, id string, tail int) string {
	rc, err := c.Stream(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/logs",
		url.Values{"stdout": {"1"}, "stderr": {"1"}, "tail": {fmt.Sprint(tail)}}, nil, "")
	if err != nil {
		return ""
	}
	defer rc.Close()
	b, _ := io.ReadAll(io.LimitReader(rc, 64*1024))
	// 没有终端的容器，每段输出前面有 8 个字节的头；去掉不可见字符即可阅读。
	var out strings.Builder
	for len(b) >= 8 && (b[0] == 1 || b[0] == 2) && b[1] == 0 && b[2] == 0 && b[3] == 0 {
		n := int(b[4])<<24 | int(b[5])<<16 | int(b[6])<<8 | int(b[7])
		b = b[8:]
		if n > len(b) {
			n = len(b)
		}
		out.Write(b[:n])
		b = b[n:]
	}
	out.Write(b)
	return strings.TrimSpace(out.String())
}

// ContainersByLabel 列出带某个标签（key=value）的全部容器。
func (c *Client) ContainersByLabel(ctx context.Context, label string) ([]ContainerSummary, error) {
	f, _ := json.Marshal(map[string][]string{"label": {label}})
	var out []ContainerSummary
	err := c.Get(ctx, "/containers/json", url.Values{"all": {"1"}, "filters": {string(f)}}, &out)
	return out, err
}

// InspectImageRaw 返回镜像的完整信息（原始 JSON）。
func (c *Client) InspectImageRaw(ctx context.Context, ref string) (json.RawMessage, error) {
	var raw json.RawMessage
	err := c.Get(ctx, "/images/"+ref+"/json", nil, &raw)
	return raw, err
}

// LoadImages 加载 docker save 导出的镜像（相当于 docker load）。
func (c *Client) LoadImages(ctx context.Context, r io.Reader) error {
	rc, err := c.Stream(ctx, http.MethodPost, "/images/load", url.Values{"quiet": {"1"}}, r, "application/x-tar")
	if err != nil {
		return err
	}
	defer rc.Close()
	return jsonStreamError(rc)
}

// PullImage 拉取镜像（ref 可以带 @sha256 摘要），没有登录信息，只用于公开镜像。
func (c *Client) PullImage(ctx context.Context, ref string) error {
	rc, err := c.Stream(ctx, http.MethodPost, "/images/create", url.Values{"fromImage": {ref}}, nil, "")
	if err != nil {
		return err
	}
	defer rc.Close()
	return jsonStreamError(rc)
}

// TagImage 给镜像加一个名字（repo:tag）。
func (c *Client) TagImage(ctx context.Context, id, repo, tag string) error {
	return c.Post(ctx, "/images/"+id+"/tag", url.Values{"repo": {repo}, "tag": {tag}}, nil, nil)
}

// DistributionInspect 向镜像仓库查询镜像信息，只查询、不下载。用来确认按摘要能不能拉到。
func (c *Client) DistributionInspect(ctx context.Context, ref string) error {
	return c.Get(ctx, "/distribution/"+ref+"/json", nil, nil)
}

// jsonStreamError 读完 Docker 的逐行 JSON 输出，返回其中的错误。
func jsonStreamError(r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		var m struct {
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if m.ErrorDetail.Message != "" {
			return errors.New(m.ErrorDetail.Message)
		}
		if m.Error != "" {
			return errors.New(m.Error)
		}
	}
	return sc.Err()
}

// Network 是网络详情里 DockHop 需要读取的部分。
type Network struct {
	Name       string            `json:"Name"`
	ID         string            `json:"Id"`
	Driver     string            `json:"Driver"`
	Internal   bool              `json:"Internal"`
	Attachable bool              `json:"Attachable"`
	EnableIPv6 bool              `json:"EnableIPv6"`
	Options    map[string]string `json:"Options"`
	Labels     map[string]string `json:"Labels"`
	IPAM       struct {
		Driver  string            `json:"Driver"`
		Options map[string]string `json:"Options"`
		Config  []IPAMConfig      `json:"Config"`
	} `json:"IPAM"`
	Containers map[string]struct {
		Name        string `json:"Name"`
		IPv4Address string `json:"IPv4Address"`
	} `json:"Containers"`
}

// IPAMConfig 是网络的一个网段。
type IPAMConfig struct {
	Subnet             string            `json:"Subnet,omitempty"`
	IPRange            string            `json:"IPRange,omitempty"`
	Gateway            string            `json:"Gateway,omitempty"`
	AuxiliaryAddresses map[string]string `json:"AuxiliaryAddresses,omitempty"`
}

// InspectNetwork 读取网络详情。
func (c *Client) InspectNetwork(ctx context.Context, name string) (*Network, error) {
	var n Network
	if err := c.Get(ctx, "/networks/"+url.PathEscape(name), nil, &n); err != nil {
		return nil, err
	}
	return &n, nil
}

// CreateNetwork 创建网络，返回网络 ID。
func (c *Client) CreateNetwork(ctx context.Context, spec any) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	err := c.Post(ctx, "/networks/create", nil, spec, &out)
	return out.ID, err
}

// RemoveNetwork 删除网络。
func (c *Client) RemoveNetwork(ctx context.Context, name string) error {
	return c.Delete(ctx, "/networks/"+url.PathEscape(name), nil)
}

// ConnectNetwork 把容器连到网络上；endpoint 是固定 IP、别名等设置，可以为 nil。
func (c *Client) ConnectNetwork(ctx context.Context, network, container string, endpoint any) error {
	body := map[string]any{"Container": container}
	if endpoint != nil {
		body["EndpointConfig"] = endpoint
	}
	return c.Post(ctx, "/networks/"+url.PathEscape(network)+"/connect", nil, body, nil)
}

// DisconnectNetwork 把容器从网络上断开。
func (c *Client) DisconnectNetwork(ctx context.Context, network, container string) error {
	return c.Post(ctx, "/networks/"+url.PathEscape(network)+"/disconnect", nil,
		map[string]any{"Container": container, "Force": true}, nil)
}

// Volume 是卷详情里 DockHop 需要读取的部分。
type Volume struct {
	Name    string            `json:"Name"`
	Driver  string            `json:"Driver"`
	Options map[string]string `json:"Options"`
	Labels  map[string]string `json:"Labels"`
}

// InspectVolume 读取卷详情。
func (c *Client) InspectVolume(ctx context.Context, name string) (*Volume, error) {
	var v Volume
	if err := c.Get(ctx, "/volumes/"+url.PathEscape(name), nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// CreateVolume 创建卷。
func (c *Client) CreateVolume(ctx context.Context, name, driver string, opts, labels map[string]string) error {
	body := map[string]any{"Name": name, "Driver": driver, "DriverOpts": opts, "Labels": labels}
	return c.Post(ctx, "/volumes/create", nil, body, nil)
}

// RemoveVolume 删除卷。
func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	return c.Delete(ctx, "/volumes/"+url.PathEscape(name), nil)
}
