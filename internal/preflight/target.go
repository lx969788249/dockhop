package preflight

import (
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/host"
)

// Target 是新服务器的情况。
type Target struct {
	Arch          string
	DockerVersion string
	DockerRoot    string
	Runtimes      map[string]bool
	VolumePlugins map[string]bool
	MemTotal      int64
	HasCompose    bool
	Compose       string // 新服务器上 Compose 的版本
	Local         bool
	Containers    map[string]TargetContainer
	Projects      map[string]bool
	Volumes       map[string]*docker.Volume
	Networks      map[string]TargetNetwork
	DockerPorts   map[string]string // tcp/8080 → 占用它的容器
	Listeners     []host.Listener
	ListenersOK   bool
	HostIPs       map[string]bool // 本机的地址；为 nil 时不检查
	Exists        func(path string) bool
	Disk          func(path string) (host.Disk, error)
	Pullable      func(ref string) bool
}

// TargetContainer 是新服务器上已有的一个容器。
type TargetContainer struct {
	Project string
	State   string
}

// TargetNetwork 是新服务器上已有的一个网络。
type TargetNetwork struct {
	Driver  string
	Subnets []string
	IPs     map[string]string // IP → 容器名
}

// Collect 读出新服务器的情况。读不到的部分留空，相应的检查就跳过。
func Collect(ctx context.Context, dc *docker.Client, compose string) (*Target, error) {
	info, err := dc.Info(ctx)
	if err != nil {
		return nil, err
	}
	t := &Target{
		Arch: info.Architecture, DockerVersion: info.ServerVersion, DockerRoot: info.DockerRootDir,
		MemTotal: info.MemTotal, HasCompose: compose != "", Compose: compose, Local: dc.Local(),
		Runtimes: map[string]bool{}, VolumePlugins: map[string]bool{},
		Containers: map[string]TargetContainer{}, Projects: map[string]bool{},
		Volumes: map[string]*docker.Volume{}, Networks: map[string]TargetNetwork{}, DockerPorts: map[string]string{},
		Exists: func(p string) bool { _, err := os.Stat(p); return err == nil },
		Disk:   host.DiskOf,
	}
	if t.DockerRoot == "" {
		t.DockerRoot = "/var/lib/docker"
	}
	for name := range info.Runtimes {
		t.Runtimes[name] = true
	}
	for _, p := range info.Plugins.Volume {
		t.VolumePlugins[p] = true
	}
	list, err := dc.Containers(ctx, true)
	if err != nil {
		return nil, err
	}
	for _, c := range list {
		proj := c.Labels["com.docker.compose.project"]
		t.Containers[c.Name()] = TargetContainer{Project: proj, State: c.State}
		if proj != "" {
			t.Projects[proj] = true
		}
		for _, p := range c.Ports {
			if p.PublicPort > 0 {
				t.DockerPorts[p.Type+"/"+strconv.Itoa(p.PublicPort)] = c.Name()
			}
		}
	}
	vols, err := dc.Volumes(ctx)
	if err == nil {
		for _, v := range vols {
			if full, err := dc.InspectVolume(ctx, v.Name); err == nil {
				t.Volumes[v.Name] = full
			}
		}
	}
	nets, err := dc.Networks(ctx)
	if err == nil {
		for _, n := range nets {
			full, err := dc.InspectNetwork(ctx, n.ID)
			if err != nil {
				continue
			}
			tn := TargetNetwork{Driver: full.Driver, IPs: map[string]string{}}
			for _, c := range full.IPAM.Config {
				if c.Subnet != "" {
					tn.Subnets = append(tn.Subnets, c.Subnet)
				}
			}
			for _, c := range full.Containers {
				ip, _, _ := strings.Cut(c.IPv4Address, "/")
				if ip != "" {
					tn.IPs[ip] = c.Name
				}
			}
			t.Networks[full.Name] = tn
		}
	}
	if ls, err := host.Listeners(); err == nil {
		t.Listeners, t.ListenersOK = ls, true
	}
	if dc.Local() {
		if addrs, err := net.InterfaceAddrs(); err == nil {
			t.HostIPs = map[string]bool{}
			for _, a := range addrs {
				if ipn, ok := a.(*net.IPNet); ok {
					t.HostIPs[ipn.IP.String()] = true
				}
			}
		}
	}
	t.Pullable = func(ref string) bool {
		c, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		return dc.DistributionInspect(c, ref) == nil
	}
	return t, nil
}
