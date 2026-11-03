package restore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/compose"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/preflight"
	"github.com/lx969788249/dockhop/internal/spec"
)

// 创建容器：独立容器按原始配置创建，Compose 项目的容器交给 Compose 创建，再放回容器里的改动和
// 匿名卷的数据；网络按迁移记录创建。这里只创建、不启动，先后顺序由 startup.go 安排。

// citem 是要迁入的一个容器。
type citem struct {
	m     bundle.Container
	raw   json.RawMessage
	c     *docker.Container
	newID string
	img   string // 创建容器用的镜像（算过之后缓存）

	startedAt time.Time     // 在新服务器上启动它的时候
	ready     bool          // 已经确认就绪
	extra     time.Duration // 等它就绪时额外多给的时间（见 unit.again）
}

func (it *citem) name() string { return it.m.Name }

func (it *citem) running() bool {
	return it.c.State.Running || it.c.State.Paused || it.c.State.Status == "restarting"
}

// 容器里没挂载出来的改动分两部分恢复：删除的文件做进镜像（Docker 没有删除容器里文件的接口，
// 只能在临时容器里删掉再 commit，见 imageWithout）；新增和修改的文件在容器创建之后、启动之前
// 写进这个容器（applyChanges）。这样每个容器的新增、修改都是它自己的，镜像只承载删除。

// baseImage 是创建容器用的原镜像：旧版本镜像、改名后的 digest 镜像，或者原来的镜像名。
func (r *run) baseImage(it *citem) string {
	if it.m.OldImage != "" {
		return it.m.OldImage
	}
	if v := r.imageAs[it.c.Config.Image]; v != "" {
		return v
	}
	return it.c.Config.Image
}

func deletedOf(it *citem) []string {
	if it.m.Changes == nil {
		return nil
	}
	return it.m.Changes.Deleted
}

// image 是创建这个独立容器用的镜像：原镜像，容器里删除过文件时是删掉这些文件的镜像。
func (r *run) image(ctx context.Context, it *citem) (string, error) {
	if it.img != "" {
		return it.img, nil
	}
	img, err := r.imageWithout(ctx, r.baseImage(it), deletedOf(it), it.m.Name)
	if err != nil {
		return "", fmt.Errorf("容器 %s 里删除过的文件没能在新容器里删除：%v", it.m.Name, err)
	}
	it.img = img
	return img, nil
}

// serviceImage 是 Compose 服务的镜像。同一个服务的副本只能用同一个镜像（覆盖文件按服务写），
// 所以镜像里只删掉每个副本都删过的文件；只有部分副本删过的文件，别的副本里还在，不能删，
// 在删过它的副本里就会重新出现，这里明确提示。新增和修改的文件不在镜像里，每个副本各写各的。
func (r *run) serviceImage(ctx context.Context, project, svc string, reps []*citem) (string, error) {
	base := r.baseImage(reps[0])
	for _, it := range reps[1:] {
		if b := r.baseImage(it); b != base {
			r.Screen.Warn("Compose 服务 %s 的副本用的镜像不一样（%s、%s），都按第一个副本的镜像创建", svc, base, b)
			break
		}
	}
	common := deletedOf(reps[0])
	for _, it := range reps[1:] {
		common = intersect(common, deletedOf(it))
	}
	for _, it := range reps {
		if extra := len(deletedOf(it)) - len(common); extra > 0 {
			r.Screen.Warn("容器 %s 里删除过的 %d 个文件在新服务器上会重新出现：同一个 Compose 服务的副本只能用同一个镜像，"+
				"别的副本里还有这些文件", it.m.Name, extra)
		}
	}
	img, err := r.imageWithout(ctx, base, common, project+"-"+svc)
	if err != nil {
		return "", fmt.Errorf("删除过的文件没能在 Compose 服务 %s 的新容器里删除：%v", svc, err)
	}
	for _, it := range reps {
		it.img = img
	}
	return img, nil
}

func intersect(a, b []string) []string {
	in := map[string]bool{}
	for _, v := range b {
		in[v] = true
	}
	var out []string
	for _, v := range a {
		if in[v] {
			out = append(out, v)
		}
	}
	return out
}

