package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Container 是容器详情里 DockHop 需要读取的部分；完整配置另外按原始 JSON 保存。
type Container struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	Image string `json:"Image"` // 镜像 ID
	State struct {
		Status  string `json:"Status"`
		Running bool   `json:"Running"`
		Paused  bool   `json:"Paused"`
		Health  *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	Config struct {
		Image       string            `json:"Image"` // 创建时用的镜像名
		Labels      map[string]string `json:"Labels"`
		Env         []string          `json:"Env"`
		StopTimeout *int              `json:"StopTimeout"`
		Healthcheck *struct {
			Test        []string `json:"Test"`
			Interval    int64    `json:"Interval"`
			Timeout     int64    `json:"Timeout"`
			StartPeriod int64    `json:"StartPeriod"`
		} `json:"Healthcheck"`
	} `json:"Config"`
	HostConfig struct {
		NetworkMode    string   `json:"NetworkMode"`
		PidMode        string   `json:"PidMode"`
		IpcMode        string   `json:"IpcMode"`
		Links          []string `json:"Links"`
		VolumesFrom    []string `json:"VolumesFrom"`
		Runtime        string   `json:"Runtime"`
		DeviceRequests []struct {
			Driver       string     `json:"Driver"`
			Capabilities [][]string `json:"Capabilities"`
		} `json:"DeviceRequests"`
		Devices []struct {
			PathOnHost string `json:"PathOnHost"`
		} `json:"Devices"`
		Memory         int64                    `json:"Memory"`
		PortBindings   map[string][]PortBinding `json:"PortBindings"`
		Tmpfs          map[string]string        `json:"Tmpfs"`
		ReadonlyRootfs bool                     `json:"ReadonlyRootfs"`
		AutoRemove     bool                     `json:"AutoRemove"` // docker run --rm
	} `json:"HostConfig"`
	Mounts          []Mount `json:"Mounts"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAMConfig *struct {
				IPv4Address string `json:"IPv4Address"`
				IPv6Address string `json:"IPv6Address"`
			} `json:"IPAMConfig"`
			Aliases []string `json:"Aliases"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// PortBinding 是一个端口映射。
type PortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

// Mount 是容器的一个挂载（运行时视图）。
type Mount struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	Driver      string `json:"Driver"`
	RW          bool   `json:"RW"`
}

// ShortName 返回不带开头斜杠的容器名。
func (c *Container) ShortName() string {
	if len(c.Name) > 0 && c.Name[0] == '/' {
		return c.Name[1:]
	}
	return c.Name
}

