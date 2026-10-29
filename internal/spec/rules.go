// Package spec 是迁出和迁入共用的容器规则：哪些挂载路径属于宿主机本身、
// 容器之间的依赖、哪些可写层改动值得迁移、哪些环境变量是秘密。
package spec

import (
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/lx969788249/dockhop/internal/docker"
)

// 宿主机系统路径只在新服务器上重建挂载关系，不迁移内容：这些路径属于宿主机本身
// （时区、DNS、内核模块、伪文件系统、Docker 自己的目录等），覆盖新服务器会把它弄坏。
// 第一组连同子路径都算；第二组是只在完全相同时才算的大目录。
var (
	systemPrefixes = []string{
		"/proc", "/sys", "/dev", "/run", "/var/run", "/boot", "/lib/modules", "/usr/lib/modules",
		"/var/lib/docker", "/var/lib/containerd", "/var/log", "/etc/docker", "/etc/systemd",
		"/lib/systemd", "/usr/lib/systemd", "/etc/localtime", "/etc/timezone", "/usr/share/zoneinfo",
		"/etc/hostname", "/etc/hosts", "/etc/resolv.conf", "/etc/machine-id", "/etc/os-release",
		"/usr/lib/os-release", "/etc/passwd", "/etc/group", "/etc/shadow", "/etc/gshadow",
		"/etc/ssl", "/etc/pki", "/etc/ca-certificates",
	}
	systemExact = []string{
		"/", "/etc", "/usr", "/usr/lib", "/usr/bin", "/usr/local", "/var", "/var/lib", "/lib", "/lib64",
		"/bin", "/sbin", "/opt", "/home", "/root", "/srv", "/mnt", "/media", "/tmp",
	}
)

// IsSystemPath 表示挂载的宿主机路径是否属于宿主机本身（不迁移内容）。
func IsSystemPath(p string) bool {
	p = path.Clean(p)
	for _, e := range systemExact {
		if p == e {
			return true
		}
	}
	for _, s := range systemPrefixes {
		if p == s || strings.HasPrefix(p, s+"/") {
			return true
		}
	}
	return IsDockerSocket(p)
}

// IsDockerSocket 表示路径是不是容器引擎的 socket（新服务器挂载它自己的）。
func IsDockerSocket(p string) bool {
	base := path.Base(p)
	return base == "docker.sock" || base == "podman.sock" || base == "containerd.sock"
}

