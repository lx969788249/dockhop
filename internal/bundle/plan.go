package bundle

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/lx969788249/dockhop/internal/crypto"
)

// Plan 是迁移计划：新服务器在下载迁移包之前，先拿它检查自己能不能迁入。
// 内容只有“迁移需要什么”，没有任何业务数据；同样用链接里的密钥加密。
type Plan struct {
	Version    int             `json:"version"`
	Source     Source          `json:"source"`
	Sizes      PlanSizes       `json:"sizes"`
	Containers []PlanContainer `json:"containers"`
	Projects   []PlanProject   `json:"projects,omitempty"`
	Volumes    []PlanVolume    `json:"volumes,omitempty"`
	Binds      []PlanBind      `json:"binds,omitempty"`
	Networks   []PlanNetwork   `json:"networks,omitempty"`
}

// PlanSizes 是各部分解开后的大小（字节），新服务器据此检查磁盘空间。
type PlanSizes struct {
	Bundle  int64 `json:"bundle"` // 迁移包文件
	Images  int64 `json:"images"`
	Volumes int64 `json:"volumes"`
	Binds   int64 `json:"binds"`
}

// PlanContainer 是预检需要的容器信息。
type PlanContainer struct {
	Name         string        `json:"name"`
	Project      string        `json:"project,omitempty"`
	State        string        `json:"state"`
	Image        string        `json:"image"`
	DigestPinned bool          `json:"digest_pinned,omitempty"`
	NetworkMode  string        `json:"network_mode"`
	Runtime      string        `json:"runtime,omitempty"`
	GPU          bool          `json:"gpu,omitempty"`
	Devices      []string      `json:"devices,omitempty"`
	Memory       int64         `json:"memory,omitempty"`
	Ports        []PlanPort    `json:"ports,omitempty"`
	Networks     []PlanNetAddr `json:"networks,omitempty"`
	Volumes      []string      `json:"volumes,omitempty"`
	Binds        []string      `json:"binds,omitempty"`
}

// PlanPort 是一个端口映射。
type PlanPort struct {
	HostIP        string `json:"host_ip,omitempty"`
	HostPort      string `json:"host_port"`
	ContainerPort string `json:"container_port"`
	Proto         string `json:"proto"`
}

// PlanNetAddr 是容器在一个网络里的固定 IP（没有固定时为空）。
type PlanNetAddr struct {
	Name string `json:"name"`
	IP   string `json:"ip,omitempty"`
}

// PlanProject 是一个 Compose 项目。
type PlanProject struct {
	Name     string   `json:"name"`
	Services []string `json:"services"`
}

// PlanVolume 是一个数据卷。
type PlanVolume struct {
	Name   string            `json:"name"`
	Driver string            `json:"driver"`
	Opts   map[string]string `json:"opts,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
	Bytes  int64             `json:"bytes"`
}

// PlanBind 是一个宿主机目录或文件。
type PlanBind struct {
	Path  string   `json:"path"`
	Link  string   `json:"link,omitempty"`
	Links []string `json:"links,omitempty"` // 全部软链接（1.0.5 起）
	Bytes int64    `json:"bytes"`
}

// AllLinks 返回全部软链接；老版本的迁移计划只有 Link。
func (b PlanBind) AllLinks() []string {
	if len(b.Links) > 0 {
		return b.Links
	}
	if b.Link != "" {
		return []string{b.Link}
	}
	return nil
}

// PlanNetwork 是一个自定义网络。
type PlanNetwork struct {
	Name    string   `json:"name"`
	Driver  string   `json:"driver"`
	Subnets []string `json:"subnets,omitempty"`
	Compose bool     `json:"compose,omitempty"` // 由 Compose 创建，迁入时交给 Compose
}

// EncryptPlan 把迁移计划加密后写到 w。
func EncryptPlan(w io.Writer, link crypto.Link, p *Plan) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	ew, err := link.Encrypt(w)
	if err != nil {
		return err
	}
	if _, err := ew.Write(b); err != nil {
		return err
	}
	return ew.Close()
}

// DecryptPlan 解密并读出迁移计划。
func DecryptPlan(r io.Reader, link crypto.Link) (*Plan, error) {
	dr, err := link.Decrypt(r)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, io.LimitReader(dr, 64<<20)); err != nil {
		return nil, err
	}
	var p Plan
	if err := json.Unmarshal(buf.Bytes(), &p); err != nil {
		return nil, err
	}
	return &p, nil
}
