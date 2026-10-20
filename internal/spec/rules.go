// Package spec 是迁出和迁入共用的容器规则：哪些挂载路径属于宿主机本身、
// 容器之间的依赖、哪些可写层改动值得迁移、哪些环境变量是秘密。
package spec

import (
	"path"
	"regexp"
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

// 运行时临时目录里的改动不算可写层改动。
var runtimeDir = regexp.MustCompile(`^/(tmp|var/tmp|run|var/run|var/cache|root/\.cache|dev|proc|sys)(/|$)`)

// WritableChanged 表示可写层里有没有值得迁移的改动：只因为子项变化而被标记为
// “修改”的父目录、运行时临时目录里的改动都不算。
func WritableChanged(changes []docker.Change) bool {
	parents := map[string]bool{}
	for _, c := range changes {
		for p := path.Dir(c.Path); p != "/" && p != "."; p = path.Dir(p) {
			parents[p] = true
		}
	}
	for _, c := range changes {
		if c.Kind == 0 && parents[c.Path] {
			continue
		}
		if runtimeDir.MatchString(c.Path) {
			continue
		}
		return true
	}
	return false
}

// 名字像密码、密钥、令牌的环境变量，它们的值要在日志里隐藏。
var secretEnvName = regexp.MustCompile(`(?i)pass|pwd|secret|token|key|credential|auth|salt|cookie|session|dsn|database_url`)

// SecretEnvValues 返回环境变量里需要隐藏的值。
func SecretEnvValues(env []string) []string {
	var out []string
	for _, e := range env {
		k, v, ok := strings.Cut(e, "=")
		if ok && v != "" && secretEnvName.MatchString(k) {
			out = append(out, v)
		}
	}
	return out
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