// PathsOverlap 表示两个路径是否相同或一个在另一个里面（按路径层级，不按字符串前缀）。
func PathsOverlap(a, b string) bool {
	a, b = path.Clean(a), path.Clean(b)
	if a == "/" || b == "/" {
		return true
	}
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// IsAnonymousVolume 表示卷是不是匿名卷（镜像声明、没有名字，只有随机编号）。
var anonymousVolume = regexp.MustCompile(`^[0-9a-f]{64}$`)

func IsAnonymousVolume(name string) bool { return anonymousVolume.MatchString(name) }

// Dependencies 返回容器依赖的其他容器（名字或 ID）：借用网络、进程、IPC 命名空间
// 的容器，--link 和 --volumes-from 引用的容器。离开它们，这个容器无法启动。
func Dependencies(c *docker.Container) []string {
	var deps []string
	for _, mode := range []string{c.HostConfig.NetworkMode, c.HostConfig.PidMode, c.HostConfig.IpcMode} {
		if ref, ok := strings.CutPrefix(mode, "container:"); ok && ref != "" {
			deps = append(deps, ref)
		}
	}
	// Links 形如 "/db:/web/database"，第一段是被链接的容器。
	for _, l := range c.HostConfig.Links {
		target, _, _ := strings.Cut(l, ":")
		if target = strings.TrimPrefix(target, "/"); target != "" {
			deps = append(deps, target)
		}
	}
	// VolumesFrom 形如 "data" 或 "data:ro"。
	for _, v := range c.HostConfig.VolumesFrom {
		if name, _, _ := strings.Cut(v, ":"); name != "" {
			deps = append(deps, name)
		}
	}
	return deps
}

// 运行时临时目录里的改动不算可写层改动；Docker 自己管理的几个文件也不算。
var (
	runtimeDir    = regexp.MustCompile(`^/(tmp|var/tmp|run|var/run|var/cache|root/\.cache|dev|proc|sys)(/|$)`)
	dockerManaged = map[string]bool{"/etc/hosts": true, "/etc/hostname": true, "/etc/resolv.conf": true, "/.dockerenv": true,
		"/sbin/docker-init": true} // --init 时 Docker 放进去的程序
)

// LayerChanges 从 docker diff 的结果里找出值得迁移的改动：新增或修改的路径（只取最上层，
// 新增的目录整个算一项）和删除的路径。这些不算：运行时临时目录、Docker 自己管理的文件、
// 挂载点（新版 Docker 会把为挂载创建的空目录也算作新增）和它们的上级目录、只因为子项
// 变化而被标记为“修改”的目录。
func LayerChanges(changes []docker.Change, mounts []string) (keep, deleted []string) {
	return layerChanges(changes, mounts, false)
}

// AllLayerChanges 和 LayerChanges 一样，只是运行时临时目录里的改动也算（--snapshot always）。
func AllLayerChanges(changes []docker.Change, mounts []string) (keep, deleted []string) {
	return layerChanges(changes, mounts, true)
}

func layerChanges(changes []docker.Change, mounts []string, withRuntime bool) (keep, deleted []string) {
	parents := map[string]bool{}
	for _, c := range changes {
		for p := path.Dir(c.Path); p != "/" && p != "."; p = path.Dir(p) {
			parents[p] = true
		}
	}
	// 挂载点和 Docker 放进去的文件，连同为它们新建的上级目录（例如镜像里本来没有
	// /sbin，--init 时 Docker 为 /sbin/docker-init 建的 /sbin）都不算改动。
	mountDirs := map[string]bool{}
	for _, m := range append(append([]string(nil), mounts...), managedPaths()...) {
		for p := path.Clean(m); p != "/" && p != "."; p = path.Dir(p) {
			mountDirs[p] = true
		}
	}
	var adds, dels []string
	for _, c := range changes {
		p := path.Clean(c.Path)
		if (!withRuntime && runtimeDir.MatchString(p)) || dockerManaged[p] || mountDirs[p] || p == "/dev" || strings.HasPrefix(p, "/dev/") || strings.HasPrefix(p, "/proc") || strings.HasPrefix(p, "/sys") {
			continue
		}
		switch c.Kind {
		case 2:
			dels = append(dels, p)
		case 0:
			if !parents[p] {
				adds = append(adds, p)
			}
		default:
			adds = append(adds, p)
		}
	}
	return topmost(adds), topmost(dels)
}

func managedPaths() []string {
	out := make([]string, 0, len(dockerManaged))
	for p := range dockerManaged {
		out = append(out, p)
	}
	return out
}

// topmost 去掉已经被上级路径包含的路径。
func topmost(paths []string) []string {
	sort.Strings(paths)
	var out []string
	for _, p := range paths {
		if len(out) > 0 {
			last := out[len(out)-1]
			if p == last || strings.HasPrefix(p, last+"/") {
				continue
			}
		}
		out = append(out, p)
	}
	return out
}

// WritableChanged 表示可写层里有没有值得迁移的改动。
func WritableChanged(changes []docker.Change, mounts []string) bool {
	keep, deleted := LayerChanges(changes, mounts)
	return len(keep)+len(deleted) > 0
}

// 面板（宝塔、1Panel）自身的容器，迁移后可能导致面板异常，默认不选。
func IsPanelContainer(c *docker.Container) bool {
	img := strings.ToLower(c.Config.Image)
	for _, k := range []string{"btpanel", "baota", "1panel"} {
		if strings.Contains(img, k) {
			return true
		}
	}
	for _, m := range c.Mounts {
		if strings.HasPrefix(m.Source, "/www/server/panel") || strings.HasPrefix(m.Source, "/opt/1panel") {
			return true
		}
	}
	return false
}

// IsSwarmTask 表示容器是不是 Swarm 服务创建的（不支持迁移）。
func IsSwarmTask(labels map[string]string) bool {
	_, ok := labels["com.docker.swarm.service.id"]
	return ok
}
