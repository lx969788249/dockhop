// Package preflight 是迁入前检查：下载迁移包、改动新服务器之前，根据迁移计划找出
// 会让迁入失败的问题，让用户针对性地选择怎么处理（换端口、跳过项目、自动分配 IP……）。
//
// 问题分三级：
//
//	Info  只提示，不停下（例如新服务器的 Docker 比旧服务器旧）
//	Ask   会导致迁入失败但可以处理，让用户选择（例如端口被占用）
//	Stop  无法继续（例如 CPU 类型不同）
//
// 用户的选择记在 Decisions 里，迁入时按它执行。检查本身（Evaluate）只读数据、
// 不访问 Docker，方便测试；Docker 和本机的情况由 Collect 读出来。
package preflight

import (
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/spec"
	"github.com/lx969788249/dockhop/internal/ui"
)

// Level 是问题的严重程度。
type Level int

const (
	Info Level = iota
	Ask
	Stop
)

// Issue 是检查发现的一个问题。
type Issue struct {
	Level     Level
	Kind      string
	Title     string
	Hint      string
	Container string
	Project   string
	Network   string
	Volume    string
	Image     string
	Port      *PortRef
	Suggest   string // 推荐的空闲端口
}

// PortRef 指向迁移计划里的一个端口映射。
type PortRef struct {
	Container     string `json:"container"`
	HostIP        string `json:"host_ip"`
	HostPort      string `json:"host_port"`
	ContainerPort string `json:"container_port"`
	Proto         string `json:"proto"`
}

// PortChange 是对一个端口映射的调整：换端口、改绑到所有地址或不再对外开放。
type PortChange struct {
	PortRef
	NewIP   string `json:"new_ip"`
	NewPort string `json:"new_port"`
	Drop    bool   `json:"drop,omitempty"`
}

// NetRef 是容器在某个网络里。
type NetRef struct {
	Container string `json:"container"`
	Network   string `json:"network"`
}

// Skip 记录跳过了什么、为什么，显示在结果里。
type Skip struct {
	Project   string `json:"project,omitempty"`
	Container string `json:"container,omitempty"`
	Reason    string `json:"reason"`
}

// Decisions 是用户（或 --yes）针对检查结果做的选择。
type Decisions struct {
	SkipProjects      []string     `json:"skip_projects,omitempty"`
	SkipContainers    []string     `json:"skip_containers,omitempty"`
	ReplaceProjects   []string     `json:"replace_projects,omitempty"`
	ReplaceContainers []string     `json:"replace_containers,omitempty"`
	KeepVolumes       []string     `json:"keep_volumes,omitempty"`
	AutoSubnet        []string     `json:"auto_subnet,omitempty"`
	ReuseNetwork      []string     `json:"reuse_network,omitempty"`
	AutoIP            []NetRef     `json:"auto_ip,omitempty"`
	Ports             []PortChange `json:"ports,omitempty"`
	BundleImages      []string     `json:"bundle_images,omitempty"` // 改用迁移包里的镜像（去掉 @sha256）
	Workdir           string       `json:"workdir,omitempty"`
	Skipped           []Skip       `json:"skipped,omitempty"`
}

