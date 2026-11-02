// Package bundle 定义迁移包的格式并负责读写。
//
// 迁移包是一条数据流：tar → zstd 压缩 → age 加密，边打包边写成最终文件，不需要
// 先在磁盘上放一份未加密的目录。事先不知道大小的数据（卷、目录、镜像导出）按
// 16MB 分段写成 前缀/part-000001、part-000002……，内存占用固定。
//
// 包里的内容：
//
//	containers/<名字>.json     容器的完整配置（docker inspect 原样）
//	networks/<名字>.json       自定义网络的完整配置
//	volumes/<名字>.json        数据卷的完整配置；volumes/<名字>/part-*  卷里的数据（tar）
//	binds/<编号>/part-*        宿主机目录或文件（tar，路径相对于根目录）
//	changes/<容器名>/part-*     容器里没挂载出来的改动（tar，路径相对于根目录）
//	compose/<项目>/dir/part-*      Compose 项目目录（tar，路径相对于项目目录）
//	compose/<项目>/outside/part-*  项目目录以外被引用的文件（tar，路径相对于根目录）
//	manifest.json              清单，最后写入
//
// 迁移计划不在包里，由下载服务单独提供（新服务器下载之前先用它做检查）。
//
// 镜像也不在包里（格式版本 2 起）：每个镜像单独导出成一个文件（docker save → zstd 压缩
// → age 加密，见 image.go），由下载服务单独提供。新服务器本机已经有、或者能从镜像仓库
// 按摘要拉到一模一样的镜像时，就不用从旧服务器下载，省下旧服务器的上行带宽。格式版本 1
// 的迁移包把所有镜像放在包里的 images/part-* 下，新版本仍然能读。
package bundle

import "time"

const (
	// Format 和 Version 写在清单里，新服务器据此确认能读懂这个迁移包。
	Format  = "dockhop-bundle"
	Version = 2
	// PartSize 是分段大小。
	PartSize = 16 << 20
)

// Manifest 是迁移包的清单。
type Manifest struct {
	Format     string      `json:"format"`
	Version    int         `json:"version"`
	CreatedAt  time.Time   `json:"created_at"`
	DockHop    string      `json:"dockhop"`
	Source     Source      `json:"source"`
	Containers []Container `json:"containers"`
	Projects   []Project   `json:"projects,omitempty"`
	Networks   []string    `json:"networks,omitempty"`
	Volumes    []Volume    `json:"volumes,omitempty"`
	Binds      []Bind      `json:"binds,omitempty"`
	Images     Images      `json:"images"`
	ImageFiles []ImageFile `json:"image_files,omitempty"` // 单独提供的镜像文件（格式版本 2 起）
}

// Source 是旧服务器的情况。迁入时用来判断哪些设置是旧服务器 Docker 自动填的默认值。
type Source struct {
	Hostname       string `json:"hostname"`
	Docker         string `json:"docker"`
	API            string `json:"api"`
	Arch           string `json:"arch"`
	OS             string `json:"os"`
	CgroupVersion  string `json:"cgroup_version"`
	LoggingDriver  string `json:"logging_driver"`
	DefaultRuntime string `json:"default_runtime"`
	StorageDriver  string `json:"storage_driver"`
	Compose        string `json:"compose,omitempty"`
}

// Container 是迁移的一个容器。
type Container struct {
	Name    string `json:"name"`
	ID      string `json:"id"`
	Image   string `json:"image"`    // 创建时用的镜像名
	ImageID string `json:"image_id"` // 镜像 ID
	// OldImage：旧服务器上镜像名已经指向别的版本（或已删除）时，容器实际用的旧版本镜像
	// 临时起的名字（dockhop-image:<ID 前 12 位>），迁入时用它创建容器。
	OldImage string   `json:"old_image,omitempty"`
	Changes  *Changes `json:"changes,omitempty"`
	Project  string   `json:"project,omitempty"`
	Service  string   `json:"service,omitempty"`
	State    string   `json:"state"` // running、paused、exited、created……
}