// imageWithout 做出删掉 deleted 这些文件的镜像 dockhop-image:<name>：在临时容器里删除，再在
// 这台服务器上 commit（本机 commit 没问题，只是导出 commit 的镜像有问题）。镜像的配置和原镜像
// 相同，容器的配置照常来自原始配置。没有要删的文件时就是原镜像。
func (r *run) imageWithout(ctx context.Context, base string, deleted []string, name string) (string, error) {
	if len(deleted) == 0 {
		return base, nil
	}
	if r.vio.helperBin == "" {
		r.Screen.Warn("%s 里删除过 %d 个文件，Docker 不在这台服务器上，没法在新容器里删除它们（文件还在，来自镜像）", name, len(deleted))
		return base, nil
	}
	dc := r.Docker
	baseRaw, err := dc.InspectImageRaw(ctx, base)
	if err != nil {
		return "", err
	}
	var baseImg struct {
		Config struct {
			Entrypoint []string `json:"Entrypoint"`
			Cmd        []string `json:"Cmd"`
			User       string   `json:"User"`
		} `json:"Config"`
	}
	_ = json.Unmarshal(baseRaw, &baseImg)
	bc := baseImg.Config
	// 临时容器只改入口命令（删除文件）和用户（root 才能删）；commit 时把入口命令、命令和
	// 用户改回原镜像的，镜像的其他配置本来就和原镜像一样。
	tmp := map[string]any{
		"Image": base, "Entrypoint": append([]string{"/dockhop-helper", "__remove"}, deleted...),
		"HostConfig": map[string]any{"NetworkMode": "none", "Binds": []string{r.vio.helperBin + ":/dockhop-helper:ro"}},
	}
	commitCfg := map[string]any{"Entrypoint": nonNil(bc.Entrypoint), "Cmd": nonNil(bc.Cmd)}
	if bc.User != "" && bc.User != "root" && bc.User != "0" && bc.User != "0:0" {
		tmp["User"] = "0:0"
		commitCfg["User"] = bc.User
	}
	// 这个临时容器不能带 dockhop.helper 标签：commit 时标签会进到镜像里，再跟着镜像到
	// 迁入的容器上。所以记进事务日志，程序被强制结束后由回滚或提交后的清理删除。
	helper := fmt.Sprintf("dockhop-helper-%s-%d", runID, r.vio.n.Add(1))
	if err := r.j.add(Entry{Op: opHelper, Name: helper}); err != nil {
		return "", err
	}
	id, err := dc.CreateContainer(ctx, helper, tmp)
	if err != nil {
		return "", err
	}
	defer dc.RemoveContainerAndVolumes(context.WithoutCancel(ctx), id)
	if err := dc.StartContainer(ctx, id); err != nil {
		return "", err
	}
	if code, err := dc.WaitContainer(ctx, id); err != nil || code != 0 {
		return "", fmt.Errorf("删除文件失败（退出码 %d）：%v %s", code, err, dc.ContainerLogs(ctx, id, 5))
	}
	tag := name
	if len(tag) > 128 {
		tag = tag[:128]
	}
	if err := r.noteTags(ctx, []string{"dockhop-image:" + tag}); err != nil {
		return "", err
	}
	if _, err := dc.CommitWithConfig(ctx, id, "dockhop-image", tag, commitCfg); err != nil {
		return "", err
	}
	return "dockhop-image:" + tag, nil
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// applyChanges 在容器创建之后、启动之前，把它里面新增和修改的文件写进去。
func (r *run) applyChanges(ctx context.Context, it *citem) error {
	ch := it.m.Changes
	if ch == nil || len(ch.Paths) == 0 {
		return nil
	}
	rc := bundle.OpenStream(r.dataDir, "changes/"+it.m.Name, ch.Parts)
	defer rc.Close()
	if err := r.Docker.PutArchive(ctx, it.newID, "/", rc); err != nil {
		return fmt.Errorf("容器 %s 里的改动没能写入：%v", it.m.Name, err)
	}
	return nil
}

// createContainer 用原始配置创建一个独立容器（不启动）。
func (r *run) createContainer(ctx context.Context, it *citem) error {
	img, err := r.image(ctx, it)
	if err != nil {
		return err
	}
	imgRaw, _ := r.Docker.InspectImageRaw(ctx, img)
	built, err := spec.Rebuild{
		Raw: it.raw, Image: img, ImageConfig: imgRaw,
		Source: spec.SourceDefaults{CgroupVersion: r.m.Source.CgroupVersion, LoggingDriver: r.m.Source.LoggingDriver, DefaultRuntime: r.m.Source.DefaultRuntime},
		NameOf: r.nameOfOld,
		Port: func(ip, port, cport, proto string) (string, string, bool) {
			ch, ok := r.d.PortFor(preflight.PortRef{Container: it.m.Name, HostIP: ip, HostPort: port, ContainerPort: cport, Proto: proto})
			if !ok {
				return ip, port, true
			}
			if ch.Drop {
				return "", "", false
			}
			return ch.NewIP, ch.NewPort, true
		},
		AutoIP:       func(n string) bool { return r.d.AutoIPFor(it.m.Name, n) },
		MultiNetwork: r.Docker.AtLeast("1.44"),
	}.Build()
	if err != nil {
		return fmt.Errorf("容器 %s 的配置有问题：%v", it.m.Name, err)
	}
	if err := r.j.add(Entry{Op: opContainer, Name: it.m.Name}); err != nil {
		return err
	}
	id, err := r.Docker.CreateContainer(ctx, it.m.Name, built.Body)
	if err != nil {
		return fmt.Errorf("容器 %s 创建失败：%v", it.m.Name, err)
	}
	it.newID = id
	for _, c := range built.Connects {
		if err := r.Docker.ConnectNetwork(ctx, c.Network, id, c.Endpoint); err != nil {
			return fmt.Errorf("容器 %s 连接网络 %s 失败：%v", it.m.Name, c.Network, err)
		}
	}
	return nil
}

// nameOfOld 把旧服务器上的容器 ID（或前缀）换成容器名。
func (r *run) nameOfOld(ref string) string {
	for _, c := range r.m.Containers {
		if c.ID == ref || c.Name == ref || (len(ref) >= 12 && strings.HasPrefix(c.ID, ref)) {
			return c.Name
		}
	}
	return ""
}

// transplantAnonymous 把迁移过来的匿名卷数据放进新容器的匿名卷：匿名卷只有随机编号，
// 新容器创建时 Docker 给它新建了一个（可能还复制了镜像里的文件），清空后写入迁移的数据。
func (r *run) transplantAnonymous(ctx context.Context, it *citem, newID string) error {
	var src struct {
		Mounts []docker.Mount `json:"Mounts"`
	}
	_ = json.Unmarshal(it.raw, &src)
	var cur *docker.Container
	for _, m := range src.Mounts {
		if m.Type != "volume" || !spec.IsAnonymousVolume(m.Name) {
			continue
		}
		v, ok := r.anonVolumes[m.Name]
		if !ok {
			continue
		}
		if cur == nil {
			c, _, err := r.Docker.InspectContainer(ctx, newID)
			if err != nil {
				return err
			}
			cur = c
		}
		target := ""
		for _, nm := range cur.Mounts {
			if nm.Type == "volume" && nm.Destination == m.Destination {
				target = nm.Name
			}
		}
		if target == "" {
			r.Screen.Warn("容器 %s 的 %s 在新服务器上不是匿名卷了，那里的数据没有迁入", it.m.Name, m.Destination)
			continue
		}
		r.Screen.Detail("匿名卷：%s 的 %s 沿用迁移过来的数据", it.m.Name, m.Destination)
		rc := bundle.OpenStream(r.dataDir, "volumes/"+v.Name, v.Parts)
		err := r.vio.replace(ctx, target, rc)
		rc.Close()
		if err != nil {
			return fmt.Errorf("容器 %s 的匿名卷（%s）数据写入失败：%v", it.m.Name, m.Destination, err)
		}
	}
	return nil
}

// createProject 用 Compose 创建项目的容器（不启动），放回匿名卷的数据和容器里的改动。
func (r *run) createProject(ctx context.Context, u *unit) error {
	if r.tool == nil {
		return errors.New("新服务器没有安装 docker compose")
	}
	p := u.project
	members := u.items
	// -f 按旧服务器上的原样写法传（在项目目录里运行），容器标签里记的配置文件才和原来一样。
	files := p.ConfigArgs
	if len(files) == 0 {
		files = p.ConfigFiles
	}
	cp := compose.Project{Name: p.Name, WorkingDir: p.WorkingDir, Files: append([]string(nil), files...)}
	// 旧版本镜像、改名的 digest 镜像、删掉了文件的镜像：用一个临时的覆盖文件按服务指定镜像，
	// 不改用户自己的文件。
	override := map[string]map[string]string{}
	for _, svc := range uniqueServices(members) {
		reps := replicas(members, svc)
		img, err := r.serviceImage(ctx, p.Name, svc, reps)
		if err != nil {
			return err
		}
		if img != reps[0].c.Config.Image {
			override[svc] = map[string]string{"image": img}
		}
	}
	if len(override) > 0 {
		path := filepath.Join(r.workdir, "dockhop-override-"+p.Name+".yml")
		b, _ := json.Marshal(map[string]any{"services": override}) // JSON 也是合法的 YAML
		if err := os.WriteFile(path, b, 0o600); err != nil {
			return err
		}
		cp.Files = append(cp.Files, path)
	}
	nets, _ := r.Docker.Networks(ctx)
	var before []string
	for _, n := range nets {
		if full, err := r.Docker.InspectNetwork(ctx, n.ID); err == nil && full.Labels["com.docker.compose.project"] == p.Name {
			before = append(before, full.Name)
		}
	}
	if err := r.j.add(Entry{Op: opComposeUp, Name: p.Name, Names: before}); err != nil {
		return err
	}
	// 一个服务可能有多个副本（deploy.replicas、--scale）：按旧服务器上的个数创建，
	// 再按副本编号一一对应，每个副本的匿名卷和容器里的改动放进对应的新容器。
	// 只在个数和配置文件里写的不一样时才加 --scale：有的 Compose 版本（例如 2.17）会把它
	// 算进记在容器上的配置摘要，之后用户自己 docker compose up 时就会重建容器。
	services := uniqueServices(members)
	args := append([]string{"up", "--no-start"}, r.tool.PullMissing(ctx)...)
	configured, err := r.tool.Services(ctx, cp)
	if err != nil {
		r.Log.Printf("读不出 Compose 项目 %s 配置里的副本个数：%v", p.Name, err)
	}
	for _, svc := range services {
		want, have := len(replicas(members, svc)), 1
		if c, ok := configured[svc]; ok {
			have = c.Replicas()
		}
		if want != have {
			args = append(args, "--scale", fmt.Sprintf("%s=%d", svc, want))
		}
	}
	if err := r.tool.Exec(ctx, cp, r.Log.Printf, append(args, services...)...); err != nil {
		return err
	}
	list, err := r.Docker.ContainersByLabel(ctx, "com.docker.compose.project="+p.Name)
	if err != nil {
		return err
	}
	for _, svc := range services {
		old := replicas(members, svc)
		var created []docker.ContainerSummary
		for _, c := range list {
			if c.Labels["com.docker.compose.service"] == svc {
				created = append(created, c)
			}
		}
		sort.SliceStable(created, func(i, j int) bool {
			return replicaNumber(created[i].Labels) < replicaNumber(created[j].Labels)
		})
		if len(created) != len(old) {
			return fmt.Errorf("服务 %s 在旧服务器上有 %d 个容器，新服务器上创建出了 %d 个", svc, len(old), len(created))
		}
		for i, it := range old {
			it.newID = created[i].ID
		}
	}
	for _, it := range members {
		if err := r.transplantAnonymous(ctx, it, it.newID); err != nil {
			return err
		}
		if err := r.applyChanges(ctx, it); err != nil {
			return err
		}
	}
	u.cp, u.services, u.configured = cp, services, configured
	return nil
}

// replicas 是服务 svc 在旧服务器上的容器，按副本编号排好。
func replicas(items []*citem, svc string) []*citem {
	var out []*citem
	for _, it := range items {
		if it.m.Service == svc {
			out = append(out, it)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return replicaNumber(out[i].c.Config.Labels) < replicaNumber(out[j].c.Config.Labels)
	})
	return out
}

// replicaNumber 是 Compose 给容器记的副本编号（com.docker.compose.container-number）。
func replicaNumber(labels map[string]string) int {
	n, _ := strconv.Atoi(labels["com.docker.compose.container-number"])
	return n
}

func uniqueServices(items []*citem) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.m.Service)
	}
	return uniq(out)
}