func has(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func add(list []string, v string) []string {
	if has(list, v) {
		return list
	}
	return append(list, v)
}

// Skipped 表示容器（或它所在的项目）是否按选择跳过。
func (d *Decisions) SkippedContainer(c bundle.PlanContainer) bool {
	return has(d.SkipContainers, c.Name) || (c.Project != "" && has(d.SkipProjects, c.Project))
}

// AutoIPFor 表示容器在这个网络里是否改为自动分配 IP（单独选择、网段改为自动、沿用已有网络时都是）。
func (d *Decisions) AutoIPFor(container, network string) bool {
	for _, r := range d.AutoIP {
		if r.Container == container && r.Network == network {
			return true
		}
	}
	return has(d.AutoSubnet, network) || has(d.ReuseNetwork, network)
}

// PortFor 返回端口映射的调整；没有调整时 ok 为 false。
func (d *Decisions) PortFor(r PortRef) (PortChange, bool) {
	for _, p := range d.Ports {
		if p.PortRef == r {
			return p, true
		}
	}
	return PortChange{}, false
}

// UseBundleImage 表示这个 digest 固定的镜像是否改用迁移包里的镜像。
func (d *Decisions) UseBundleImage(ref string) bool { return has(d.BundleImages, ref) }

// KeepVolume 表示是否保留新服务器上已有的这个卷、不写入迁移过来的数据。
func (d *Decisions) KeepVolume(name string) bool { return has(d.KeepVolumes, name) }

// ReuseExisting 表示是否沿用新服务器上已有的同名网络。
func (d *Decisions) ReuseExisting(network string) bool { return has(d.ReuseNetwork, network) }

// AutoSubnetFor 表示网络是否改为让 Docker 自动选择网段。
func (d *Decisions) AutoSubnetFor(network string) bool { return has(d.AutoSubnet, network) }

// Options 是检查的选项。
type Options struct {
	Existing     string // replace、skip、fail；空表示有同名服务时询问
	BaseDir      string // 存放下载和解压文件的目录
	BundleOnDisk bool   // 迁移包已经在本机（--file），不需要下载
	Home         string // DockHop 自己的目录，挂载目录不能和它重叠
}

var archAlias = map[string]string{"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64",
	"armv7l": "arm", "armhf": "arm", "arm": "arm"}

// Result 是一轮检查的结果。
type Result struct {
	Issues     []Issue
	Containers int   // 要迁入的容器数（去掉跳过的）
	NeedBytes  int64 // 需要的磁盘空间
}

// Evaluate 根据迁移计划和新服务器的情况找出问题。
func Evaluate(plan *bundle.Plan, t *Target, d *Decisions, o Options) *Result {
	res := &Result{}
	addIssue := func(i Issue) { res.Issues = append(res.Issues, i) }

	var containers []bundle.PlanContainer
	for _, c := range plan.Containers {
		if !d.SkippedContainer(c) {
			containers = append(containers, c)
		}
	}
	res.Containers = len(containers)
	planNames := map[string]bool{}
	usedVolumes := map[string]bool{}
	usedBinds := map[string]bool{}
	hasProject := false
	for _, c := range containers {
		planNames[c.Name] = true
		for _, v := range c.Volumes {
			usedVolumes[v] = true
		}
		for _, b := range c.Binds {
			usedBinds[b] = true
		}
		if c.Project != "" {
			hasProject = true
		}
	}

	// ---------- Docker 环境 ----------
	src, dst := plan.Source.Arch, t.Arch
	if src != "" && dst != "" && alias(src) != alias(dst) {
		addIssue(Issue{Level: Stop, Kind: "arch",
			Title: fmt.Sprintf("两台服务器的 CPU 类型不同：旧服务器是 %s，新服务器是 %s", src, dst),
			Hint:  "容器镜像只能在相同 CPU 类型的服务器上运行，请换一台和旧服务器相同类型的服务器"})
	}
	if older(t.DockerVersion, plan.Source.Docker) {
		addIssue(Issue{Level: Info, Kind: "docker_version",
			Title: fmt.Sprintf("新服务器的 Docker（%s）比旧服务器（%s）旧，一般不影响迁移", t.DockerVersion, plan.Source.Docker)})
	}
	if hasProject && !t.HasCompose {
		addIssue(Issue{Level: Stop, Kind: "compose", Title: "要迁移 Compose 项目，但新服务器没有安装 Docker Compose",
			Hint: "先安装 Docker Compose（例如 apt install docker-compose-plugin）再重试"})
	}
	if hasProject && t.HasCompose && strings.HasPrefix(t.Compose, "1.") && !strings.HasPrefix(plan.Source.Compose, "1.") && plan.Source.Compose != "" {
		addIssue(Issue{Level: Info, Kind: "old_compose",
			Title: fmt.Sprintf("新服务器的 docker-compose 是老版本 %s，旧服务器用的是 Compose %s；有些写法老版本读不懂，迁入时可能失败（失败会自动还原）。"+
				"建议先在新服务器安装新版 Docker Compose", t.Compose, plan.Source.Compose)})
	}
	if !t.Local && (len(plan.Binds) > 0 || hasProject) {
		addIssue(Issue{Level: Stop, Kind: "remote_docker",
			Title: "Docker 不在这台服务器上（DOCKER_HOST 指向了别的机器），没法把目录和 Compose 项目文件放到 Docker 所在的服务器",
			Hint:  "请在 Docker 所在的服务器上直接运行 DockHop"})
	}
	for _, c := range containers {
		if c.Runtime != "" && len(t.Runtimes) > 0 && !t.Runtimes[c.Runtime] {
			addIssue(Issue{Level: Stop, Kind: "runtime", Container: c.Name,
				Title: fmt.Sprintf("容器 %s 需要 %s 运行环境，新服务器没有安装", c.Name, c.Runtime),
				Hint:  "先在新服务器安装它（例如 NVIDIA 显卡需要安装 NVIDIA Container Toolkit）再重试"})
		}
		if c.GPU && !t.Runtimes["nvidia"] && !t.Exists("/dev/nvidiactl") {
			addIssue(Issue{Level: Stop, Kind: "gpu", Container: c.Name,
				Title: fmt.Sprintf("容器 %s 要用显卡，新服务器没有 NVIDIA 显卡运行环境", c.Name),
				Hint:  "确认新服务器有显卡，并安装驱动和 NVIDIA Container Toolkit 后重试"})
		}
		for _, dev := range c.Devices {
			if dev != "" && !t.Exists(dev) {
				addIssue(Issue{Level: Stop, Kind: "device", Container: c.Name,
					Title: fmt.Sprintf("容器 %s 需要设备 %s，新服务器上没有", c.Name, dev),
					Hint:  "确认新服务器有这个硬件并已安装驱动"})
			}
		}
	}
	for _, v := range plan.Volumes {
		if !usedVolumes[v.Name] {
			continue
		}
		if v.Driver != "" && v.Driver != "local" && len(t.VolumePlugins) > 0 && !t.VolumePlugins[v.Driver] {
			addIssue(Issue{Level: Stop, Kind: "volume_plugin", Volume: v.Name,
				Title: fmt.Sprintf("数据卷 %s 使用 %s 插件存储，新服务器没有安装这个插件", v.Name, v.Driver),
				Hint:  "先在新服务器安装 " + v.Driver + " 卷插件再重试"})
		}
	}
	var memNeed int64
	for _, c := range containers {
		memNeed += c.Memory
	}
	if t.MemTotal > 0 && memNeed > t.MemTotal {
		addIssue(Issue{Level: Info, Kind: "memory", Title: fmt.Sprintf("容器设置的内存上限合计 %s，超过新服务器的内存 %s，内存紧张时容器可能被系统结束",
			ui.HumanBytes(memNeed), ui.HumanBytes(t.MemTotal))})
	}

	// ---------- 挂载目录不能盖住 DockHop 自己的文件 ----------
	if o.Home != "" {
		for _, b := range plan.Binds {
			for _, p := range []string{b.Path, b.Link} {
				if p == "" || spec.IsSystemPath(p) {
					continue
				}
				if spec.PathsOverlap(normalize(p), normalize(o.Home)) {
					addIssue(Issue{Level: Stop, Kind: "bind_home",
						Title: fmt.Sprintf("挂载目录 %s 和 DockHop 存放迁移文件、回滚资料的目录 %s 重叠，迁入会把它们覆盖", p, o.Home),
						Hint:  "设置 DOCKHOP_HOME=其他目录（例如 /var/tmp/dockhop）后重新运行"})
				}
			}
		}
	}

	// ---------- 磁盘空间 ----------
	res.NeedBytes = diskIssues(plan, t, d, o, usedVolumes, usedBinds, addIssue)

	// ---------- 已有的同名容器、Compose 项目、数据卷 ----------
	existingTargets := 0
	for _, c := range containers {
		if c.Project != "" {
			continue
		}
		tc, ok := t.Containers[c.Name]
		if !ok {
			continue
		}
		existingTargets++
		if has(d.ReplaceContainers, c.Name) {
			continue
		}
		switch o.Existing {
		case "fail":
			addIssue(Issue{Level: Stop, Kind: "existing_container", Container: c.Name,
				Title: "新服务器上已经有同名容器 " + c.Name + "（--existing fail 不允许替换）",
				Hint:  "删除这个容器，或者去掉 --existing fail"})
		case "replace":
			addIssue(Issue{Level: Info, Kind: "existing_container", Container: c.Name,
				Title: "新服务器上已有同名容器 " + c.Name + "，会被替换（迁入失败时自动还原）"})
		case "skip":
			addIssue(Issue{Level: Info, Kind: "existing_container", Container: c.Name,
				Title: "新服务器上已有同名容器 " + c.Name + "，保留它、不迁入这个容器"})
		default:
			if tc.Project != "" {
				addIssue(Issue{Level: Ask, Kind: "existing_container", Container: c.Name,
					Title: fmt.Sprintf("新服务器上已经有同名容器 %s（属于 Compose 项目 %s）", c.Name, tc.Project)})
			} else {
				addIssue(Issue{Level: Ask, Kind: "existing_container", Container: c.Name,
					Title: "新服务器上已经有同名容器 " + c.Name})
			}
		}
	}
	for _, p := range projectNames(containers) {
		if !t.Projects[p] {
			continue
		}
		existingTargets++
		if has(d.ReplaceProjects, p) {
			continue
		}
		switch o.Existing {
		case "fail":
			addIssue(Issue{Level: Stop, Kind: "existing_project", Project: p,
				Title: "新服务器上已经有同名的 Compose 项目 " + p + "（--existing fail 不允许替换）",
				Hint:  "删除这个项目的容器，或者去掉 --existing fail"})
		case "replace":
			addIssue(Issue{Level: Info, Kind: "existing_project", Project: p,
				Title: "新服务器上已有同名的 Compose 项目 " + p + "，会被替换（迁入失败时自动还原）"})
		case "skip":
			addIssue(Issue{Level: Info, Kind: "existing_project", Project: p,
				Title: "新服务器上已有同名的 Compose 项目 " + p + "，保留它、不迁入这个项目"})
		default:
			addIssue(Issue{Level: Ask, Kind: "existing_project", Project: p,
				Title: "新服务器上已经有同名的 Compose 项目 " + p})
		}
	}
	if o.Existing == "skip" && existingTargets > 0 && (len(plan.Volumes) > 0 || len(plan.Binds) > 0) {
		addIssue(Issue{Level: Stop, Kind: "skip_with_data",
			Title: "--existing skip 时无法判断共用的数据属于谁；迁移包里有数据卷或目录，为了不覆盖已有数据，已停止",
			Hint:  "改用 --existing replace（失败时自动还原），或者先处理新服务器上的同名服务"})
	}
	for _, v := range plan.Volumes {
		if !usedVolumes[v.Name] || d.KeepVolume(v.Name) || spec.IsAnonymousVolume(v.Name) {
			continue
		}
		cur, ok := t.Volumes[v.Name]
		if !ok {
			continue
		}
		if driverOf(cur.Driver) != driverOf(v.Driver) || !sameMap(cur.Options, v.Opts) {
			addIssue(Issue{Level: Ask, Kind: "volume_mismatch", Volume: v.Name,
				Title: "新服务器已有同名数据卷 " + v.Name + "，但它的存储方式和旧服务器不同"})
		} else {
			addIssue(Issue{Level: Info, Kind: "volume_replace", Volume: v.Name,
				Title: "数据卷 " + v.Name + " 已存在，迁移过来的数据会替换它（失败时自动还原）"})
		}
	}

	networkIssues(plan, t, d, containers, planNames, addIssue)
	portIssues(t, d, containers, planNames, o, addIssue)

	// ---------- digest 固定的镜像 ----------
	seen := map[string]bool{}
	for _, c := range containers {
		if !c.DigestPinned || seen[c.Image] || d.UseBundleImage(c.Image) {
			continue
		}
		seen[c.Image] = true
		if t.Pullable != nil && !t.Pullable(c.Image) {
			addIssue(Issue{Level: Ask, Kind: "digest", Image: c.Image, Container: c.Name,
				Title: fmt.Sprintf("容器 %s 的镜像按摘要固定了版本（%s），新服务器连不上镜像仓库，没法按摘要补齐", c.Name, shortRef(c.Image))})
		}
	}
	return res
}

func diskIssues(plan *bundle.Plan, t *Target, d *Decisions, o Options, usedVolumes, usedBinds map[string]bool, addIssue func(Issue)) int64 {
	if t.Disk == nil {
		return 0
	}
	bundleBytes := plan.Sizes.Bundle
	if o.BundleOnDisk {
		bundleBytes = 0
	}
	var keptVolumes int64
	for _, v := range plan.Volumes {
		if d.KeepVolume(v.Name) || !usedVolumes[v.Name] {
			keptVolumes += v.Bytes
		}
	}
	extracted := plan.Sizes.Images + plan.Sizes.Volumes + plan.Sizes.Binds
	dockerNeed := plan.Sizes.Images + plan.Sizes.Volumes - keptVolumes
	base, berr := t.Disk(o.BaseDir)
	root, rerr := t.Disk(t.DockerRoot)
	need := bundleBytes + extracted
	if berr == nil && rerr == nil && base.Dev == root.Dev {
		// 同一块磁盘：下载的包、解开的数据和加载进 Docker 的镜像会同时存在（数据用完就删）。
		need = bundleBytes + extracted + plan.Sizes.Images
		if int64(base.Free) < need {
			addIssue(Issue{Level: Ask, Kind: "disk_base",
				Title: fmt.Sprintf("磁盘空间不够：迁入需要约 %s，%s 所在磁盘只剩 %s", ui.HumanBytes(need), base.Path, ui.HumanBytes(int64(base.Free))),
				Hint:  "清理磁盘后重新检查，或换一个空间更大的磁盘存放下载文件"})
		} else if float64(base.Free) < float64(need)*1.2 {
			addIssue(Issue{Level: Info, Kind: "disk_tight", Title: fmt.Sprintf("磁盘空间偏紧：迁入需要约 %s，剩余 %s", ui.HumanBytes(need), ui.HumanBytes(int64(base.Free)))})
		}
	} else {
		if berr == nil && int64(base.Free) < need {
			addIssue(Issue{Level: Ask, Kind: "disk_base",
				Title: fmt.Sprintf("磁盘空间不够：下载和解开需要约 %s，%s 所在磁盘只剩 %s", ui.HumanBytes(need), base.Path, ui.HumanBytes(int64(base.Free))),
				Hint:  "清理磁盘后重新检查，或换一个空间更大的磁盘存放下载文件"})
		}
		if rerr == nil && int64(root.Free) < dockerNeed {
			addIssue(Issue{Level: Ask, Kind: "disk_docker",
				Title: fmt.Sprintf("磁盘空间不够：Docker 存放镜像和数据卷需要约 %s，%s 所在磁盘只剩 %s", ui.HumanBytes(dockerNeed), root.Path, ui.HumanBytes(int64(root.Free))),
				Hint:  "清理磁盘后重新检查（docker system prune 可以清理不用的镜像和容器）"})
		}
	}
	for _, b := range plan.Binds {
		if !usedBinds[b.Path] || b.Bytes == 0 {
			continue
		}
		disk, err := t.Disk(b.Path)
		if err == nil && int64(disk.Free) < b.Bytes {
			addIssue(Issue{Level: Ask, Kind: "disk_bind",
				Title: fmt.Sprintf("磁盘空间不够：目录 %s 需要约 %s，所在磁盘只剩 %s", b.Path, ui.HumanBytes(b.Bytes), ui.HumanBytes(int64(disk.Free))),
				Hint:  "清理这块磁盘后重新检查"})
		}
	}
	return need
}

func networkIssues(plan *bundle.Plan, t *Target, d *Decisions, containers []bundle.PlanContainer, planNames map[string]bool, addIssue func(Issue)) {
	used := map[string]bool{}
	for _, c := range containers {
		for _, n := range c.Networks {
			used[n.Name] = true
		}
	}
	for _, n := range plan.Networks {
		if !used[n.Name] || n.Compose || n.Name == "bridge" || n.Name == "host" || n.Name == "none" {
			continue
		}
		if n.Driver == "macvlan" || n.Driver == "ipvlan" {
			addIssue(Issue{Level: Info, Kind: "macvlan", Network: n.Name,
				Title: fmt.Sprintf("网络 %s 使用 %s，需要新服务器的网卡名称和旧服务器一致", n.Name, n.Driver)})
		}
		if d.ReuseExisting(n.Name) || d.AutoSubnetFor(n.Name) {
			continue
		}
		if have, ok := t.Networks[n.Name]; ok {
			if !sameSet(have.Subnets, n.Subnets) || driverOf(have.Driver) != driverOf(n.Driver) {
				addIssue(Issue{Level: Ask, Kind: "network_mismatch", Network: n.Name,
					Title: fmt.Sprintf("新服务器已有同名网络 %s，但网段不同（旧服务器 %s，新服务器 %s）", n.Name, joinOr(n.Subnets, "自动"), joinOr(have.Subnets, "自动"))})
			}
			continue
		}
		for _, s := range n.Subnets {
			_, mine, err := net.ParseCIDR(s)
			if err != nil {
				continue
			}
			clash := ""
			for other, have := range t.Networks {
				for _, hs := range have.Subnets {
					if _, theirs, err := net.ParseCIDR(hs); err == nil && (mine.Contains(theirs.IP) || theirs.Contains(mine.IP)) {
						clash = fmt.Sprintf("%s（%s）", other, hs)
					}
				}
			}
			if clash != "" {
				addIssue(Issue{Level: Ask, Kind: "subnet_conflict", Network: n.Name,
					Title: fmt.Sprintf("网络 %s 要用的网段 %s 和新服务器上的网络 %s 重叠", n.Name, s, clash)})
				break
			}
		}
	}
	for _, c := range containers {
		for _, n := range c.Networks {
			if n.IP == "" || d.AutoIPFor(c.Name, n.Name) {
				continue
			}
			holder := t.Networks[n.Name].IPs[n.IP]
			if holder != "" && holder != c.Name && !planNames[holder] {
				addIssue(Issue{Level: Ask, Kind: "ip_conflict", Container: c.Name, Project: c.Project, Network: n.Name,
					Title: fmt.Sprintf("网络 %s 中的 IP %s 已被容器 %s 使用，%s需要它", n.Name, n.IP, holder, who(c))})
			}
		}
	}
}

var wildcard = map[string]bool{"": true, "0.0.0.0": true, "::": true, "*": true, "[::]": true}

func portIssues(t *Target, d *Decisions, containers []bundle.PlanContainer, planNames map[string]bool, o Options, addIssue func(Issue)) {
	busy := map[string]bool{}
	for _, l := range t.Listeners {
		busy[l.Proto+"/"+strconv.Itoa(l.Port)] = true
	}
	for k := range t.DockerPorts {
		busy[k] = true
	}
	requested := map[string]string{} // 协议/端口 → 要用它的容器
	for _, c := range containers {
		if c.NetworkMode == "host" || strings.HasPrefix(c.NetworkMode, "container:") {
			continue
		}
		stopped := c.State != "running" && c.State != "paused" && c.State != "restarting"
		for _, p := range c.Ports {
			ref := PortRef{Container: c.Name, HostIP: p.HostIP, HostPort: p.HostPort, ContainerPort: p.ContainerPort, Proto: p.Proto}
			ip, port := p.HostIP, p.HostPort
			if ch, ok := d.PortFor(ref); ok {
				if ch.Drop {
					continue
				}
				ip, port = ch.NewIP, ch.NewPort
			}
			// 端口绑定的具体 IP 在新服务器上不存在（C12）。
			if !wildcard[ip] && !isLoopback(ip) && t.HostIPs != nil && !t.HostIPs[ip] {
				addIssue(Issue{Level: Ask, Kind: "host_ip", Container: c.Name, Project: c.Project, Port: &ref,
					Title: fmt.Sprintf("%s的端口 %s 绑定在地址 %s 上，新服务器没有这个地址", who(c), port, ip)})
				continue
			}
			key := p.Proto + "/" + port
			if stopped {
				continue // 源端本来就停着，迁入后也不启动，不会占用端口
			}
			if other, ok := requested[key]; ok {
				addIssue(Issue{Level: Ask, Kind: "port", Container: c.Name, Project: c.Project, Port: &ref,
					Title:   fmt.Sprintf("端口 %s 同时被迁移包里的 %s 和%s使用", port, other, who(c)),
					Suggest: suggest(p.Proto, port, busy, requested)})
				continue
			}
			requested[key] = c.Name
			holder, owner := portHolder(t, p.Proto, ip, port)
			if holder == "" || (owner != "" && planNames[owner] && replaceable(owner, d, t, o)) {
				continue
			}
			addIssue(Issue{Level: Ask, Kind: "port", Container: c.Name, Project: c.Project, Port: &ref,
				Title:   fmt.Sprintf("端口 %s 已被%s占用，%s需要使用它", port, ui.CJKSpaced(holder), who(c)),
				Suggest: suggest(p.Proto, port, busy, requested)})
		}
	}
	if !t.ListenersOK && len(containers) > 0 {
		for _, c := range containers {
			if len(c.Ports) > 0 {
				addIssue(Issue{Level: Info, Kind: "port_unchecked", Title: "读不到这台服务器正在监听的端口，只检查了 Docker 容器占用的端口"})
				break
			}
		}
	}
}

// replaceable 表示占用端口的同名容器会在迁入时被替换（替换前先停止），不算冲突。
func replaceable(name string, d *Decisions, t *Target, o Options) bool {
	tc := t.Containers[name]
	if tc.Project != "" {
		return o.Existing != "skip" && !has(d.SkipProjects, tc.Project)
	}
	return o.Existing != "skip" && !has(d.SkipContainers, name)
}

func portHolder(t *Target, proto, ip, port string) (holder, owner string) {
	if c := t.DockerPorts[proto+"/"+port]; c != "" {
		return "容器 " + c, c
	}
	n, _ := strconv.Atoi(port)
	for _, l := range t.Listeners {
		if l.Proto == proto && l.Port == n && (wildcard[ip] || wildcard[l.Addr] || l.Addr == ip) {
			return "其他程序", ""
		}
	}
	return "", ""
}

func suggest(proto, port string, busy map[string]bool, requested map[string]string) string {
	n, err := strconv.Atoi(port)
	if err != nil {
		return ""
	}
	for p := n + 1; p < n+200 && p <= 65535; p++ {
		k := proto + "/" + strconv.Itoa(p)
		if !busy[k] && requested[k] == "" {
			return strconv.Itoa(p)
		}
	}
	return ""
}

func who(c bundle.PlanContainer) string {
	if c.Project != "" {
		return fmt.Sprintf("Compose 项目 %s 的容器 %s ", c.Project, c.Name)
	}
	return "容器 " + c.Name + " "
}

func projectNames(cs []bundle.PlanContainer) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range cs {
		if c.Project != "" && !seen[c.Project] {
			seen[c.Project] = true
			out = append(out, c.Project)
		}
	}
	sort.Strings(out)
	return out
}