// Changes 是容器里没挂载出来的改动（docker diff）：新增和修改的文件在 changes/<容器名> 下，
// 删除的路径列在 Deleted 里。不用 docker commit 加 docker save：新版 Docker（containerd
// 存储）导出 commit 出来的镜像时只有清单，没有内容（第 3 阶段实测 Docker 29.2）。
type Changes struct {
	Paths   []string `json:"paths"`
	Deleted []string `json:"deleted,omitempty"`
	Bytes   int64    `json:"bytes"`
	Parts   int      `json:"parts"`
}

// Project 是迁移的一个 Compose 项目，整组迁移。
type Project struct {
	Name        string   `json:"name"`
	WorkingDir  string   `json:"working_dir"`
	ConfigFiles []string `json:"config_files"`          // 绝对路径
	ConfigArgs  []string `json:"config_args,omitempty"` // 启动时 -f 的原样写法（可能是相对于项目目录的路径）
	Services    []string `json:"services"`
	Outside     []string `json:"outside,omitempty"`      // 项目目录以外一起迁移的文件
	Skipped     []string `json:"skipped,omitempty"`      // 太大没有迁移的文件
	ConfigDrift []string `json:"config_drift,omitempty"` // 现在的文件和运行中的容器对不上的服务
	DirParts    int      `json:"dir_parts"`
	OutParts    int      `json:"outside_parts,omitempty"`
}

// Volume 是迁移的一个数据卷。
type Volume struct {
	Name      string `json:"name"`
	Driver    string `json:"driver"`
	Anonymous bool   `json:"anonymous,omitempty"`
	Bytes     int64  `json:"bytes"`
	Parts     int    `json:"parts"`
}

// Bind 是迁移的一个宿主机目录或文件。
type Bind struct {
	ID    string     `json:"id"`
	Path  string     `json:"path"`            // 真实路径
	Link  string     `json:"link,omitempty"`  // 挂载时用的一个指向 Path 的软链接（老版本只认这一个）
	Links []BindLink `json:"links,omitempty"` // 挂载时用到的全部软链接（1.0.5 起）
	Dir   bool       `json:"dir"`
	Bytes int64      `json:"bytes"`
	Parts int        `json:"parts"`
}

// BindLink 是容器挂载时用的一个软链接：Link 指向 Target（就是 Path 或它里面的目录）。
type BindLink struct {
	Link   string `json:"link"`
	Target string `json:"target"`
}

// AllLinks 返回全部软链接；老版本的迁移包只有 Link。
func (b Bind) AllLinks() []BindLink {
	if len(b.Links) > 0 {
		return b.Links
	}
	if b.Link != "" {
		return []BindLink{{Link: b.Link, Target: b.Path}}
	}
	return nil
}

// Images 是迁移用到的镜像。Bytes 是导出的总字节数（压缩前）；Parts 是格式版本 1 放在包里
// 的分段数，版本 2 起镜像单独成文件（见 ImageFile），Parts 为 0。
type Images struct {
	Refs  map[string]string `json:"refs"` // 镜像名 → 镜像 ID
	Bytes int64             `json:"bytes"`
	Parts int               `json:"parts"`
}

// ImageFile 是单独提供下载的一个镜像文件：同一个镜像（同一个 ID）的所有名字一起导出。
type ImageFile struct {
	File  string   `json:"file"`           // 下载服务上的文件名，例如 image-1
	ID    string   `json:"id"`             // 旧服务器上的镜像 ID
	Refs  []string `json:"refs"`           // 容器用的镜像名，导出时一起带上
	Pull  string   `json:"pull,omitempty"` // 能从镜像仓库拉到同一个镜像的引用（名字@sha256:摘要）；自己构建的镜像没有
	Bytes int64    `json:"bytes"`          // 文件大小（压缩、加密后）
	Size  int64    `json:"size"`           // 导出的字节数（压缩前），加载时显示进度用
}
