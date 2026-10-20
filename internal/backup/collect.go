package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lx969788249/dockhop/internal/compose"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/spec"
	"github.com/lx969788249/dockhop/internal/ui"
)

// 项目目录里单个超过这个大小的文件不随项目迁移（通常是日志、备份、下载的安装包），
// 会提示用户。真正的数据应该在卷或挂载目录里，那些照常迁移。
const bigFileLimit = 100 << 20

type volumeInfo struct {
	name      string
	driver    string
	anonymous bool
	raw       json.RawMessage
	opts      map[string]string
	labels    map[string]string
	bytes     int64  // -1 表示读不到大小
	imageID   string // 用哪个镜像创建读数据用的临时容器（任何本地镜像都行）
}

type bindInfo struct {
	id    string
	path  string // 真实路径
	link  string // 挂载用的路径是软链接时，记下它
	dir   bool
	bytes int64
}

type networkInfo struct {
	name    string
	raw     json.RawMessage
	driver  string
	subnets []string
	compose bool
}

type projectInfo struct {
	u        *unit
	excludes []string // 项目目录里已经作为挂载目录迁移的路径
	outside  []string // 项目目录以外被引用的文件
	skipped  []string // 太大没有迁移的文件
	drift    []string // 现在的文件和运行中的容器对不上的服务
	dirBytes int64
	outBytes int64
	inBind   bool // 项目目录本身在某个挂载目录里，随挂载目录迁移
}

type collected struct {
	sel        *selection
	volumes    []*volumeInfo
	binds      []*bindInfo
	networks   []*networkInfo
	projects   []*projectInfo
	imageBytes int64
	shared     []*item  // 没选中、但和选中的容器共用数据并且正在运行的容器
	systemOnly []string // 只重建挂载关系、不迁移内容的宿主机路径
	secrets    []string // 需要在日志里隐藏的值
}

func (c *collected) dataBytes() int64 {
	var n int64
	for _, v := range c.volumes {
		if v.bytes > 0 {
			n += v.bytes
		}
	}
	for _, b := range c.binds {
		n += b.bytes
	}
	for _, p := range c.projects {
		n += p.dirBytes + p.outBytes
	}
	return n
}

