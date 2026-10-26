package restore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/spec"
	"github.com/lx969788249/dockhop/internal/ui"
)

// undo 按事务日志撤销迁入：删除这次迁入的内容，把被替换的原有服务放回去。迁入失败、被中断时在同一个进程里
// 执行；程序被强制结束时，下次运行再执行。
type undo struct {
	dc     *docker.Client
	screen *ui.Screen
	logf   func(string, ...any)
	vio    *volumeIO
	fail   func(point string) bool // 测试用：在指定的位置模拟失败
	// 回滚重建旧容器时用的默认值（就是这台服务器自己的）。
	defaults spec.SourceDefaults
}

// rollback 返回没能撤销的地方；为空表示已经全部撤销。
//
// 顺序和改动的顺序相反，分三遍：
//  1. 先停止并删除这次新建的容器。没能全部删掉时停下：它们可能还在写数据，这时放回
//     的数据会被覆盖，回滚资料全部保留，交给人工处理。
//  2. 再处理数据：删除这次写入的网络、Compose 文件、目录、数据卷，放回原有的。
//  3. 最后放回旧服务：重建旧 Compose 容器、旧容器改回原名、启动临时停下的容器。
//     原有的数据没能全部放回时，旧服务保持停止，以免在不完整的数据上运行。
func (u *undo) rollback(ctx context.Context, entries []Entry) []string {
	var problems []string
	note := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		problems = append(problems, msg)
		u.logf("回滚：%s", msg)
	}
	oldIDs := map[string]bool{}
	for _, e := range entries {
		if e.ID != "" && e.Op != opHelper {
			oldIDs[e.ID] = true
		}
		for _, o := range e.Old {
			oldIDs[o.ID] = true
		}
	}

	// 第 1 遍：删除新建的容器。
	u.screen.Note("删除这次新建的容器")
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		switch e.Op {
		case opContainer:
			if c, _, err := u.dc.InspectContainer(ctx, e.Name); err == nil && !oldIDs[c.ID] {
				if err := u.dc.RemoveContainerAndVolumes(ctx, c.ID); err != nil && !docker.IsNotFound(err) {
					note("本次新建的容器 %s 没能删除：%v", e.Name, err)
				}
			}
		case opHelper:
			u.removeHelper(ctx, e, note)
		case opComposeUp:
			list, err := u.dc.ContainersByLabel(ctx, "com.docker.compose.project="+e.Name)
			if err != nil {
				note("读不到 Compose 项目 %s 的容器：%v", e.Name, err)
				continue
			}
			for _, c := range list {
				if oldIDs[c.ID] {
					continue
				}
				if err := u.dc.RemoveContainerAndVolumes(ctx, c.ID); err != nil && !docker.IsNotFound(err) {
					note("Compose 项目 %s 的新容器 %s 没能删除：%v", e.Name, c.Name(), err)
				}
			}
		}
	}
	if len(problems) > 0 || u.fail("rollback-remove") {
		if u.fail("rollback-remove") {
			note("（测试）模拟新容器没能删除")
		}
		note("新建的容器没能全部删除，为了安全没有动数据，回滚资料全部保留")
		return problems
	}

	// 第 2 遍：数据。
	u.screen.Note("删除这次写入的网络、文件、目录和数据卷，放回原有的")
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		switch e.Op {
		case opComposeUp:
			u.removeComposeNetworks(ctx, e, note)
		case opNetwork:
			if err := u.dc.RemoveNetwork(ctx, e.Name); err != nil && !docker.IsNotFound(err) {
				note("本次新建的网络 %s 没能删除：%v", e.Name, err)
			}
		case opFile:
			if e.Existed {
				if _, err := os.Lstat(e.Backup); err != nil {
					continue // 备份已经放回去了（之前撤销过），或者还没来得及挪走原来的文件
				}
			}
			if err := os.RemoveAll(e.Path); err != nil {
				note("没能删除 %s：%v", e.Path, err)
				continue
			}
			if e.Existed {
				if err := moveOrCopy(e.Backup, e.Path); err != nil {
					note("文件 %s 没能放回原来的：%v", e.Path, err)
				}
			}
		case opMkdir:
			_ = os.Remove(e.Path) // 只删空目录；里面还有别的东西就留着
		case opLink:
			if st, err := os.Lstat(e.Path); err == nil && st.Mode()&os.ModeSymlink != 0 {
				if err := os.Remove(e.Path); err != nil {
					note("本次新建的软链接 %s 没能删除：%v", e.Path, err)
				}
			}
		case opBind:
			u.undoBind(e, note)
		case opVolume:
			u.undoVolume(ctx, e, note)
		}
	}
	if u.fail("rollback-data") {
		note("（测试）模拟数据没能放回")
	}
	dataOK := len(problems) == 0
	if !dataOK {
		note("原有的数据没能全部放回，原来的服务保持停止，请按回滚资料人工处理")
	}

	// 第 3 遍：旧服务。
	u.screen.Note("放回新服务器上原来的服务")
	images := map[string]string{}
	for _, e := range entries {
		if e.Op == opOldImages {
			for k, v := range e.Images {
				images[k] = v
			}
		}
	}
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		switch e.Op {
		case opComposeOld:
			for _, o := range e.Old {
				u.undoOldCompose(ctx, o, images[o.ID], dataOK, note)
			}
		case opRename:
			u.undoRename(ctx, e, note)
		case opStop:
			if dataOK {
				if err := restoreState(ctx, u.dc, e.ID, e.State); err != nil {
					note("容器 %s 没能恢复原来的状态（%s）：%v", e.Name, e.State, err)
				}
			}
		}
	}
	return problems
}

