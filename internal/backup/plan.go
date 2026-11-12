package backup

import (
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/crypto"
	"github.com/lx969788249/dockhop/internal/docker"
)

// buildPlan 生成迁移计划：新服务器下载之前据此检查自己能不能迁入。
func buildPlan(m *bundle.Manifest, col *collected, bundleSize int64, bundleSum string) *bundle.Plan {
	p := &bundle.Plan{Version: bundle.PlanVersion, Source: m.Source, Images: m.ImageFiles, BundleSHA256: bundleSum}
	p.Sizes.Bundle = bundleSize
	p.Sizes.ImageFiles = imageFileBytes(m.ImageFiles)
	p.Sizes.Images = m.Images.Bytes
	for _, pr := range col.projects {
		p.Sizes.Projects += pr.dirBytes + pr.outBytes
	}
	for _, c := range m.Containers {
		if c.Changes != nil {
			p.Sizes.Changes += c.Changes.Bytes
		}
	}
	for _, v := range m.Volumes {
		p.Sizes.Volumes += v.Bytes
	}
	for _, b := range m.Binds {
		p.Sizes.Binds += b.Bytes
	}
	for _, it := range col.sel.items {
		p.Containers = append(p.Containers, planContainer(it, col))
	}
	for _, pr := range m.Projects {
		p.Projects = append(p.Projects, bundle.PlanProject{Name: pr.Name, Services: pr.Services, WorkingDir: pr.WorkingDir, Outside: pr.Outside})
	}
	for _, v := range col.volumes {
		p.Volumes = append(p.Volumes, bundle.PlanVolume{Name: v.name, Driver: v.driver, Opts: v.opts, Labels: v.labels, Bytes: max(v.bytes, 0)})
	}
	for _, b := range col.binds {
		p.Binds = append(p.Binds, bundle.PlanBind{Path: b.path, Link: b.link(), Links: linkPaths(b.links), Bytes: b.bytes})
	}
	for _, n := range col.networks {
		p.Networks = append(p.Networks, bundle.PlanNetwork{Name: n.name, Driver: n.driver, Subnets: n.subnets, Compose: n.compose})
	}
	return p
}

// planContainer 是迁移计划里的一个容器：新服务器要满足的条件（运行时、显卡、设备、端口、网络和固定 IP），
// 和它要写的数据卷、目录。
func planContainer(it *item, col *collected) bundle.PlanContainer {
	c := it.c
	pc := bundle.PlanContainer{
		Name:         it.name(),
		Project:      projectOf(col, it),
		State:        stateName(c.State.Status, c.State.Paused),
		Image:        c.Config.Image,
		DigestPinned: strings.Contains(c.Config.Image, "@sha256:"),
		NetworkMode:  c.HostConfig.NetworkMode,
		Memory:       c.HostConfig.Memory,
		GPU:          wantsGPU(c),
		Ports:        planPorts(c),
		Networks:     planNetworks(c),
		Binds:        bindsOf(c, col.binds),
	}
	if c.HostConfig.Runtime != "" && c.HostConfig.Runtime != "runc" {
		pc.Runtime = c.HostConfig.Runtime
	}
	for _, d := range c.HostConfig.Devices {
		pc.Devices = append(pc.Devices, d.PathOnHost)
	}
	for _, mt := range c.Mounts {
		if mt.Type == "volume" {
			pc.Volumes = append(pc.Volumes, mt.Name)
		}
	}
	return pc
}

// wantsGPU 表示容器要用显卡：--gpus（设备能力里有 gpu），或者用 nvidia 的设备驱动。
func wantsGPU(c *docker.Container) bool {
	for _, r := range c.HostConfig.DeviceRequests {
		if r.Driver == "nvidia" || slices.ContainsFunc(r.Capabilities, func(caps []string) bool { return slices.Contains(caps, "gpu") }) {
			return true
		}
	}
	return false
}

// planPorts 是容器映射到宿主机的端口，按容器端口排好；Docker 随机分配的不会冲突，不记。
func planPorts(c *docker.Container) []bundle.PlanPort {
	keys := make([]string, 0, len(c.HostConfig.PortBindings))
	for k := range c.HostConfig.PortBindings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []bundle.PlanPort
	for _, k := range keys {
		cport, proto, _ := strings.Cut(k, "/")
		if proto == "" {
			proto = "tcp"
		}
		for _, b := range c.HostConfig.PortBindings[k] {
			if b.HostPort != "" {
				out = append(out, bundle.PlanPort{HostIP: b.HostIP, HostPort: b.HostPort, ContainerPort: cport, Proto: proto})
			}
		}
	}
	return out
}

// planNetworks 是容器接的网络，按名字排好；只记用户指定的固定 IP。
func planNetworks(c *docker.Container) []bundle.PlanNetAddr {
	names := make([]string, 0, len(c.NetworkSettings.Networks))
	for n := range c.NetworkSettings.Networks {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []bundle.PlanNetAddr
	for _, n := range names {
		addr := bundle.PlanNetAddr{Name: n}
		if cfg := c.NetworkSettings.Networks[n].IPAMConfig; cfg != nil {
			addr.IP = cfg.IPv4Address
		}
		out = append(out, addr)
	}
	return out
}

// bindsOf 是容器 c 挂载的目录在迁移包里是哪几个（binds 里的真实路径）：挂载的路径就是它、在它里面，
// 或者是指向它的软链接（或者在软链接里面）。迁入前检查和迁入都按迁移计划里记的这一份算要写哪些目录。
func bindsOf(c *docker.Container, binds []*bindInfo) []string {
	var out []string
	for _, b := range binds {
		for _, mt := range c.Mounts {
			if mt.Type == "bind" && slices.ContainsFunc(append([]string{b.path}, linkPaths(b.links)...), func(p string) bool {
				return mt.Source == p || strings.HasPrefix(mt.Source, p+"/")
			}) {
				out = append(out, b.path)
				break
			}
		}
	}
	return out
}

func projectOf(col *collected, it *item) string {
	for _, u := range col.sel.units {
		if u.project == "" {
			continue
		}
		for _, x := range u.items {
			if x == it {
				return u.project
			}
		}
	}
	return ""
}

func stateName(status string, paused bool) string {
	if paused {
		return "paused"
	}
	return status
}

func linkPaths(links []bundle.BindLink) []string {
	var out []string
	for _, l := range links {
		out = append(out, l.Link)
	}
	return out
}

// writePlan 把加密的迁移计划写到 path。
func writePlan(path string, link crypto.Link, p *bundle.Plan) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	err = bundle.EncryptPlan(f, link, p)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