func uniq(list []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range list {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// ensureNetwork 按迁移记录创建网络；新服务器上已有同名、核心设置相同的网络时沿用。
func (r *run) ensureNetwork(ctx context.Context, name string, raw json.RawMessage) error {
	var n struct {
		Name       string            `json:"Name"`
		Driver     string            `json:"Driver"`
		Internal   bool              `json:"Internal"`
		Attachable bool              `json:"Attachable"`
		EnableIPv6 bool              `json:"EnableIPv6"`
		EnableIPv4 *bool             `json:"EnableIPv4"`
		Options    map[string]string `json:"Options"`
		Labels     map[string]string `json:"Labels"`
		IPAM       struct {
			Driver  string              `json:"Driver"`
			Options map[string]string   `json:"Options"`
			Config  []docker.IPAMConfig `json:"Config"`
		} `json:"IPAM"`
	}
	if err := json.Unmarshal(raw, &n); err != nil {
		return fmt.Errorf("网络 %s 的配置读不懂：%v", name, err)
	}
	if cur, err := r.Docker.InspectNetwork(ctx, name); err == nil {
		if r.d.ReuseExisting(name) {
			r.Screen.Detail("按检查时的选择沿用新服务器上已有的网络 %s", name)
			return nil
		}
		if driverName(cur.Driver) != driverName(n.Driver) || !sameSubnets(cur.IPAM.Config, n.IPAM.Config) && !r.d.AutoSubnetFor(name) {
			return fmt.Errorf("新服务器上已有同名网络 %s，但设置和旧服务器不同；为了不影响用它的其他容器，没有改动它", name)
		}
		if !maps.Equal(cur.Labels, n.Labels) {
			r.Screen.Warn("网络 %s 已存在，只是标签和旧服务器不同，直接沿用，不做修改", name)
		}
		return nil
	} else if !docker.IsNotFound(err) {
		return err
	}
	ipam := map[string]any{"Driver": n.IPAM.Driver, "Options": n.IPAM.Options}
	if !r.d.AutoSubnetFor(name) {
		ipam["Config"] = n.IPAM.Config
	}
	body := map[string]any{
		"Name": name, "Driver": n.Driver, "Internal": n.Internal, "Attachable": n.Attachable,
		"EnableIPv6": n.EnableIPv6, "Options": n.Options, "Labels": n.Labels, "IPAM": ipam, "CheckDuplicate": true,
	}
	if n.EnableIPv4 != nil {
		body["EnableIPv4"] = *n.EnableIPv4
	}
	if err := r.j.add(Entry{Op: opNetwork, Name: name}); err != nil {
		return err
	}
	if _, err := r.Docker.CreateNetwork(ctx, body); err != nil {
		return fmt.Errorf("网络 %s 创建失败：%v", name, err)
	}
	return nil
}

func driverName(d string) string {
	if d == "" {
		return "bridge"
	}
	return d
}

func sameSubnets(a, b []docker.IPAMConfig) bool {
	sa, sb := subnets(a), subnets(b)
	return strings.Join(sa, ",") == strings.Join(sb, ",")
}

func subnets(cfg []docker.IPAMConfig) []string {
	var out []string
	for _, c := range cfg {
		if c.Subnet != "" {
			out = append(out, c.Subnet)
		}
	}
	sort.Strings(out)
	return out
}