// removeHelper 删除 DockHop 自己的临时容器（按日志里记下的名字，名字里带着进程号）。
func (u *undo) removeHelper(ctx context.Context, e Entry, note func(string, ...any)) {
	if err := u.dc.RemoveContainerAndVolumes(ctx, e.Name); err != nil && !docker.IsNotFound(err) {
		note("临时容器 %s 没能删除：%v", e.Name, err)
	}
}

func (u *undo) removeComposeNetworks(ctx context.Context, e Entry, note func(string, ...any)) {
	nets, err := u.dc.Networks(ctx)
	if err != nil {
		return
	}
	before := map[string]bool{}
	for _, n := range e.Names {
		before[n] = true
	}
	for _, n := range nets {
		full, err := u.dc.InspectNetwork(ctx, n.ID)
		if err != nil || full.Labels["com.docker.compose.project"] != e.Name || before[full.Name] {
			continue
		}
		if err := u.dc.RemoveNetwork(ctx, full.ID); err != nil && !docker.IsNotFound(err) {
			note("Compose 项目 %s 新建的网络 %s 没能删除：%v", e.Name, full.Name, err)
		}
	}
}

func (u *undo) undoBind(e Entry, note func(string, ...any)) {
	if e.Path == "" || e.Path == "/" {
		return
	}
	_, curErr := os.Lstat(e.Path)
	if !e.Existed {
		if curErr == nil {
			if err := os.RemoveAll(e.Path); err != nil {
				note("本次新建的目录 %s 没能删除：%v", e.Path, err)
			}
		}
		return
	}
	if _, err := os.Lstat(e.Backup); err != nil {
		if curErr == nil {
			return // 记下了但还没挪开原来的目录：现在的就是原来的
		}
		note("目录 %s 和它的备份都找不到了", e.Path)
		return
	}
	if curErr == nil {
		if err := os.RemoveAll(e.Path); err != nil {
			note("没能删除迁入的目录 %s：%v", e.Path, err)
			return
		}
	}
	if err := os.Rename(e.Backup, e.Path); err != nil {
		note("目录 %s 没能放回原来的：%v", e.Path, err)
	}
}

