package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/compose"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/host"
	"github.com/lx969788249/dockhop/internal/logging"
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
	path  string            // 真实路径
	links []bundle.BindLink // 挂载用的路径是软链接时，记下它们（多个软链接可能指向同一个目录）
	dir   bool
	bytes int64
}

// addLinks 记下软链接（去掉重复的）。
func (b *bindInfo) addLinks(links []bundle.BindLink) {
	for _, l := range links {
		dup := false
		for _, k := range b.links {
			dup = dup || k.Link == l.Link
		}
		if !dup {
			b.links = append(b.links, l)
		}
	}
}

// link 是指向 path 本身的第一个软链接（写给只认一个软链接的老版本）。
func (b *bindInfo) link() string {
	for _, l := range b.links {
		if l.Target == b.path {
			return l.Link
		}
	}
	return ""
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
	inBind   bool     // 项目目录本身在某个挂载目录里，随挂载目录迁移
	check    []string // inBind 时另外打包的配置文件，迁入前检查用（见 compose.CheckFiles）
}

type collected struct {
	sel        *selection
	inv        *inventory // 读配置时的全部容器，停机之前按它核对（见 recheck）
	volumes    []*volumeInfo
	binds      []*bindInfo
	networks   []*networkInfo
	projects   []*projectInfo
	imageBytes int64
	layerBytes map[*item]int64 // 容器里没挂载出来的改动有多大（停机期间要打包）
	shared     []*item         // 没选中、但和选中的容器共用数据并且正在运行的容器
	systemOnly []string        // 只重建挂载关系、不迁移内容的宿主机路径
	secrets    []string        // 需要在日志里隐藏的值
}

// hostPaths 是打包时要读的宿主机上的地方（真实路径）：挂载目录、Compose 项目目录（在挂载目录里的
// 随挂载目录打包）和项目目录外的文件。
func (c *collected) hostPaths() []string {
	var out []string
	for _, b := range c.binds {
		out = append(out, b.path)
	}
	for _, p := range c.projects {
		if !p.inBind {
			out = append(out, host.RealPath(p.u.workingDir))
		}
		for _, o := range p.outside {
			out = append(out, host.RealPath(o))
		}
	}
	return out
}

// volumeNames 是要打包的数据卷的名字。
func (c *collected) volumeNames() map[string]bool {
	m := map[string]bool{}
	for _, v := range c.volumes {
		m[v.name] = true
	}
	return m
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
	for _, b := range c.layerBytes {
		n += b
	}
	return n
}