// collect 读取选中内容的全部配置和大小。只读，不改动任何东西。
func collect(ctx context.Context, dc *docker.Client, tool *compose.Tool, inv *inventory, sel *selection, screen *ui.Screen, ignore map[string]bool) (*collected, error) {
	out := &collected{sel: sel}
	usage, err := dc.VolumeUsage(ctx)
	if err != nil {
		screen.Detail("读取数据卷大小失败：%v", err)
		usage = map[string]int64{}
	}
	vols := map[string]*volumeInfo{}
	nets := map[string]*networkInfo{}
	var bindPaths []*bindInfo
	images := map[string]int64{}
	for i, it := range sel.items {
		screen.Count("读取配置", i+1, len(sel.items), it.name())
		out.secrets = append(out.secrets, spec.SecretEnvValues(it.c.Config.Env)...)
		if _, ok := images[it.c.Image]; !ok {
			size := int64(0)
			if img, err := dc.InspectImage(ctx, it.c.Image); err == nil {
				size = img.Size
			}
			images[it.c.Image] = size
		}
		for _, m := range it.c.Mounts {
			switch m.Type {
			case "volume":
				if vols[m.Name] != nil {
					continue
				}
				raw, err := dc.InspectVolumeRaw(ctx, m.Name)
				if err != nil {
					return nil, fmt.Errorf("读取数据卷 %s 失败：%v", m.Name, err)
				}
				v := &volumeInfo{name: m.Name, anonymous: spec.IsAnonymousVolume(m.Name), raw: raw, imageID: it.c.Image, bytes: -1}
				var meta struct {
					Driver  string            `json:"Driver"`
					Options map[string]string `json:"Options"`
					Labels  map[string]string `json:"Labels"`
				}
				_ = json.Unmarshal(raw, &meta)
				v.driver, v.opts, v.labels = meta.Driver, meta.Options, meta.Labels
				if size, ok := usage[m.Name]; ok {
					v.bytes = size
				}
				vols[m.Name] = v
			case "bind":
				if spec.IsSystemPath(m.Source) {
					out.systemOnly = append(out.systemOnly, m.Source)
					continue
				}
				real, err := filepath.EvalSymlinks(m.Source)
				if err != nil {
					return nil, fmt.Errorf("容器 %s 挂载的目录 %s 读不到：%v", it.name(), m.Source, err)
				}
				if spec.IsSystemPath(real) {
					out.systemOnly = append(out.systemOnly, m.Source)
					continue
				}
				b := &bindInfo{path: real}
				if real != filepath.Clean(m.Source) {
					b.link = filepath.Clean(m.Source)
				}
				bindPaths = append(bindPaths, b)
			}
		}
		for name := range it.c.NetworkSettings.Networks {
			if name == "bridge" || name == "host" || name == "none" || nets[name] != nil {
				continue
			}
			raw, err := dc.InspectNetworkRaw(ctx, name)
			if err != nil {
				return nil, fmt.Errorf("读取网络 %s 失败：%v", name, err)
			}
			n := &networkInfo{name: name, raw: raw}
			var meta struct {
				Driver string            `json:"Driver"`
				Labels map[string]string `json:"Labels"`
				IPAM   struct {
					Config []struct {
						Subnet string `json:"Subnet"`
					} `json:"Config"`
				} `json:"IPAM"`
			}
			_ = json.Unmarshal(raw, &meta)
			n.driver = meta.Driver
			_, n.compose = meta.Labels["com.docker.compose.network"]
			for _, c := range meta.IPAM.Config {
				if c.Subnet != "" {
					n.subnets = append(n.subnets, c.Subnet)
				}
			}
			nets[name] = n
		}
	}
	for _, size := range images {
		out.imageBytes += size
	}
	for _, v := range vols {
		out.volumes = append(out.volumes, v)
	}
	sort.Slice(out.volumes, func(i, j int) bool { return out.volumes[i].name < out.volumes[j].name })
	for _, n := range nets {
		out.networks = append(out.networks, n)
	}
	sort.Slice(out.networks, func(i, j int) bool { return out.networks[i].name < out.networks[j].name })

	// 去掉重复和嵌套的挂载目录：外层目录迁移了，里面的就不用再迁一次。
	sort.Slice(bindPaths, func(i, j int) bool { return len(bindPaths[i].path) < len(bindPaths[j].path) })
	for _, b := range bindPaths {
		covered := false
		for _, kept := range out.binds {
			if b.path == kept.path || strings.HasPrefix(b.path, kept.path+"/") {
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		st, err := os.Stat(b.path)
		if err != nil {
			return nil, fmt.Errorf("挂载的目录 %s 读不到：%v", b.path, err)
		}
		b.dir = st.IsDir()
		b.id = fmt.Sprintf("b%03d", len(out.binds)+1)
		screen.Note("统计目录大小 · " + b.path)
		b.bytes, _ = treeSize(b.path, nil, 0)
		out.binds = append(out.binds, b)
	}

	for _, u := range sel.units {
		if u.project == "" {
			continue
		}
		p := &projectInfo{u: u}
		for _, b := range out.binds {
			if b.path == u.workingDir || strings.HasPrefix(u.workingDir, b.path+"/") {
				p.inBind = true
			} else if strings.HasPrefix(b.path, u.workingDir+"/") {
				p.excludes = append(p.excludes, b.path)
			}
		}
		if !p.inBind {
			screen.Note("统计项目目录 · " + u.workingDir)
			p.dirBytes, p.skipped = treeSize(u.workingDir, p.excludes, bigFileLimit)
		}
		if tool != nil {
			cp := compose.Project{Name: u.project, WorkingDir: u.workingDir, Files: u.files}
			if hashes, err := tool.ConfigHashes(ctx, cp); err != nil {
				screen.Detail("Compose 项目 %s 的配置摘要算不出来：%v", u.project, err)
			} else {
				for _, it := range u.items {
					svc := it.c.Config.Labels[labelService]
					if want := it.c.Config.Labels[labelConfigHash]; want != "" && hashes[svc] != "" && hashes[svc] != want {
						p.drift = append(p.drift, svc)
					}
				}
			}
			if refs, err := tool.ReferencedFiles(ctx, cp); err != nil {
				screen.Detail("Compose 项目 %s 引用的文件找不全：%v", u.project, err)
			} else {
				for _, r := range refs {
					if _, err := os.Stat(r); err == nil && !spec.IsSystemPath(r) {
						p.outside = append(p.outside, r)
						size, _ := treeSize(r, nil, 0)
						p.outBytes += size
					}
				}
			}
		}
		out.projects = append(out.projects, p)
	}

	// 没选中但正在运行、和选中的容器共用数据的容器：打包时也要临时停止，否则数据不一致。
	selected := map[*item]bool{}
	for _, it := range sel.items {
		selected[it] = true
	}
	stopping := map[*item]bool{}
	for it := range selected {
		stopping[it] = true
	}
	for _, u := range inv.units {
		for _, it := range u.items {
			if selected[it] || !it.c.State.Running || ignore[it.name()] {
				continue
			}
			if sharesData(it, vols, out.binds) {
				out.shared = append(out.shared, it)
				stopping[it] = true
			}
		}
	}
	// 借用要停止的容器的网络、进程空间的容器（如 VPN 的 sidecar）：被借用的容器
	// 一停，它们就断了，重启后也不会自己接回去，所以一起临时停止、再一起恢复。
	for changed := true; changed; {
		changed = false
		for _, u := range inv.units {
			for _, it := range u.items {
				if stopping[it] || !it.c.State.Running || ignore[it.name()] {
					continue
				}
				for _, ref := range spec.Dependencies(it.c) {
					if dep := inv.lookup(ref); dep != nil && stopping[dep] {
						out.shared = append(out.shared, it)
						stopping[it] = true
						changed = true
						break
					}
				}
			}
		}
	}
	return out, nil
}

func sharesData(it *item, vols map[string]*volumeInfo, binds []*bindInfo) bool {
	for _, m := range it.c.Mounts {
		switch m.Type {
		case "volume":
			if vols[m.Name] != nil {
				return true
			}
		case "bind":
			if spec.IsSystemPath(m.Source) {
				continue
			}
			src := m.Source
			if real, err := filepath.EvalSymlinks(src); err == nil {
				src = real
			}
			for _, b := range binds {
				if spec.PathsOverlap(src, b.path) {
					return true
				}
			}
		}
	}
	return false
}

// treeSize 统计目录或文件的大小。excludes 里的路径不统计；bigLimit 大于 0 时，
// 超过它的文件不统计并返回它们的路径。
func treeSize(root string, excludes []string, bigLimit int64) (int64, []string) {
	var total int64
	var skipped []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		for _, e := range excludes {
			if p == e {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if bigLimit > 0 && info.Size() > bigLimit {
			skipped = append(skipped, p)
			return nil
		}
		total += info.Size()
		return nil
	})
	return total, skipped
}
