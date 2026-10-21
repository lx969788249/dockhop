package docker

import (
	"context"
	"encoding/json"
	"net/url"
)

// Info 是 Docker 守护进程的概况，只取 DockHop 用得到的字段。
type Info struct {
	ServerVersion   string                     `json:"ServerVersion"`
	OperatingSystem string                     `json:"OperatingSystem"`
	Architecture    string                     `json:"Architecture"`
	NCPU            int                        `json:"NCPU"`
	MemTotal        int64                      `json:"MemTotal"`
	Driver          string                     `json:"Driver"`
	DockerRootDir   string                     `json:"DockerRootDir"`
	CgroupVersion   string                     `json:"CgroupVersion"`
	LoggingDriver   string                     `json:"LoggingDriver"`
	DefaultRuntime  string                     `json:"DefaultRuntime"`
	Runtimes        map[string]json.RawMessage `json:"Runtimes"`
	Containers      int                        `json:"Containers"`
	Images          int                        `json:"Images"`
	Swarm           struct {
		LocalNodeState string `json:"LocalNodeState"`
	} `json:"Swarm"`
	Plugins struct {
		Volume []string `json:"Volume"`
	} `json:"Plugins"`
}

// Info 返回 Docker 的概况。
func (c *Client) Info(ctx context.Context) (Info, error) {
	var i Info
	err := c.Get(ctx, "/info", nil, &i)
	return i, err
}

// ContainerSummary 是容器列表里的一项。
type ContainerSummary struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Status string            `json:"Status"`
	Labels map[string]string `json:"Labels"`
	Ports  []struct {
		IP          string `json:"IP"`
		PrivatePort int    `json:"PrivatePort"`
		PublicPort  int    `json:"PublicPort"`
		Type        string `json:"Type"`
	} `json:"Ports"`
}

// Name 返回不带开头斜杠的容器名。
func (s ContainerSummary) Name() string {
	if len(s.Names) == 0 {
		return s.ID[:12]
	}
	n := s.Names[0]
	if len(n) > 0 && n[0] == '/' {
		n = n[1:]
	}
	return n
}

// Containers 列出容器；all 为 true 时包括已停止的。
func (c *Client) Containers(ctx context.Context, all bool) ([]ContainerSummary, error) {
	q := url.Values{}
	if all {
		q.Set("all", "1")
	}
	var out []ContainerSummary
	err := c.Get(ctx, "/containers/json", q, &out)
	return out, err
}

// InspectContainerRaw 返回容器的完整配置（原始 JSON），供原样重建。
func (c *Client) InspectContainerRaw(ctx context.Context, id string) (json.RawMessage, error) {
	var raw json.RawMessage
	err := c.Get(ctx, "/containers/"+url.PathEscape(id)+"/json", nil, &raw)
	return raw, err
}

// NetworkSummary 是网络列表里的一项。
type NetworkSummary struct {
	Name   string `json:"Name"`
	ID     string `json:"Id"`
	Driver string `json:"Driver"`
	Scope  string `json:"Scope"`
	IPAM   struct {
		Config []struct {
			Subnet  string `json:"Subnet"`
			Gateway string `json:"Gateway"`
		} `json:"Config"`
	} `json:"IPAM"`
}

// Networks 列出网络。
func (c *Client) Networks(ctx context.Context) ([]NetworkSummary, error) {
	var out []NetworkSummary
	err := c.Get(ctx, "/networks", nil, &out)
	return out, err
}

// VolumeSummary 是卷列表里的一项。
type VolumeSummary struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Mountpoint string            `json:"Mountpoint"`
	Labels     map[string]string `json:"Labels"`
}

// Volumes 列出卷。
func (c *Client) Volumes(ctx context.Context) ([]VolumeSummary, error) {
	var out struct {
		Volumes []VolumeSummary `json:"Volumes"`
	}
	err := c.Get(ctx, "/volumes", nil, &out)
	return out.Volumes, err
}
