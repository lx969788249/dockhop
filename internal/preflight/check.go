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
	"maps"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/host"
	"github.com/lx969788249/dockhop/internal/spec"
	"github.com/lx969788249/dockhop/internal/ui"
)

// Level 是问题的严重程度。
type Level int

const (
	Note Level = iota // 只是说一声，不影响迁移，也不用处理
	Info              // 要让用户留意，但不用选择
	Ask               // 要用户选择怎么处理
	Stop              // 没法继续
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

func add(list []string, v string) []string {
	if slices.Contains(list, v) {
		return list
	}
	return append(list, v)
}

// Skipped 表示容器（或它所在的项目）是否按选择跳过。
func (d *Decisions) SkippedContainer(c bundle.PlanContainer) bool {
	return slices.Contains(d.SkipContainers, c.Name) || (c.Project != "" && slices.Contains(d.SkipProjects, c.Project))
}

// AutoIPFor 表示容器在这个网络里是否改为自动分配 IP（单独选择、网段改为自动、沿用已有网络时都是）。
func (d *Decisions) AutoIPFor(container, network string) bool {
	for _, r := range d.AutoIP {
		if r.Container == container && r.Network == network {
			return true
		}
	}
	return slices.Contains(d.AutoSubnet, network) || slices.Contains(d.ReuseNetwork, network)
}

// currentPort 是端口映射现在的样子：调整过就是调整后的，否则就是原来的。处理一个问题时
// 只改这次选的那一项，不能把之前对它做过的其他调整（例如改绑地址）冲掉。
func (d *Decisions) currentPort(r PortRef) PortChange {
	if ch, ok := d.PortFor(r); ok {
		return ch
	}
	return PortChange{PortRef: r, NewIP: r.HostIP, NewPort: r.HostPort}
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
func (d *Decisions) UseBundleImage(ref string) bool { return slices.Contains(d.BundleImages, ref) }

// KeepVolume 表示是否保留新服务器上已有的这个卷、不写入迁移过来的数据。
func (d *Decisions) KeepVolume(name string) bool { return slices.Contains(d.KeepVolumes, name) }

// ReuseExisting 表示是否沿用新服务器上已有的同名网络。
func (d *Decisions) ReuseExisting(network string) bool {
	return slices.Contains(d.ReuseNetwork, network)
}

// AutoSubnetFor 表示网络是否改为让 Docker 自动选择网段。
func (d *Decisions) AutoSubnetFor(network string) bool { return slices.Contains(d.AutoSubnet, network) }

// Counterparts 说明新服务器上的已有容器在迁入时会怎样、迁移包里的哪些部分因此不迁入，迁入前检查和
// 迁入（restore）都按它判断。迁移包按一起迁入的单位分成几部分：每个 Compose 项目是一部分，每个单独
// 迁入的容器是一部分。每个已有容器只跟着其中一部分走：
//   - 它属于迁移包里也有的 Compose 项目时，跟着这个项目，即使迁移包里还有和它同名的容器：项目替换时
//     它随项目一起替换，名字空出来给同名的容器；
//   - 否则跟着迁移包里和它同名的容器所在的部分：单独迁入的容器，或者这个容器所在的 Compose 项目
//     （Compose 按原来的名字创建容器，例如写了 container_name 的），不管它是独立容器还是属于别的
//     Compose 项目。
//
// 跟着的部分迁入，就替换它（迁入前停下、备份，失败时放回）；跟着的部分不迁入（检查时选择跳过，或者
// --existing skip），就保留它。迁移包里的容器要用的名字被保留下来的已有容器占着时（它跟着别的部分），
// 这个容器所在的部分也不迁入。
type Counterparts struct {
	part    map[string]string // 迁移包里的容器 → 所在的部分（见 unitName）
	follows map[string]string // 新服务器上的已有容器 → 跟着的部分；没有对应的部分时不在里面
	project map[string]string // 新服务器上的已有容器 → 所属的 Compose 项目，独立容器为空
	there   map[string]bool   // 新服务器上有已有容器跟着的部分
	out     map[string]bool   // 不迁入的部分
}

// CounterpartsOf 按迁移包里的容器、新服务器上的已有容器（名字 → 所属的 Compose 项目）和用户的
// 选择（policy 是 --existing）建立 Counterparts。
func CounterpartsOf(plan []bundle.PlanContainer, existing map[string]string, d *Decisions, policy string) Counterparts {
	c := Counterparts{part: map[string]string{}, follows: map[string]string{}, project: existing,
		there: map[string]bool{}, out: map[string]bool{}}
	projects := map[string]bool{} // 迁移包里的 Compose 项目
	for _, pc := range plan {
		u := unitName(pc)
		c.part[pc.Name] = u
		if pc.Project != "" {
			projects[pc.Project] = true
		}
		if pc.Project != "" && slices.Contains(d.SkipProjects, pc.Project) || pc.Project == "" && slices.Contains(d.SkipContainers, pc.Name) {
			c.out[u] = true
		}
	}
	for name, p := range existing {
		u := c.part[name]
		if projects[p] {
			u = projectUnit(p)
		}
		if u != "" {
			c.follows[name], c.there[u] = u, true
		}
	}
	if policy == "skip" {
		for u := range c.there {
			c.out[u] = true
		}
	}
	// 名字被保留下来的已有容器占着的部分不迁入；它在新服务器上对应的已有容器也就保留下来，可能又占着
	// 别的部分要用的名字，所以一直算到没有变化。
	for changed := true; changed; {
		changed = false
		for name, u := range c.part {
			if h := c.Holder(name); h != "" && c.out[h] && !c.out[u] {
				c.out[u], changed = true, true
			}
		}
	}
	return c
}

// Of 返回已有容器 name 跟着迁移包里的哪一部分，例如“Compose 项目 blog”“容器 web”，没有时为空；
// 以及那部分是不是不迁入。
func (c Counterparts) Of(name string) (unit string, skipped bool) {
	unit = c.follows[name]
	return unit, unit != "" && c.out[unit]
}

// Replaced 表示迁入时会替换已有容器 name。
func (c Counterparts) Replaced(name string) bool {
	unit, skipped := c.Of(name)
	return unit != "" && !skipped
}

// Kept 表示已有容器 name 保留下来：它跟着的部分不迁入。
func (c Counterparts) Kept(name string) bool {
	unit, skipped := c.Of(name)
	return unit != "" && skipped
}

// Holder 返回占着迁移包里的容器 name 要用的名字、跟着迁移包里另一部分的已有容器跟着的那部分（只会是
// 它所属的 Compose 项目），没有时为空。那部分迁入时它随着替换，名字空出来（两者分在同一组）；不迁入时
// 名字被占着，name 所在的部分也不迁入。
func (c Counterparts) Holder(name string) string {
	if u, ok := c.part[name]; ok && c.follows[name] != u {
		return c.follows[name]
	}
	return ""
}

// ExistingFor 说明新服务器上跟着迁移包里的 Compose 项目 project 的已有容器，例如“同名的 Compose 项目
// blog”“容器 nginx，和 Compose 项目 blog 里的容器同名”；没有时为空。
func (c Counterparts) ExistingFor(project string) string {
	same := false
	var named []string // 不属于这个项目、和它里面的容器同名的
	for name, u := range c.follows {
		switch p := c.project[name]; {
		case u != projectUnit(project):
		case p == project:
			same = true
		case p != "":
			named = append(named, name+"（属于 Compose 项目 "+p+"）")
		default:
			named = append(named, name)
		}
	}
	slices.Sort(named)
	list := strings.Join(named, "、")
	switch {
	case same && len(named) > 0:
		return "同名的 Compose 项目 " + project + "，还有容器 " + list + " 和它里面的容器同名"
	case same:
		return "同名的 Compose 项目 " + project
	case len(named) > 0:
		return "容器 " + list + "，和 Compose 项目 " + project + " 里的容器同名"
	}
	return ""
}

// migrates 表示迁移包里的容器 pc 所在的部分迁入。
func (c Counterparts) migrates(pc bundle.PlanContainer) bool { return !c.out[c.part[pc.Name]] }

// keeps 表示迁移包里的容器 pc 是新服务器上保留下来的服务在旧服务器上的样子：它所在的部分在新服务器
// 上有对应的已有容器、不迁入，或者和它同名的已有容器保留下来了（名字被占着）。
func (c Counterparts) keeps(pc bundle.PlanContainer) bool {
	u := c.part[pc.Name]
	return c.there[u] && c.out[u] || c.Kept(pc.Name)
}

// Options 是检查的选项。
type Options struct {
	Existing string // replace、skip、fail；空表示有同名服务时询问
	BaseDir  string // 存放下载和解压文件的目录
	Home     string // DockHop 自己的目录，挂载目录不能和它重叠
	// LocalWork 由迁入给出：要写的这些东西（w）里，哪些事只能在 Docker 所在的机器上做。
	// Docker 不在这台服务器上、又有这样的事时，在下载之前停下。
	LocalWork func(w Writes, t *Target) []string
}

// Writes 是按眼下的选择，这次迁入要写到新服务器上的东西：跳过的容器独占的、保留已有的不算。
type Writes struct {
	Binds    []string // 宿主机上的目录和文件（真实路径）
	Projects []string // Compose 项目
	Volumes  []string // 数据卷，包括匿名卷
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

	// 要迁入的容器：去掉选择跳过的，以及因为新服务器上的已有容器不迁入的（见 Counterparts）。
	existing := map[string]string{}
	for name, tc := range t.Containers {
		existing[name] = tc.Project
	}
	match := CounterpartsOf(plan.Containers, existing, d, o.Existing)
	var containers []bundle.PlanContainer
	for _, c := range plan.Containers {
		if !d.SkippedContainer(c) && match.migrates(c) {
			containers = append(containers, c)
		}
	}
	res.Containers = len(containers)
	usedVolumes := map[string]bool{}
	usedBinds := map[string]bool{}
	hasProject := false
	for _, c := range containers {
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
		addIssue(Issue{Level: Note, Kind: "docker_version",
			Title: fmt.Sprintf("新服务器的 Docker（%s）比旧服务器（%s）旧，一般不影响迁移", t.DockerVersion, plan.Source.Docker)})
	}
	if hasProject && !t.HasCompose {
		addIssue(Issue{Level: Stop, Kind: "compose", Title: "要迁移 Compose 项目，但新服务器没有安装 Docker Compose",
			Hint: "先安装 Docker Compose（例如 apt install docker-compose-plugin）再重试"})
	}
	if hasProject && t.HasCompose && strings.HasPrefix(t.Compose, "1.") && !strings.HasPrefix(plan.Source.Compose, "1.") && plan.Source.Compose != "" {
		addIssue(Issue{Level: Info, Kind: "old_compose",
			Title: fmt.Sprintf("新服务器的 docker-compose 是老版本 %s，旧服务器用的是 Compose %s；有些写法老版本读不懂，迁入时可能失败（失败会自动删除迁入的内容）。"+
				"建议先在新服务器安装新版 Docker Compose", t.Compose, plan.Source.Compose)})
	}
	if !t.Local && o.LocalWork != nil {
		var w Writes
		for _, b := range plan.Binds {
			if usedBinds[b.Path] {
				w.Binds = append(w.Binds, b.Path)
			}
		}
		for _, c := range containers {
			if c.Project != "" && !slices.Contains(w.Projects, c.Project) {
				w.Projects = append(w.Projects, c.Project)
			}
		}
		for _, v := range plan.Volumes {
			if usedVolumes[v.Name] && !d.KeepVolume(v.Name) {
				w.Volumes = append(w.Volumes, v.Name)
			}
		}
		if what := o.LocalWork(w, t); len(what) > 0 {
			addIssue(Issue{Level: Stop, Kind: "remote_docker",
				Title: "Docker 不在这台服务器上（DOCKER_HOST 指向了别的机器），没法在 Docker 所在的服务器上" + strings.Join(what, "、"),
				Hint:  "请在 Docker 所在的服务器上直接运行 DockHop"})
		}
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
	for _, b := range plan.Binds {
		for _, p := range append([]string{b.Path}, b.AllLinks()...) {
			if is := HomeClash(p, o.Home); is != nil {
				addIssue(*is)
			}
		}
	}

	// ---------- 磁盘空间 ----------
	res.NeedBytes = diskIssues(plan, t, d, o, usedVolumes, usedBinds, addIssue)

	// ---------- 已有的同名容器、Compose 项目、数据卷 ----------
	// 按 --existing skip 保留、不迁入的也要说一声，所以看的是选择跳过的之外的全部。
	held := map[string]bool{} // 里面的容器要用的名字被保留下来的已有容器占着、不迁入的 Compose 项目
	for _, c := range plan.Containers {
		if d.SkippedContainer(c) {
			continue
		}
		if h := match.Holder(c.Name); h != "" {
			// 同名的已有容器跟着另一个 Compose 项目 h 走（见 Counterparts），不单独问：h 替换时名字空出来，
			// 保留时名字被占着，c 所在的部分也不迁入。
			if match.Kept(c.Name) {
				what := "这个容器"
				if c.Project != "" {
					what, held[c.Project] = " Compose 项目 "+c.Project, true
				}
				addIssue(Issue{Level: Info, Kind: "existing_container", Container: c.Name, Project: c.Project,
					Title: fmt.Sprintf("新服务器上已有同名容器 %s，它属于保留的 %s，不迁入%s", c.Name, h, what)})
			}
			continue
		}
		if c.Project != "" || slices.Contains(d.ReplaceContainers, c.Name) {
			continue
		}
		tc, ok := t.Containers[c.Name]
		if !ok {
			continue
		}
		switch o.Existing {
		case "fail":
			addIssue(Issue{Level: Stop, Kind: "existing_container", Container: c.Name,
				Title: "新服务器上已经有同名容器 " + c.Name + "（--existing fail 不允许替换）",
				Hint:  "删除这个容器，或者去掉 --existing fail"})
		case "replace":
			addIssue(Issue{Level: Info, Kind: "existing_container", Container: c.Name,
				Title: "新服务器上已有同名容器 " + c.Name + "，会被替换（迁入失败时放回原来的）"})
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
	for _, p := range projectNames(plan.Containers) {
		what := match.ExistingFor(p)
		if what == "" || held[p] || slices.Contains(d.SkipProjects, p) || slices.Contains(d.ReplaceProjects, p) {
			continue
		}
		switch o.Existing {
		case "fail":
			addIssue(Issue{Level: Stop, Kind: "existing_project", Project: p,
				Title: "新服务器上已经有" + what + "（--existing fail 不允许替换）",
				Hint:  "删除或改名新服务器上的这些容器，或者去掉 --existing fail"})
		case "replace":
			addIssue(Issue{Level: Info, Kind: "existing_project", Project: p,
				Title: "新服务器上已有" + what + "，会被替换（迁入失败时放回原来的）"})
		case "skip":
			addIssue(Issue{Level: Info, Kind: "existing_project", Project: p,
				Title: "新服务器上已有" + what + "，保留它、不迁入这个项目"})
		default:
			addIssue(Issue{Level: Ask, Kind: "existing_project", Project: p,
				Title: "新服务器上已经有" + what})
		}
	}
	keptDataIssues(plan, t, d, containers, match, addIssue)
	for _, v := range plan.Volumes {
		if !usedVolumes[v.Name] || d.KeepVolume(v.Name) || spec.IsAnonymousVolume(v.Name) {
			continue
		}
		cur, ok := t.Volumes[v.Name]
		if !ok {
			continue
		}
		if driverOf(cur.Driver) != driverOf(v.Driver) || !maps.Equal(cur.Options, v.Opts) {
			addIssue(Issue{Level: Ask, Kind: "volume_mismatch", Volume: v.Name,
				Title: "新服务器已有同名数据卷 " + v.Name + "，但它的存储方式和旧服务器不同"})
		} else {
			addIssue(Issue{Level: Info, Kind: "volume_replace", Volume: v.Name,
				Title: "数据卷 " + v.Name + " 已存在，迁移过来的数据会替换它（失败时放回原来的）"})
		}
	}
	// 目录和数据卷一样整个替换，迁入成功后原来的内容就删了，所以里面有东西时也要说一声。
	// 里面还挂着别的磁盘时不能替换：挂在里面的会跟着挪进备份，提交后删备份会连那块盘上的东西一起删掉。
	for _, b := range plan.Binds {
		if !usedBinds[b.Path] {
			continue
		}
		if dest := t.Link(b.Path); dest != "" {
			addIssue(Issue{Level: Stop, Kind: "bind_link", Title: LinkedBind(b.Path, dest),
				Hint: "用不着这个软链接就删掉它；想让数据放在它指向的地方，删掉软链接后把那个目录挂载（mount --bind）到 " + b.Path + "。处理好之后重新运行迁入"})
			continue
		}
		if t.HasContent(b.Path) {
			addIssue(Issue{Level: Info, Kind: "bind_replace",
				Title: "目录 " + b.Path + " 已存在，迁移过来的内容会替换它（失败时放回原来的）"})
		}
		switch inner, err := t.MountsUnder(b.Path); {
		case err != nil:
			addIssue(Issue{Level: Stop, Kind: "nested_mount",
				Title: "没法确认目录 " + b.Path + " 里面有没有挂着别的磁盘：" + err.Error(),
				Hint:  "确认 /proc 可以读取后重新检查"})
		case len(inner) > 0:
			addIssue(Issue{Level: Stop, Kind: "nested_mount",
				Title: fmt.Sprintf("目录 %s 里面还挂着 %s：迁入要整个替换这个目录，会连带动到挂在里面的东西", b.Path, strings.Join(inner, "、")),
				Hint:  "先把它们卸载（umount），或者挂到这个目录外面，再重新检查"})
		}
	}

	networkIssues(plan, t, d, containers, match, addIssue)
	portIssues(t, d, containers, match, addIssue)

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

// keptDataIssues 找出要迁入的容器（containers）里和保留下来的已有服务共用数据的。保留的服务（检查时
// 选择了保留，或者 --existing skip）不迁入，但共用数据的其他容器迁入时会写入它的卷和目录，等于没保住
// 它的数据。保留的服务用到的数据按新服务器上它实际挂载的、加上旧服务器上同名服务挂载的算。
func keptDataIssues(plan *bundle.Plan, t *Target, d *Decisions, containers []bundle.PlanContainer, match Counterparts, addIssue func(Issue)) {
	type owner struct{ path, who string }
	keptVolumes := map[string]string{} // 卷名 → 谁在用
	var keptBinds []owner
	use := func(who string, vols, binds []string) {
		for _, v := range vols {
			if !spec.IsAnonymousVolume(v) {
				keptVolumes[v] = who
			}
		}
		for _, b := range binds {
			if !spec.IsSystemPath(b) {
				keptBinds = append(keptBinds, owner{host.RealPath(b), who})
			}
		}
	}
	for _, c := range plan.Containers {
		if match.keeps(c) {
			use(unitName(c), c.Volumes, c.Binds)
		}
	}
	for name, tc := range t.Containers {
		if unit, skipped := match.Of(name); unit != "" && skipped {
			use(unit, tc.Volumes, tc.Binds)
		}
	}
	// shared 返回容器 c 要写入的数据里，保留的服务也在用的第一份。
	shared := func(c bundle.PlanContainer) (who, what string) {
		for _, v := range c.Volumes {
			if w, ok := keptVolumes[v]; ok && !d.KeepVolume(v) {
				return w, "数据卷 " + v
			}
		}
		for _, b := range c.Binds {
			for _, k := range keptBinds {
				if !spec.IsSystemPath(b) && spec.PathsOverlap(host.RealPath(b), k.path) {
					return k.who, "目录 " + b
				}
			}
		}
		return "", ""
	}
	reported := map[string]bool{}
	for _, c := range containers {
		if reported[unitName(c)] {
			continue
		}
		who, what := shared(c)
		if who == "" {
			continue
		}
		reported[unitName(c)] = true
		addIssue(Issue{Level: Ask, Kind: "skip_with_data", Container: c.Name, Project: c.Project,
			Title: fmt.Sprintf("%s 和保留的%s 共用%s，迁入会覆盖%s 正在用的数据", unitName(c), who, what, who),
			Hint:  "把共用数据的服务一起跳过，或者替换保留的服务"})
	}
}

// unitName 是迁移时的单位：Compose 项目里的容器跟着项目走。
func unitName(c bundle.PlanContainer) string {
	if c.Project != "" {
		return projectUnit(c.Project)
	}
	return "容器 " + c.Name
}

func projectUnit(project string) string { return "Compose 项目 " + project }

func diskIssues(plan *bundle.Plan, t *Target, d *Decisions, o Options, usedVolumes, usedBinds map[string]bool, addIssue func(Issue)) int64 {
	if t.Disk == nil {
		return 0
	}
	bundleBytes := plan.Sizes.Bundle + plan.Sizes.ImageFiles // 按镜像全部从旧服务器下载算
	var keptVolumes int64
	for _, v := range plan.Volumes {
		if d.KeepVolume(v.Name) || !usedVolumes[v.Name] {
			keptVolumes += v.Bytes
		}
	}
	// 解开后的数据：镜像（格式版本 1 在包里）、数据卷、目录、Compose 项目的文件、容器里的改动。
	// 写进 Docker 的：镜像、数据卷、容器里的改动（写进新容器）。
	extracted := plan.Sizes.Images + plan.Sizes.Volumes + plan.Sizes.Binds + plan.Sizes.Projects + plan.Sizes.Changes
	dockerNeed := plan.Sizes.Images + plan.Sizes.Volumes + plan.Sizes.Changes - keptVolumes
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

func networkIssues(plan *bundle.Plan, t *Target, d *Decisions, containers []bundle.PlanContainer, match Counterparts, addIssue func(Issue)) {
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
		// 处理这两种问题要让网络里的容器改为自动分配 IP。Compose 项目的固定 IP 写在用户自己的
		// 配置文件里，迁入时不会去改，所以对它们不提供这个做法，只能处理后重查或者跳过项目。
		netIssue := func(kind, problem string) {
			for _, c := range containers {
				for _, cn := range c.Networks {
					if cn.Name == n.Name && cn.IP != "" && c.Project != "" {
						addIssue(Issue{Level: Ask, Kind: kind, Network: n.Name, Container: c.Name, Project: c.Project,
							Title: fmt.Sprintf("%s；Compose 项目 %s 的容器 %s 在这个网络里用固定 IP %s", problem, c.Project, c.Name, cn.IP),
							Hint:  "在新服务器上处理好这个网络，或者修改 " + c.Project + " 配置文件里的固定 IP 后重新运行"})
						return
					}
				}
			}
			addIssue(Issue{Level: Ask, Kind: kind, Network: n.Name, Title: problem})
		}
		if have, ok := t.Networks[n.Name]; ok {
			if !sameSet(have.Subnets, n.Subnets) || driverOf(have.Driver) != driverOf(n.Driver) {
				netIssue("network_mismatch", fmt.Sprintf("新服务器已有同名网络 %s，但网段不同（旧服务器 %s，新服务器 %s）", n.Name, joinOr(n.Subnets, "自动"), joinOr(have.Subnets, "自动")))
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
				netIssue("subnet_conflict", fmt.Sprintf("网络 %s 要用的网段 %s 和新服务器上的网络 %s 重叠", n.Name, s, clash))
				break
			}
		}
	}
	for _, c := range containers {
		for _, n := range c.Networks {
			if n.IP == "" || d.AutoIPFor(c.Name, n.Name) {
				continue
			}
			// 迁入前会替换的已有容器（见 Counterparts）让出 IP。
			holder := t.Networks[n.Name].IPs[n.IP]
			if holder != "" && !match.Replaced(holder) {
				addIssue(Issue{Level: Ask, Kind: "ip_conflict", Container: c.Name, Project: c.Project, Network: n.Name,
					Title: fmt.Sprintf("网络 %s 中的 IP %s 已被容器 %s 使用，%s需要它", n.Name, n.IP, holder, who(c))})
			}
		}
	}
}

var wildcard = map[string]bool{"": true, "0.0.0.0": true, "::": true, "*": true, "[::]": true}

// sameAddress 表示绑定在这两个地址上的同一个端口会互相冲突：其中一个是“所有地址”，
// 或者是同一个地址。绑定在不同的具体地址上可以用同一个端口。
func sameAddress(a, b string) bool { return wildcard[a] || wildcard[b] || a == b }

// binding 是迁移包里的一个容器要把端口绑定在哪个地址上。
type binding struct{ ip, container string }

func portIssues(t *Target, d *Decisions, containers []bundle.PlanContainer, match Counterparts, addIssue func(Issue)) {
	// 推荐的空闲端口要避开：新服务器上已被占用的、迁移包里的容器要用的，以及这一轮已经推荐
	// 给别的容器的（suggest 选中时记进来），否则一次接受几个推荐（--yes）时会互相冲突。
	taken := map[string]bool{}
	for _, l := range t.Listeners {
		taken[l.Proto+"/"+strconv.Itoa(l.Port)] = true
	}
	for _, p := range t.DockerPorts {
		taken[p.Proto+"/"+p.Port] = true
	}
	for _, c := range containers {
		for _, p := range c.Ports {
			if ch := d.currentPort(portRef(c, p)); !ch.Drop {
				taken[p.Proto+"/"+ch.NewPort] = true
			}
		}
	}
	requested := map[string][]binding{} // 协议/端口 → 迁移包里要用它的容器
	for _, c := range containers {
		if c.NetworkMode == "host" || strings.HasPrefix(c.NetworkMode, "container:") {
			continue
		}
		stopped := c.State != "running" && c.State != "paused" && c.State != "restarting"
		for _, p := range c.Ports {
			ref := portRef(c, p)
			ch := d.currentPort(ref)
			if ch.Drop {
				continue
			}
			ip, port := ch.NewIP, ch.NewPort
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
			if other := conflicting(requested[key], ip); other != "" {
				addIssue(Issue{Level: Ask, Kind: "port", Container: c.Name, Project: c.Project, Port: &ref,
					Title:   fmt.Sprintf("端口 %s 同时被迁移包里的 %s 和%s使用", port, other, who(c)),
					Suggest: suggest(p.Proto, port, taken)})
				continue
			}
			requested[key] = append(requested[key], binding{ip: ip, container: c.Name})
			// 迁入前会替换的已有容器（见 Counterparts）让出端口。
			if holder := portHolder(t, p.Proto, ip, port, match.Replaced); holder != "" {
				addIssue(Issue{Level: Ask, Kind: "port", Container: c.Name, Project: c.Project, Port: &ref,
					Title:   fmt.Sprintf("端口 %s 已被%s占用，%s需要使用它", port, ui.CJKSpaced(holder), who(c)),
					Suggest: suggest(p.Proto, port, taken)})
			}
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

func portRef(c bundle.PlanContainer, p bundle.PlanPort) PortRef {
	return PortRef{Container: c.Name, HostIP: p.HostIP, HostPort: p.HostPort, ContainerPort: p.ContainerPort, Proto: p.Proto}
}

// conflicting 返回迁移包里和这个地址抢同一个端口的容器；没有时为空。
func conflicting(list []binding, ip string) string {
	for _, b := range list {
		if sameAddress(b.ip, ip) {
			return b.container
		}
	}
	return ""
}

// portHolder 返回在这个地址上占着端口、迁入时不会让开的程序；没有时为空。同一个端口可能
// 被几个容器绑定在不同的地址上，要逐个看：releases 的容器迁入前会停下，不算占用。
func portHolder(t *Target, proto, ip, port string, releases func(container string) bool) string {
	for _, p := range t.DockerPorts {
		if p.Proto == proto && p.Port == port && sameAddress(p.IP, ip) && !releases(p.Container) {
			return "容器 " + p.Container
		}
	}
	n, _ := strconv.Atoi(port)
	for _, l := range t.Listeners {
		if l.Proto == proto && l.Port == n && sameAddress(l.Addr, ip) && !dockerListener(t, l) {
			return "其他程序"
		}
	}
	return ""
}

// dockerListener 表示这个监听是 Docker 为容器开放的端口（docker-proxy），上面已经按容器查过。
func dockerListener(t *Target, l host.Listener) bool {
	for _, p := range t.DockerPorts {
		if p.Proto == l.Proto && p.Port == strconv.Itoa(l.Port) && sameAddress(p.IP, l.Addr) {
			return true
		}
	}
	return false
}

// suggest 找一个空闲端口：在任何地址上都没人用、也没推荐给别人的才算，免得换过去又冲突。
// 选中的记进 taken。
func suggest(proto, port string, taken map[string]bool) string {
	n, err := strconv.Atoi(port)
	if err != nil {
		return ""
	}
	for p := n + 1; p < n+200 && p <= 65535; p++ {
		if k := proto + "/" + strconv.Itoa(p); !taken[k] {
			taken[k] = true
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

// LinkedBind 说明为什么要迁入的目录在新服务器上是软链接时不迁入：跟着它写，会整个替换掉它指向的地方
// （可能是系统目录或者一整块盘）；把它换成普通目录，数据又写到了软链接所在的盘上。迁入前检查和真正
// 写入之前都按它停下。
func LinkedBind(path, dest string) string {
	return fmt.Sprintf("新服务器上的 %s 是软链接，指向 %s：迁入不会跟着它去替换它指向的地方，也不会把它换成普通目录", path, dest)
}

// HomeClash 检查迁入的目录 p 会不会盖住 DockHop 存放迁移文件、回滚资料的目录 home。
// 迁入前检查按迁移计划查一遍，解开迁移包后按实际要写的目录再查一遍，用的是这同一条规则。
func HomeClash(p, home string) *Issue {
	if p == "" || home == "" || spec.IsSystemPath(p) || !spec.PathsOverlap(host.RealPath(p), host.RealPath(home)) {
		return nil
	}
	return &Issue{Level: Stop, Kind: "bind_home",
		Title: fmt.Sprintf("挂载目录 %s 和 DockHop 存放迁移文件、回滚资料的目录 %s 重叠，迁入会把它们覆盖", p, home),
		Hint:  "设置 DOCKHOP_HOME=其他目录（例如 /var/tmp/dockhop）后重新运行"}
}