// InspectContainer 同时返回解析后的常用字段和完整的原始配置。
func (c *Client) InspectContainer(ctx context.Context, id string) (*Container, json.RawMessage, error) {
	raw, err := c.InspectContainerRaw(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	var ct Container
	if err := json.Unmarshal(raw, &ct); err != nil {
		return nil, nil, fmt.Errorf("读取容器 %s 的配置失败：%v", id, err)
	}
	return &ct, raw, nil
}

// ContainerSize 返回容器可写层的大小（容器里没挂载出来的改动），单位字节。
// Docker 要现场统计，改动多时会慢一些，但不需要停止容器。
func (c *Client) ContainerSize(ctx context.Context, id string) (int64, error) {
	var out struct {
		SizeRw int64 `json:"SizeRw"`
	}
	err := c.Get(ctx, "/containers/"+url.PathEscape(id)+"/json", url.Values{"size": {"1"}}, &out)
	return out.SizeRw, err
}

// StopContainer 停止容器；timeout 为 nil 时用容器自己的停止等待时间。
func (c *Client) StopContainer(ctx context.Context, id string, timeout *time.Duration) error {
	q := url.Values{}
	if timeout != nil {
		q.Set("t", strconv.Itoa(int(timeout.Seconds())))
	}
	err := c.Post(ctx, "/containers/"+url.PathEscape(id)+"/stop", q, nil, nil)
	if e, ok := err.(*Error); ok && e.Status == http.StatusNotModified {
		return nil // 已经停了
	}
	return err
}

// StartContainer 启动容器。
func (c *Client) StartContainer(ctx context.Context, id string) error {
	err := c.Post(ctx, "/containers/"+url.PathEscape(id)+"/start", nil, nil, nil)
	if e, ok := err.(*Error); ok && e.Status == http.StatusNotModified {
		return nil
	}
	return err
}

// PauseContainer、UnpauseContainer 暂停和恢复容器。
func (c *Client) PauseContainer(ctx context.Context, id string) error {
	return c.Post(ctx, "/containers/"+url.PathEscape(id)+"/pause", nil, nil, nil)
}

func (c *Client) UnpauseContainer(ctx context.Context, id string) error {
	return c.Post(ctx, "/containers/"+url.PathEscape(id)+"/unpause", nil, nil, nil)
}

// RemoveContainer 删除容器（force 时连同运行中的一起删）。
func (c *Client) RemoveContainer(ctx context.Context, id string, force bool) error {
	q := url.Values{}
	if force {
		q.Set("force", "1")
	}
	return c.Delete(ctx, "/containers/"+url.PathEscape(id), q)
}

// RemoveContainerAndVolumes 删除容器（连同运行中的），以及 Docker 为它自动创建的
// 匿名卷；用户指定名字挂载的卷不会被删。
func (c *Client) RemoveContainerAndVolumes(ctx context.Context, id string) error {
	return c.Delete(ctx, "/containers/"+url.PathEscape(id), url.Values{"force": {"1"}, "v": {"1"}})
}

// Change 是容器可写层里的一处改动。Kind：0 修改，1 新增，2 删除。
type Change struct {
	Path string `json:"Path"`
	Kind int    `json:"Kind"`
}

// ContainerChanges 返回容器可写层的改动（相当于 docker diff）。
func (c *Client) ContainerChanges(ctx context.Context, id string) ([]Change, error) {
	var out []Change
	err := c.Get(ctx, "/containers/"+url.PathEscape(id)+"/changes", nil, &out)
	return out, err
}

// Commit 把容器当前的文件系统存成镜像（repo:tag），返回镜像 ID。
func (c *Client) Commit(ctx context.Context, id, repo, tag string, pause bool) (string, error) {
	q := url.Values{"container": {id}, "repo": {repo}, "tag": {tag}}
	if !pause {
		q.Set("pause", "0")
	}
	var out struct {
		ID string `json:"Id"`
	}
	err := c.Post(ctx, "/commit", q, nil, &out)
	return out.ID, err
}

// CommitWithConfig 把容器存成镜像，cfg 里给出的字段（如 Entrypoint、Cmd、User）覆盖
// 容器自己的配置；没给出的沿用容器的。返回镜像 ID。注意要清空入口命令时得给空列表 []，
// 给 null 等于没给。
func (c *Client) CommitWithConfig(ctx context.Context, id, repo, tag string, cfg map[string]any) (string, error) {
	q := url.Values{"container": {id}, "repo": {repo}, "tag": {tag}, "pause": {"0"}}
	var out struct {
		ID string `json:"Id"`
	}
	err := c.Post(ctx, "/commit", q, cfg, &out)
	return out.ID, err
}

// CreateContainer 用原始配置创建容器，返回容器 ID。
func (c *Client) CreateContainer(ctx context.Context, name string, spec any) (string, error) {
	q := url.Values{}
	if name != "" {
		q.Set("name", name)
	}
	var out struct {
		ID string `json:"Id"`
	}
	err := c.Post(ctx, "/containers/create", q, spec, &out)
	return out.ID, err
}

// GetArchive 把容器里 path 下的内容打成 tar 流（容器不需要启动，挂载的卷也包括在内）。
func (c *Client) GetArchive(ctx context.Context, id, path string) (io.ReadCloser, error) {
	return c.Stream(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/archive", url.Values{"path": {path}}, nil, "")
}

// PutArchive 把 tar 流解到容器里的 path 下，保留 tar 里记录的属主和权限。
// 注意不能加 copyUIDGID：它的意思是把所有文件改成容器用户的属主，正好相反。
func (c *Client) PutArchive(ctx context.Context, id, path string, r io.Reader) error {
	rc, err := c.Stream(ctx, http.MethodPut, "/containers/"+url.PathEscape(id)+"/archive",
		url.Values{"path": {path}, "noOverwriteDirNonDir": {"true"}}, r, "application/x-tar")
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, rc)
	return rc.Close()
}