func alias(a string) string {
	if v, ok := archAlias[a]; ok {
		return v
	}
	return a
}

// older 表示版本 a 比 b 旧（只比较前两段）。
func older(a, b string) bool {
	pa, pb := versionParts(a), versionParts(b)
	if pa == nil || pb == nil {
		return false
	}
	if pa[0] != pb[0] {
		return pa[0] < pb[0]
	}
	return pa[1] < pb[1]
}

func versionParts(v string) []int {
	var out []int
	cur := -1
	for _, r := range v + "." {
		if r >= '0' && r <= '9' {
			if cur < 0 {
				cur = 0
			}
			cur = cur*10 + int(r-'0')
			continue
		}
		if cur >= 0 {
			out = append(out, cur)
			cur = -1
			if len(out) == 2 {
				return out
			}
		}
	}
	return nil
}

func driverOf(d string) string {
	if d == "" {
		return "local"
	}
	return d
}

func sameMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func sameSet(a, b []string) bool {
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	return strings.Join(x, ",") == strings.Join(y, ",")
}

func joinOr(s []string, empty string) string {
	if len(s) == 0 {
		return empty
	}
	return strings.Join(s, "、")
}

func isLoopback(ip string) bool {
	p := net.ParseIP(strings.Trim(ip, "[]"))
	return p != nil && p.IsLoopback()
}

func shortRef(ref string) string {
	if name, digest, ok := strings.Cut(ref, "@sha256:"); ok && len(digest) > 12 {
		return name + "@sha256:" + digest[:12] + "…"
	}
	return ref
}

// normalize 把路径里的软链接解析掉（不存在的部分原样接在后面），用来判断两个路径
// 是否重叠：经过软链接父目录指过来的路径也要算重叠。
func normalize(p string) string {
	p = filepath.Clean(p)
	rest := ""
	cur := p
	for {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// Normalize 供迁入时再次核对路径。
func Normalize(p string) string { return normalize(p) }