// collect 读取选中内容的全部配置和大小。只读，不改动任何东西。
func collect(ctx context.Context, dc *docker.Client, tool *compose.Tool, inv *inventory, sel *selection, screen *ui.Screen, ignore map[string]bool) (*collected, error) {
	out := &collected{sel: sel, inv: inv, layerBytes: map[*item]int64{}}
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
		screen.Count("读取容器配置", i+1, len(sel.items), it.name())
		out.secrets = append(out.secrets, logging.EnvSecrets(it.c.Config.Env)...)
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
				real, system, err := approveRoot(bindRoot, m.Source, "")
				if err != nil {
					return nil, fmt.Errorf("容器 %s 挂载的目录 %s 读不到：%v", it.name(), m.Source, err)
				}
				if system {
					out.systemOnly = append(out.systemOnly, m.Source)
					continue
				}
				b := &bindInfo{path: real}
				if real != filepath.Clean(m.Source) {
					b.links = []bundle.BindLink{{Link: filepath.Clean(m.Source), Target: real}}
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
	// 容器里没挂载出来的改动要在停机期间打包：先量一下，太大的提前提醒（停机会更久）。
	for i, it := range sel.items {
		if it.c.HostConfig.ReadonlyRootfs {
			continue
		}
		screen.Count("统计容器里的改动", i+1, len(sel.items), it.name())
		size, err := dc.ContainerSize(ctx, it.c.ID)
		if err != nil {
			screen.Detail("读不到容器 %s 里改动的大小：%v", it.name(), err)
			continue
		}
		out.layerBytes[it] = size
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
	// 数据只迁一份，但每个软链接都要记下：容器挂载时用的可能是其中任何一个。
	for _, b := range bindPaths {
		covered := false
		for _, kept := range out.binds {
			if b.path == kept.path || strings.HasPrefix(b.path, kept.path+"/") {
				kept.addLinks(b.links)
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
		screen.Note("统计目录大小 · " + ui.ShortPath(b.path))
		b.bytes, _ = treeSize(ctx, b.path, nil, 0)
		out.binds = append(out.binds, b)
	}

	for _, u := range sel.units {
		if u.project == "" {
			continue
		}
		p := &projectInfo{u: u}
		stop := func(err error) error {
			return screen.StepFailed("Compose 项目 "+u.project+" 没法迁移："+err.Error(), "",
				"把它改成普通的目录或文件（或者让它指向一个单独的地方）后重新运行，或者不迁移这个项目")
		}
		// 挂载目录记的是真实路径，项目目录本身是软链接时也按它的真实路径比较。
		dir, _, err := approveRoot(projectRoot, u.workingDir, "")
		if err != nil {
			return nil, stop(err)
		}
		for _, b := range out.binds {
			if b.path == dir || strings.HasPrefix(dir, b.path+"/") {
				p.inBind = true
			} else if strings.HasPrefix(b.path, dir+"/") {
				p.excludes = append(p.excludes, b.path)
			}
		}
		if !p.inBind {
			screen.Note("统计项目目录 · " + ui.ShortPath(u.workingDir))
			p.dirBytes, p.skipped = treeSize(ctx, u.workingDir, p.excludes, bigFileLimit)
		}
		cp := compose.Project{Name: u.project, WorkingDir: u.workingDir, Files: u.files}
		if p.inBind {
			for _, f := range compose.CheckFiles(cp) {
				_, system, err := approveRoot(checkRoot, f, dir)
				if err != nil {
					return nil, stop(err)
				}
				if !system {
					p.check = append(p.check, f)
				}
			}
		}
		files, unknown := compose.DeclaredFiles(cp)
		for _, r := range files {
			_, system, err := approveRoot(outsideRoot, r, dir)
			if err != nil {
				return nil, stop(err)
			}
			if system {
				continue
			}
			p.outside = append(p.outside, r)
			size, _ := treeSize(ctx, r, nil, 0)
			p.outBytes += size
		}
		if len(unknown) > 0 {
			screen.Warn("Compose 项目 %s 的配置里有带变量的路径（%s），没法确定是哪个文件，不会一起迁移；迁入后请确认新服务器上有这些文件",
				u.project, ui.Names(unknown))
		}
		if tool != nil {
			if services, err := tool.Services(ctx, cp); err != nil {
				screen.Detail("Compose 项目 %s 的配置算不出来：%v", u.project, err)
			} else if services != nil {
				for _, it := range u.items {
					svc := it.c.Config.Labels[labelService]
					if why := driftReason(services[svc], it.c); why != "" {
						p.drift = append(p.drift, svc+"（"+why+"）")
					}
				}
			}
		}
		out.projects = append(out.projects, p)
	}

	shared, err := sharers(inv, sel.items, out.volumeNames(), out.hostPaths(), ignore, screen)
	if err != nil {
		return nil, err
	}
	out.shared = shared
	return out, nil
}

// sharers 找出打包时要和选中的容器 sel 一起临时停止的容器，菜单里不能选的容器（docker compose run
// 的临时容器等）也算：没选中、正在运行、和选中的容器共用数据的（见 sharesData），否则打包时它还在写，
// 数据前后对不上；借用要停止的容器的网络、进程空间的（如 VPN 的 sidecar），被借用的容器一停它们就断了，
// 重启后也不会自己接回去，所以一起临时停止、再一起恢复。Swarm 服务的容器一停就会被重新创建，停不住，
// 遇到时停下。
func sharers(inv *inventory, sel []*item, vols map[string]bool, paths []string, ignore map[string]bool, screen *ui.Screen) ([]*item, error) {
	stopping := map[*item]bool{}
	for _, it := range sel {
		stopping[it] = true
	}
	var out []*item
	share := func(it *item) error {
		if spec.StopRiskOf(it.c) == spec.StopReplaced {
			svc := it.c.Config.Labels[labelSwarmService]
			return screen.StepFailed(fmt.Sprintf("容器 %s 属于 Swarm 服务 %s，和选中的容器共用数据，但它一停就会被 Swarm 重新启动，打包的数据没法保证一致，已取消（什么都没有改动）", it.name(), svc),
				"", "先执行 docker service scale "+svc+"=0，迁出后再改回原来的个数")
		}
		out = append(out, it)
		stopping[it] = true
		return nil
	}
	for _, it := range inv.all {
		if stopping[it] || !it.c.State.Running || ignore[it.name()] || !sharesData(it, vols, paths) {
			continue
		}
		if err := share(it); err != nil {
			return nil, err
		}
	}
	for changed := true; changed; {
		changed = false
		for _, it := range inv.all {
			if stopping[it] || !it.c.State.Running || ignore[it.name()] {
				continue
			}
			for _, ref := range spec.Dependencies(it.c) {
				if dep := inv.lookup(ref); dep != nil && stopping[dep] {
					if err := share(it); err != nil {
						return nil, err
					}
					changed = true
					break
				}
			}
		}
	}
	return out, nil
}

// recheck 在停机之前再看一次和这次迁出有关的容器：选中的、要一起停的，以及按现在的样子会要一起停的。
// 从读容器到停机，中间可能隔了好一会儿（选菜单、回答问题），这期间有容器启动、停下、暂停、改名、被重建，
// 或者新建了共用数据的容器，再按读的时候的样子停机打包就不对了：新启动的共用数据的容器没停下，打包时
// 它还在写；已经停下的容器打包完会被启动起来；被重建的容器打包的是旧的配置。有变化就停下，什么都不动，
// 重新运行会按现在的样子重新读。
func (r *run) recheck(ctx context.Context, col *collected) error {
	s := r.Screen
	list, err := r.Docker.Containers(ctx, true)
	if err != nil {
		return s.StepFailed("读取容器列表失败，为了安全没有停止任何容器", err.Error(), "")
	}
	// 读过的容器照旧用读的时候的配置（配置变了，ID 也会变），名字和状态按现在的；新出现的读一下。
	now := &inventory{byName: map[string]*item{}, byID: map[string]*item{}}
	for _, sum := range list {
		it := col.inv.byID[sum.ID]
		if it == nil {
			c, raw, err := r.Docker.InspectContainer(ctx, sum.ID)
			if docker.IsNotFound(err) {
				continue // 刚好被删掉了
			}
			if err != nil {
				return s.StepFailed("读取容器 "+sum.Name()+" 失败，为了安全没有停止任何容器", err.Error(), "")
			}
			it = &item{c: c, raw: raw}
		}
		c := *it.c
		c.Name = "/" + sum.Name()
		c.State.Status = sum.State
		c.State.Running = sum.State == "running" || sum.State == "restarting" || sum.State == "paused"
		c.State.Paused = sum.State == "paused"
		cur := &item{c: &c, raw: it.raw}
		now.all = append(now.all, cur)
		now.byName[cur.name()] = cur
		now.byID[c.ID] = cur
	}
	var sel []*item
	for _, it := range col.sel.items {
		if cur := now.byID[it.c.ID]; cur != nil {
			sel = append(sel, cur)
		}
	}
	shared, err := sharers(now, sel, col.volumeNames(), col.hostPaths(), r.ignored(), s)
	if err != nil {
		return err
	}
	before := scene(append(slices.Clone(col.sel.items), col.shared...))
	after := scene(append(sel, shared...))
	var changed []string
	for _, e := range append(slices.Clone(before), after...) {
		if slices.Contains(before, e) != slices.Contains(after, e) {
			changed = append(changed, strings.Fields(e)[0])
		}
	}
	if len(changed) > 0 {
		slices.Sort(changed)
		return s.StepFailed("容器 "+strings.Join(slices.Compact(changed), "、")+" 在读取配置之后有了变化（启动、停下、暂停、改名或者被重建了），为了安全没有停止任何容器", "",
			"重新运行迁出，会按现在的样子重新读取")
	}
	return nil
}

// scene 是这些容器的名字、ID 和状态（运行、暂停、停止），比较前后有没有变化用。
func scene(items []*item) []string {
	var out []string
	for _, it := range items {
		state := "stopped"
		switch {
		case it.c.State.Paused:
			state = "paused"
		case it.c.State.Running:
			state = "running"
		}
		out = append(out, it.name()+" "+it.c.ID+" "+state)
	}
	slices.Sort(out)
	return out
}

// sharesData 表示容器有没有用到要打包的数据卷 vols，或者挂载了要打包的宿主机上的地方 paths（真实路径）。
func sharesData(it *item, vols map[string]bool, paths []string) bool {
	for _, m := range it.c.Mounts {
		switch m.Type {
		case "volume":
			if vols[m.Name] {
				return true
			}
		case "bind":
			if spec.IsSystemPath(m.Source) {
				continue
			}
			src := host.RealPath(m.Source)
			for _, p := range paths {
				if spec.PathsOverlap(src, p) {
					return true
				}
			}
		}
	}
	return false
}

// rootPolicy 是一种要从宿主机上打包的路径过检查的规则（见 approveRoot）。
type rootPolicy struct {
	what       string // 给人看的叫法
	mustExist  bool   // 读不到就停下
	skipSystem bool   // 它本身或者它指向的地方是系统路径时不打包（新服务器上自己有）
	guardLink  bool   // 经过软链接时不能指向系统路径、它自己的上级目录或者项目目录的上级目录：会把整个系统目录、或者别的项目一起打进去
}

// 要从宿主机上打包的每一处都过 approveRoot，各种路径的规则都写在这里。不经过软链接、配置里直接写的照原样
// 打包：项目目录哪怕是系统路径（例如 compose.yml 直接放在 /root 下），项目目录外的文件哪怕是项目的上级
// 目录（例如 build: ..，构建上下文是仓库根目录）。
var (
	bindRoot    = rootPolicy{what: "挂载的目录", mustExist: true, skipSystem: true}
	projectRoot = rootPolicy{what: "项目目录", guardLink: true}
	outsideRoot = rootPolicy{what: "项目目录外的文件", skipSystem: true, guardLink: true}
	checkRoot   = rootPolicy{what: "迁入前检查用的配置文件", skipSystem: true, guardLink: true}
)

// approveRoot 按 pol 检查一处要从宿主机上打包的路径，返回它的实际位置（解析掉软链接，打包的是它指向的
// 内容，见 walkRoot）。project 是它所属的 Compose 项目目录的实际位置，不属于项目时为空。system 表示它是
// 系统路径、不打包；返回错误表示不能迁移，要停下。
func approveRoot(pol rootPolicy, path, project string) (real string, system bool, err error) {
	if pol.skipSystem && spec.IsSystemPath(path) {
		return "", true, nil
	}
	real = host.RealPath(path)
	if pol.mustExist {
		if real, err = filepath.EvalSymlinks(path); err != nil {
			return "", false, err
		}
	}
	if pol.skipSystem && spec.IsSystemPath(real) {
		return real, true, nil
	}
	if pol.guardLink && real != filepath.Clean(path) {
		around := func(p string) bool {
			return p != "" && (p == real || strings.HasPrefix(p, real+string(filepath.Separator)))
		}
		if spec.IsSystemPath(real) || around(host.RealPath(filepath.Dir(path))) || around(project) {
			return "", false, fmt.Errorf("%s %s 经过软链接指向 %s（系统目录，或者它自己、项目目录的上级目录），打包会把整个目录一起打进去", pol.what, path, real)
		}
	}
	return real, false, nil
}

// treeSize 统计目录或文件的大小，和打包时一样，root 本身是软链接时统计它指向的内容（见 walkRoot）。
// excludes 里的路径不统计（按原来的写法或者真实路径）；bigLimit 大于 0 时，超过它的文件不统计并
// 返回它们的路径。
func treeSize(ctx context.Context, root string, excludes []string, bigLimit int64) (int64, []string) {
	var total int64
	var skipped []string
	_ = walkRoot(root, func(p, real string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return filepath.SkipAll // 按了 Ctrl+C：不用统计完，调用的地方接着会发现
		}
		if err != nil {
			return nil
		}
		for _, e := range excludes {
			if p == e || real == e {
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

// driftReason 核对按现在的文件算出来的服务配置和正在运行的容器：镜像不同、或者
// 文件里写的环境变量值和容器里的不一样时，说明启动后改过文件，或者启动时用了
// shell 里临时设置的变量。返回不一样的地方；一致时返回空。
func driftReason(svc compose.Service, c *docker.Container) string {
	if svc.Image != "" && svc.Image != c.Config.Image {
		return "镜像 " + c.Config.Image + " → " + svc.Image
	}
	env := map[string]string{}
	for _, e := range c.Config.Env {
		k, v, _ := strings.Cut(e, "=")
		env[k] = v
	}
	var diff []string
	for k, v := range svc.Environment {
		if v == nil {
			continue // 只写了变量名，值来自启动时的环境，无法核对
		}
		if cur, ok := env[k]; !ok || cur != *v {
			diff = append(diff, k)
		}
	}
	if len(diff) == 0 {
		return ""
	}
	sort.Strings(diff)
	return "环境变量 " + strings.Join(diff, "、")
}
