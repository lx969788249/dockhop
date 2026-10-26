package restore

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/preflight"
	"github.com/lx969788249/dockhop/internal/spec"
)

// citem 是要迁入的一个容器。
type citem struct {
	m       bundle.Container
	raw     json.RawMessage
	c       *docker.Container
	newID   string
	img     string // 创建容器用的镜像（算过之后缓存）
	applied bool   // 容器里的改动已经包含在 img 里
}

func (it *citem) running() bool {
	return it.c.State.Running || it.c.State.Paused || it.c.State.Status == "restarting"
}

// image 是创建这个容器用的镜像：旧版本镜像、改名后的 digest 镜像或原来的镜像名。
// 容器里有删除的文件时，先在临时容器里做出带改动的镜像（见 changedImage）。
func (r *run) image(ctx context.Context, it *citem) (string, error) {
	if it.img != "" {
		return it.img, nil
	}
	img := it.c.Config.Image
	if v := r.imageAs[img]; v != "" {
		img = v
	}
	if it.m.OldImage != "" {
		img = it.m.OldImage
	}
	if ch := it.m.Changes; ch != nil && len(ch.Deleted) > 0 {
		if r.vio.helperBin == "" {
			r.Screen.Warn("容器 %s 里删除过 %d 个文件，Docker 不在这台服务器上，没法在新容器里删除它们（文件还在，来自镜像）", it.m.Name, len(ch.Deleted))
		} else {
			built, err := r.changedImage(ctx, it, img)
			if err != nil {
				return "", fmt.Errorf("容器 %s 里的改动没能放进镜像：%v", it.m.Name, err)
			}
			img, it.applied = built, true
		}
	}
	it.img = img
	return img, nil
}

// changedImage 用临时容器做出带改动的镜像：写入新增和修改的文件、删除删掉的文件，
// 再在这台服务器上 commit（本机 commit 没问题，只是导出 commit 的镜像有问题）。
// 镜像的配置和原镜像相同，容器的配置照常来自原始配置。
func (r *run) changedImage(ctx context.Context, it *citem, base string) (string, error) {
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
		"Image": base, "Entrypoint": append([]string{"/dockhop-helper", "__remove"}, it.m.Changes.Deleted...),
		"HostConfig": map[string]any{"NetworkMode": "none", "Binds": []string{r.vio.helperBin + ":/dockhop-helper:ro"}},
	}
	commitCfg := map[string]any{"Entrypoint": nonNil(bc.Entrypoint), "Cmd": nonNil(bc.Cmd)}
	if bc.User != "" && bc.User != "root" && bc.User != "0" && bc.User != "0:0" {
		tmp["User"] = "0:0"
		commitCfg["User"] = bc.User
	}
	// 这个临时容器不能带 dockhop.helper 标签：commit 时标签会进到镜像里，再跟着镜像到
	// 迁入的容器上。所以记进事务日志，程序被强制结束后由回滚或提交后的清理删除。
	name := fmt.Sprintf("dockhop-helper-%s-%d", runID, r.vio.n.Add(1))
	if err := r.j.add(Entry{Op: opHelper, Name: name}); err != nil {
		return "", err
	}
	id, err := dc.CreateContainer(ctx, name, tmp)
	if err != nil {
		return "", err
	}
	defer dc.RemoveContainerAndVolumes(context.WithoutCancel(ctx), id)
	if err := r.putChanges(ctx, it, id); err != nil {
		return "", err
	}
	if err := dc.StartContainer(ctx, id); err != nil {
		return "", err
	}
	if code, err := dc.WaitContainer(ctx, id); err != nil || code != 0 {
		return "", fmt.Errorf("删除文件失败（退出码 %d）：%v %s", code, err, dc.ContainerLogs(ctx, id, 5))
	}
	tag := it.m.Name
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

// putChanges 把容器里新增和修改的文件写进 id 这个（没启动的）容器。
func (r *run) putChanges(ctx context.Context, it *citem, id string) error {
	ch := it.m.Changes
	rc := bundle.OpenStream(r.dataDir, "changes/"+it.m.Name, ch.Parts)
	defer rc.Close()
	if err := r.Docker.PutArchive(ctx, id, "/", rc); err != nil {
		return fmt.Errorf("容器 %s 里的改动没能写入：%v", it.m.Name, err)
	}
	return nil
}

