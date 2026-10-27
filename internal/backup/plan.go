package backup

import (
	"sort"
	"strings"

	"github.com/lx969788249/dockhop/internal/bundle"
)

// buildPlan 生成迁移计划：新服务器下载之前据此检查自己能不能迁入。
func buildPlan(m *bundle.Manifest, col *collected, bundleSize int64) *bundle.Plan {
	p := &bundle.Plan{Version: 1, Source: m.Source}
	p.Sizes.Bundle = bundleSize
	p.Sizes.Images = m.Images.Bytes
	for _, v := range m.Volumes {
		p.Sizes.Volumes += v.Bytes
	}
	for _, b := range m.Binds {
		p.Sizes.Binds += b.Bytes
	}
	bindOf := map[string]string{} // 挂载用的路径 → 迁移的真实路径
	for _, b := range col.binds {
		bindOf[b.path] = b.path
		for _, l := range b.links {
			bindOf[l.Link] = b.path
		}
	}
	for _, it := range col.sel.items {
		c := it.c
		pc := bundle.PlanContainer{
			Name:         it.name(),
			Project:      projectOf(col, it),
			State:        stateName(c.State.Status, c.State.Paused),
			Image:        c.Config.Image,
			DigestPinned: strings.Contains(c.Config.Image, "@sha256:"),
			NetworkMode:  c.HostConfig.NetworkMode,
			Memory:       c.HostConfig.Memory,
		}
		if c.HostConfig.Runtime != "" && c.HostConfig.Runtime != "runc" {
			pc.Runtime = c.HostConfig.Runtime
		}
		for _, r := range c.HostConfig.DeviceRequests {
			for _, caps := range r.Capabilities {
				for _, cp := range caps {
					if cp == "gpu" {
						pc.GPU = true
					}
				}
			}
			if r.Driver == "nvidia" {
				pc.GPU = true
			}
		}
		for _, d := range c.HostConfig.Devices {
			pc.Devices = append(pc.Devices, d.PathOnHost)
		}
		ports := make([]string, 0, len(c.HostConfig.PortBindings))
		for k := range c.HostConfig.PortBindings {
			ports = append(ports, k)
		}
		sort.Strings(ports)
		for _, k := range ports {
			cport, proto, _ := strings.Cut(k, "/")
			if proto == "" {
				proto = "tcp"
			}
			for _, b := range c.HostConfig.PortBindings[k] {
				if b.HostPort == "" {
					continue // Docker 随机分配的端口，不会冲突
				}
				pc.Ports = append(pc.Ports, bundle.PlanPort{HostIP: b.HostIP, HostPort: b.HostPort, ContainerPort: cport, Proto: proto})
			}
		}
		nets := make([]string, 0, len(c.NetworkSettings.Networks))
		for n := range c.NetworkSettings.Networks {
			nets = append(nets, n)
		}
		sort.Strings(nets)
		for _, n := range nets {
			addr := bundle.PlanNetAddr{Name: n}
			if cfg := c.NetworkSettings.Networks[n].IPAMConfig; cfg != nil {
				addr.IP = cfg.IPv4Address // 只有用户指定的固定 IP 才记录
			}
			pc.Networks = append(pc.Networks, addr)
		}
		for _, mt := range c.Mounts {
			switch mt.Type {
			case "volume":
				pc.Volumes = append(pc.Volumes, mt.Name)
			case "bind":
				if real, ok := bindOf[mt.Source]; ok {
					pc.Binds = append(pc.Binds, real)
				}
			}
		}
		p.Containers = append(p.Containers, pc)
	}
	for _, pr := range m.Projects {
		p.Projects = append(p.Projects, bundle.PlanProject{Name: pr.Name, Services: pr.Services})
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