func (u *undo) undoVolume(ctx context.Context, e Entry, note func(string, ...any)) {
	_, err := u.dc.InspectVolume(ctx, e.Name)
	exists := err == nil
	if !e.Existed {
		if exists {
			if err := u.dc.RemoveVolume(ctx, e.Name); err != nil && !docker.IsNotFound(err) {
				note("本次新建的数据卷 %s 没能删除：%v", e.Name, err)
			}
		}
		return
	}
	if !exists {
		note("数据卷 %s 不见了，没法放回原来的数据", e.Name)
		return
	}
	f, err := os.Open(e.Backup)
	if err != nil {
		note("数据卷 %s 的备份读不到：%v", e.Name, err)
		return
	}
	defer f.Close()
	if err := bundle.ScanArchive(f, "dockhop-data"); err != nil {
		note("数据卷 %s 的备份有问题：%v", e.Name, err)
		return
	}
	_, _ = f.Seek(0, 0)
	if err := u.vio.replace(ctx, e.Name, f); err != nil {
		note("数据卷 %s 没能放回原来的数据：%v", e.Name, err)
	}
}

// undoRename 把改名备份的旧容器改回原名，重新连上原来的网络。
func (u *undo) undoRename(ctx context.Context, e Entry, note func(string, ...any)) {
	if _, _, err := u.dc.InspectContainer(ctx, e.Backup); err != nil {
		if c, _, err := u.dc.InspectContainer(ctx, e.Name); err == nil && c.ID == e.ID {
			return // 记下了但还没改名
		}
		note("旧容器 %s 和它的备份都找不到了", e.Name)
		return
	}
	if c, _, err := u.dc.InspectContainer(ctx, e.Name); err == nil && c.ID != e.ID {
		_ = u.dc.RemoveContainerAndVolumes(ctx, c.ID)
	}
	if err := u.dc.RenameContainer(ctx, e.ID, e.Name); err != nil {
		note("旧容器 %s 没能改回原名，它现在叫 %s：%v", e.Name, e.Backup, err)
		return
	}
	reconnect(ctx, u.dc, e.ID, e.Networks, note, e.Name)
}

// reconnect 按记录重新连上网络（包括固定 IP 和别名）。
func reconnect(ctx context.Context, dc *docker.Client, id string, raw json.RawMessage, note func(string, ...any), name string) {
	var nets map[string]map[string]any
	if json.Unmarshal(raw, &nets) != nil {
		return
	}
	cur, _, err := dc.InspectContainer(ctx, id)
	if err != nil {
		return
	}
	for n, ep := range nets {
		if _, ok := cur.NetworkSettings.Networks[n]; ok || n == "host" || n == "none" {
			continue
		}
		endpoint := map[string]any{}
		if ipam, ok := ep["IPAMConfig"].(map[string]any); ok && len(ipam) > 0 {
			endpoint["IPAMConfig"] = ipam
		}
		short := id
		if len(short) > 12 {
			short = short[:12]
		}
		var aliases []string
		if list, ok := ep["Aliases"].([]any); ok {
			for _, a := range list {
				if s, _ := a.(string); s != "" && s != short {
					aliases = append(aliases, s)
				}
			}
		}
		if len(aliases) > 0 {
			endpoint["Aliases"] = aliases
		}
		if err := dc.ConnectNetwork(ctx, n, id, endpoint); err != nil {
			note("旧容器 %s 没能重新连上网络 %s：%v", name, n, err)
		}
	}
}