// applyChanges 在容器创建之后、启动之前写入容器里的改动（已经做进镜像的除外）。
func (r *run) applyChanges(ctx context.Context, it *citem) error {
	if it.m.Changes == nil || it.applied || len(it.m.Changes.Paths) == 0 {
		return nil
	}
	return r.putChanges(ctx, it, it.newID)
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

// startAndVerify 启动容器并确认它正常：有健康检查的等到健康，没有的确认它
// 启动后没有马上退出。原来是暂停状态的，最后再暂停。
func (r *run) startAndVerify(ctx context.Context, it *citem, id string) error {
	if err := r.Docker.StartContainer(ctx, id); err != nil {
		return fmt.Errorf("容器 %s 启动失败：%v", it.m.Name, err)
	}
	if err := r.waitReady(ctx, it.m.Name, id); err != nil {
		return err
	}
	if it.c.State.Paused {
		if err := r.Docker.PauseContainer(ctx, id); err != nil {
			return fmt.Errorf("没能把容器 %s 恢复成暂停状态：%v", it.m.Name, err)
		}
	}
	return nil
}

// waitReady 等容器就绪。
func (r *run) waitReady(ctx context.Context, name, id string) error {
	c, _, err := r.Docker.InspectContainer(ctx, id)
	if err != nil {
		return err
	}
	hc := c.Config.Healthcheck
	hasCheck := hc != nil && len(hc.Test) > 0 && hc.Test[0] != "NONE"
	started := time.Now()
	if !hasCheck {
		grace := r.startupGrace()
		for {
			c, _, err := r.Docker.InspectContainer(ctx, id)
			if err != nil {
				return err
			}
			if c.State.Status != "running" {
				return fmt.Errorf("容器 %s 启动后马上退出了（%s）%s", name, c.State.Status, r.logsHint(ctx, id))
			}
			if time.Since(started) >= grace {
				return nil
			}
			if err := sleep(ctx, time.Second); err != nil {
				return err
			}
		}
	}
	timeout := healthTimeout(r.opts.HealthTimeout, hc.StartPeriod, hc.Interval, hc.Timeout)
	for {
		c, _, err := r.Docker.InspectContainer(ctx, id)
		if err != nil {
			return err
		}
		status := c.State.Status
		if c.State.Health != nil && status == "running" {
			status = c.State.Health.Status
		}
		switch status {
		case "healthy":
			r.Log.Printf("容器 %s 健康检查通过（%s）", name, time.Since(started).Round(time.Second))
			return nil
		case "unhealthy", "exited", "dead":
			return fmt.Errorf("容器 %s 健康检查没有通过（%s）%s", name, status, r.logsHint(ctx, id))
		}
		elapsed := time.Since(started)
		if elapsed >= timeout {
			return fmt.Errorf("容器 %s 健康检查超时（等了 %d 秒）%s", name, int(timeout.Seconds()), r.logsHint(ctx, id))
		}
		r.Screen.Note(fmt.Sprintf("%s 健康检查 · %s · %d/%d 秒", name, status, int(elapsed.Seconds()), int(timeout.Seconds())))
		if err := sleep(ctx, time.Second); err != nil {
			return err
		}
	}
}

func (r *run) logsHint(ctx context.Context, id string) string {
	if logs := r.Docker.ContainerLogs(ctx, id, 5); logs != "" {
		r.Log.Printf("容器最后的输出：\n%s", logs)
		return "，最后的输出：" + lastLine(logs)
	}
	return ""
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	l := strings.TrimSpace(lines[len(lines)-1])
	if len([]rune(l)) > 160 {
		l = string([]rune(l)[:160]) + "…"
	}
	return l
}

// healthTimeout 是健康检查最长等多久：至少 base，并且够容器自己的健康检查跑完第一次。
// 有的镜像每 60 秒才检查一次（例如 Vaultwarden），固定等 60 秒会在第一次检查出结果之前就超时。
func healthTimeout(base time.Duration, startPeriod, interval, timeout int64) time.Duration {
	if interval <= 0 {
		interval = int64(30 * time.Second)
	}
	if timeout <= 0 {
		timeout = int64(30 * time.Second)
	}
	need := time.Duration(startPeriod+interval+timeout) + 30*time.Second
	if need < base {
		need = base
	}
	return min(need, time.Hour)
}

func (r *run) startupGrace() time.Duration {
	if v := r.Getenv("DOCKHOP_TEST_STARTUP_GRACE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return 3 * time.Second
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// dependencyOrder 按依赖排序：被借用网络、--link、--volumes-from 的容器先创建。
// 循环依赖时按原来的顺序。
func dependencyOrder(items []*citem, nameOf func(string) string) []*citem {
	in := map[string]*citem{}
	for _, it := range items {
		in[it.m.Name] = it
	}
	done := map[string]bool{}
	var out []*citem
	pending := append([]*citem(nil), items...)
	for len(pending) > 0 {
		var next []*citem
		progressed := false
		for _, it := range pending {
			blocked := false
			for _, ref := range spec.Dependencies(it.c) {
				dep := ref
				if n := nameOf(ref); n != "" {
					dep = n
				}
				if dep != it.m.Name && in[dep] != nil && !done[dep] {
					blocked = true
				}
			}
			if blocked {
				next = append(next, it)
				continue
			}
			out = append(out, it)
			done[it.m.Name] = true
			progressed = true
		}
		if !progressed {
			return append(out, next...)
		}
		pending = next
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
		if !sameLabels(cur.Labels, n.Labels) {
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

func sameLabels(a, b map[string]string) bool {
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