// undoOldCompose 按记录原样重建新服务器上原有的 Compose 容器。
func (u *undo) undoOldCompose(ctx context.Context, o OldContainer, image string, dataOK bool, note func(string, ...any)) {
	if c, _, err := u.dc.InspectContainer(ctx, o.ID); err == nil {
		// 还没被删除：只需恢复状态。
		if dataOK {
			if err := restoreState(ctx, u.dc, c.ID, o.State); err != nil {
				note("原来的容器 %s 没能恢复状态：%v", o.Name, err)
			}
		}
		return
	}
	if c, _, err := u.dc.InspectContainer(ctx, o.Name); err == nil {
		_ = u.dc.RemoveContainerAndVolumes(ctx, c.ID)
	}
	var src struct {
		Image  string `json:"Image"`
		Config struct {
			Image string `json:"Image"`
		} `json:"Config"`
	}
	_ = json.Unmarshal(o.Raw, &src)
	if image == "" {
		// 没有改动、没存回滚镜像：用原来的镜像。标签还指向同一个镜像时用名字（看起来和原来
		// 一样）；否则按 ID。containerd 存储里容器记的 ID 不一定能直接当镜像用，都不行时用名字。
		image = src.Config.Image
		if img, err := u.dc.InspectImage(ctx, src.Config.Image); err != nil || img.ID != src.Image {
			if _, err := u.dc.InspectImage(ctx, src.Image); err == nil {
				image = src.Image
			}
		}
	}
	built, err := spec.Rebuild{
		Raw: o.Raw, Image: image, Source: u.defaults, KeepVolumes: o.Volumes,
		NameOf:       func(ref string) string { return currentName(ctx, u.dc, ref) },
		MultiNetwork: u.dc.AtLeast("1.44"),
	}.Build()
	if err != nil {
		note("原来的容器 %s 没能重建：%v", o.Name, err)
		return
	}
	id, err := u.dc.CreateContainer(ctx, o.Name, built.Body)
	if err != nil {
		note("原来的容器 %s 没能重建：%v", o.Name, err)
		return
	}
	for _, c := range built.Connects {
		if err := u.dc.ConnectNetwork(ctx, c.Network, id, c.Endpoint); err != nil {
			note("原来的容器 %s 没能连上网络 %s：%v", o.Name, c.Network, err)
		}
	}
	if dataOK {
		if err := restoreState(ctx, u.dc, id, o.State); err != nil {
			note("原来的容器 %s 没能恢复状态：%v", o.Name, err)
		}
	}
}

func currentName(ctx context.Context, dc *docker.Client, ref string) string {
	c, _, err := dc.InspectContainer(ctx, ref)
	if err != nil {
		return ""
	}
	return c.ShortName()
}

// restoreState 把容器恢复成原来的状态：运行或暂停；原来停着的不动。
func restoreState(ctx context.Context, dc *docker.Client, id, state string) error {
	switch state {
	case "running", "restarting", "paused":
	default:
		return nil
	}
	c, _, err := dc.InspectContainer(ctx, id)
	if err != nil {
		return err
	}
	if !c.State.Running {
		if err := dc.StartContainer(ctx, id); err != nil {
			return err
		}
	}
	if state == "paused" && !c.State.Paused {
		return dc.PauseContainer(ctx, id)
	}
	return nil
}

// cleanupCommitted 提交之后删除回滚资料：改名备份的旧容器、回滚镜像、旧数据。
// 返回没能删除的东西。
func (u *undo) cleanupCommitted(ctx context.Context, entries []Entry) []string {
	var left []string
	if u.fail("commit-cleanup") {
		left = append(left, "（测试）模拟回滚资料没能删除")
	}
	for _, e := range entries {
		switch e.Op {
		case opHelper:
			u.removeHelper(ctx, e, func(f string, a ...any) { left = append(left, fmt.Sprintf(f, a...)) })
		case opRename:
			if err := u.dc.RemoveContainer(ctx, e.ID, true); err != nil && !docker.IsNotFound(err) {
				left = append(left, fmt.Sprintf("改名备份的旧容器 %s：%v", e.Backup, err))
			}
		case opOldImages:
			for _, img := range e.Images {
				if err := u.dc.RemoveImage(ctx, img); err != nil && !docker.IsNotFound(err) {
					left = append(left, fmt.Sprintf("回滚镜像 %s：%v", img, err))
				}
			}
		case opBind:
			if e.Existed {
				if err := os.RemoveAll(e.Backup); err != nil {
					left = append(left, fmt.Sprintf("旧目录 %s：%v", e.Backup, err))
				}
			}
		}
	}
	return left
}
