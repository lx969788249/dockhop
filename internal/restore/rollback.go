package restore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

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
	record func(Entry) error       // 把回滚自己做的改动追加到事务日志（见 opRebuilt）
	// 回滚重建旧容器时用的默认值（就是这台服务器自己的）。
	defaults spec.SourceDefaults
}

// rollback 返回没能撤销的地方；为空表示已经全部撤销。
//
// 顺序和改动的顺序相反，分三遍：
//  1. 先停下所有可能在写数据的容器：删除这次新建的容器，再把临时停过、又在运行的已有容器
//     重新停下（提交之后才出错或被中断，或者上次回滚已经放回、重建了原来的服务时会这样）。
//     没能做到时停下：它们可能还在写数据，这时放回的数据会被覆盖，回滚资料全部保留，交给
//     人工处理。
//  2. 再处理数据：删除这次写入的网络、Compose 文件、目录、数据卷，放回原有的。
//  3. 最后放回旧服务：旧容器改回原名、重建旧 Compose 容器，再按依赖统一启动原来在运行的
//     （包括临时停下的）。原有的数据没能全部放回时，旧服务保持停止，以免在不完整的数据上运行。
func (u *undo) rollback(ctx context.Context, entries []Entry) []string {
	var problems []string
	note := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		problems = append(problems, msg)
		u.logf("回滚：%s", msg)
	}
	// 原有的容器，包括上次回滚已经重建的原有 Compose 容器（opRebuilt）：都不能当成这次新建的删掉。
	oldIDs := map[string]bool{}
	rebuilt := map[string]string{} // 上次回滚重建过的原有 Compose 容器：名字 → 重建后的 ID
	for _, e := range entries {
		if e.ID != "" && e.Op != opHelper {
			oldIDs[e.ID] = true
		}
		for _, o := range e.Old {
			oldIDs[o.ID] = true
		}
		if e.Op == opRebuilt {
			rebuilt[e.Name] = e.ID
		}
	}

	// 第 1 遍：删除新建的容器，停下临时停过、又被启动了的容器。
	u.screen.Note("删除这次新建的容器")
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		switch e.Op {
		case opContainer:
			// 只有 Docker 明确说没有这个容器，才算已经删掉；其他错误说明不知道它还在不在写数据。
			c, _, err := u.dc.InspectContainer(ctx, e.Name)
			switch {
			case docker.IsNotFound(err):
			case err != nil:
				note("没法确认本次新建的容器 %s 已经删除：%v", e.Name, err)
			case !oldIDs[c.ID]:
				if err := u.dc.RemoveContainerAndVolumes(ctx, c.ID); err != nil && !docker.IsNotFound(err) {
					note("本次新建的容器 %s 没能删除：%v", e.Name, err)
				}
			}
		case opHelper:
			u.removeHelper(ctx, e, note)
		case opStop:
			u.restop(ctx, e, note)
		case opComposeOld:
			// 原来的 Compose 容器：上次回滚可能已经把它放回去（ID 不变）或者重建了（ID 变了，
			// 只能按名字找），现在可能在运行。
			for _, o := range e.Old {
				ref := o.ID
				if _, _, err := u.dc.InspectContainer(ctx, ref); docker.IsNotFound(err) {
					ref = o.Name
				}
				u.restop(ctx, Entry{Name: o.Name, ID: ref}, note)
			}
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
		note("有容器没能删除或停下，为了安全没有动数据，回滚资料全部保留")
		return problems
	}

	// 第 2 遍：数据。
	u.screen.Note("删除这次写入的网络、文件、目录和数据卷，放回原有的")
	var tags []Entry // 改动过的镜像名，最后一起改回去（见 undoTags）
	var had []string // 加载镜像之前已有的镜像
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
		case opTag:
			tags = append(tags, e)
		case opImages:
			had = e.Names
		}
	}
	u.undoTags(ctx, tags, had, note)
	if u.fail("rollback-data") {
		note("（测试）模拟数据没能放回")
	}
	dataOK := len(problems) == 0
	if !dataOK {
		note("原有的数据没能全部放回，原来的服务保持停止，请按回滚资料人工处理")
	}

	// 第 3 遍：旧服务。先全部放回去：旧容器改回原名，再重建原来的 Compose 容器（改回原名之后，
	// 按名字引用它们的才找得到）；最后按依赖统一启动原来在运行的，被借用网络等的先启动。依赖可能
	// 跨项目、跨独立容器（例如一个项目借用另一个项目的容器的网络），按日志一条一条地放回、启动，
	// 会把用到别人的排在前面，它就启动不了。
	u.screen.Note("放回新服务器上原来的服务")
	images := map[string]string{}
	oldNames := map[string]string{} // 原来的 Compose 容器：ID → 名字
	names := map[string]string{}    // 要放回、要启动的全部容器：ID → 名字
	var olds []OldContainer
	for _, e := range entries {
		for k, v := range e.Images {
			images[k] = v
		}
		for _, o := range e.Old {
			oldNames[o.ID], names[o.ID] = o.Name, o.Name
			olds = append(olds, o)
		}
		if e.Op == opStop || e.Op == opRename {
			names[e.ID] = e.Name
		}
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Op == opRename {
			u.undoRename(ctx, entries[i], note)
		}
	}
	type revive struct {
		name, id, state string
		deps            []string
	}
	var revives []revive
	depsOf := func(c *docker.Container) []string { return containerDeps(c, nameIn(names)) }
	conf := map[string]*docker.Container{}
	for _, o := range olds {
		conf[o.Name] = &docker.Container{}
		_ = json.Unmarshal(o.Raw, conf[o.Name])
	}
	for _, o := range dependencyOrder(olds, func(o OldContainer) string { return o.Name },
		func(o OldContainer) []string { return depsOf(conf[o.Name]) }) {
		if id := u.undoOldCompose(ctx, o, rebuilt[o.Name], images[o.ID], nameIn(oldNames), note); id != "" {
			revives = append(revives, revive{o.Name, id, o.State, depsOf(conf[o.Name])})
		}
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if e := entries[i]; e.Op == opStop {
			var deps []string
			if c, _, err := u.dc.InspectContainer(ctx, e.ID); err == nil {
				deps = depsOf(c)
			}
			revives = append(revives, revive{e.Name, e.ID, e.State, deps})
		}
	}
	if !dataOK {
		return problems
	}
	for _, v := range dependencyOrder(revives, func(v revive) string { return v.name }, func(v revive) []string { return v.deps }) {
		if err := restoreState(ctx, u.dc, v.id, v.state); err != nil {
			note("原来的容器 %s 没能恢复原来的状态（%s）：%v", v.name, v.state, err)
		}
	}
	return problems
}

// nameIn 返回按 ids（ID → 名字）把引用的 ID（或它的前缀）换成名字的函数：原来的 Compose 容器
// 删除、重建之后 ID 就变了，它们之间按 ID 的引用（借用网络的 container:<ID> 等）要改成按名字。
func nameIn(ids map[string]string) func(string) string {
	return func(ref string) string {
		for id, name := range ids {
			if id == ref || len(ref) >= 12 && strings.HasPrefix(id, ref) {
				return name
			}
		}
		return ""
	}
}

// undoTags 把镜像名改回迁入前的样子（tags 是日志里改动过的镜像名，按撤销的顺序）：原来指向别的
// 镜像的改回去，原来没有的删掉；再删掉这次迁入带来的、已经没有名字的镜像。加载镜像之前这台服务器
// 上就有的镜像（had）一个不删。
//
// 删掉镜像最后一个名字时，Docker 会连镜像一起删（有容器在用时不让删），所以：
//  1. 先把原来指向别的镜像的名字全部改回去。要是先删原来没有的名字，它指向的镜像可能被一起删掉，
//     而后面要改回去的名字原来正指向它（迁入时一个名字挪走、另一个新名字加到了它上面）；
//  2. 再删原来没有的名字。它是原有的镜像唯一的名字时不删（那个镜像原来没有名字，迁入时用上了它、
//     加了这个名字）：删了名字，镜像就没了；
//  3. 最后看名字改回去之前指向的镜像，没有名字了、又不是原有的，删掉（见 removeImages）。
func (u *undo) undoTags(ctx context.Context, tags []Entry, had []string, note func(string, ...any)) {
	var brought []string // 名字改回去之前指向的镜像
	for _, e := range tags {
		if e.ID == "" {
			continue
		}
		cur, err := u.dc.InspectImage(ctx, e.Name)
		switch {
		case docker.IsNotFound(err):
		case err != nil:
			note("读不到镜像 %s：%v", e.Name, err)
			continue
		case cur.ID == e.ID:
			continue // 还指向原来的镜像（没来得及改，或者之前撤销过）
		}
		repo, tag := splitTag(e.Name)
		if err := u.dc.TagImage(ctx, e.ID, repo, tag); err != nil {
			note("镜像名 %s 没能改回原来的镜像：%v", e.Name, err)
			continue
		}
		if cur != nil {
			brought = append(brought, cur.ID)
		}
	}
	for _, e := range tags {
		if e.ID != "" {
			continue
		}
		cur, err := u.dc.InspectImage(ctx, e.Name)
		switch {
		case docker.IsNotFound(err):
			continue // 已经删掉了（之前撤销过）
		case err != nil:
			note("读不到镜像 %s：%v", e.Name, err)
			continue
		case slices.Contains(had, cur.ID) && len(cur.RepoTags) <= 1:
			u.logf("回滚：镜像名 %s 是原有的镜像 %s 唯一的名字，删掉名字 Docker 会连镜像一起删，保留", e.Name, cur.ID)
			continue
		}
		if err := u.dc.RemoveImage(ctx, e.Name); err != nil && !docker.IsNotFound(err) {
			u.logf("回滚：镜像名 %s 有容器在用，保留：%v", e.Name, err)
		}
	}
	u.removeImages(ctx, brought, had)
}

// removeImages 删掉这次迁入带来的镜像：镜像名都改回去之后，它们原来指向的这些镜像（ids）没有
// 名字、也没有容器在用的，就删掉。加载镜像之前这台服务器上就有的（had）一个不删，哪怕现在没有
// 名字；按 ID 删镜像时 Docker 会连它仅剩的一个名字一起删，所以还有名字的也不删。
func (u *undo) removeImages(ctx context.Context, ids, had []string) {
	for _, id := range ids {
		if slices.Contains(had, id) {
			continue
		}
		if img, err := u.dc.InspectImage(ctx, id); err != nil || len(img.RepoTags) > 0 {
			continue // 已经删掉了（同一个镜像出现了几次），或者还有名字指向它
		}
		if err := u.dc.RemoveImage(ctx, id); err != nil && !docker.IsNotFound(err) {
			u.logf("回滚：镜像 %s 有容器在用，保留：%v", id, err)
		}
	}
}

// restop 把临时停过、现在又在运行的已有容器重新停下（暂停的先恢复再停）。e.ID 可以是
// ID 或名字。
func (u *undo) restop(ctx context.Context, e Entry, note func(string, ...any)) {
	c, _, err := u.dc.InspectContainer(ctx, e.ID)
	switch {
	case docker.IsNotFound(err), err == nil && !c.State.Running:
		return
	case err != nil:
		note("没法确认临时停过的容器 %s 已经停下：%v", e.Name, err)
		return
	}
	if c.State.Paused {
		if err := u.dc.UnpauseContainer(ctx, c.ID); err != nil {
			note("临时停过的容器 %s 又在运行，没能停下：%v", e.Name, err)
			return
		}
	}
	if err := u.dc.StopContainer(ctx, c.ID, nil); err != nil {
		note("临时停过的容器 %s 又在运行，没能停下：%v", e.Name, err)
	}
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
	if errors.Is(err, os.ErrNotExist) {
		return // 已经放回去了（之前撤销过）：放回之后备份就删了，日志里有这一条时备份一定已经存好
	}
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
		return
	}
	// 放回之后删掉备份，和放回去的目录、文件一样：这次回滚做完、还没来得及删除回滚资料就被结束时，
	// 下次再回滚不会把这之后（原来的服务已经重新启动）写进卷里的数据又改回去。
	if err := os.Remove(e.Backup); err != nil {
		u.logf("回滚：数据卷 %s 的备份没能删除：%v", e.Name, err)
	}
}

// undoRename 让改名备份的旧容器回到原来的名字、接回原来的网络（和固定 IP）。可以重复执行，
// 不管上次做到哪一步：备份还在就改回原名；已经叫回原名的（记下了还没来得及改名，或者上次回滚
// 改回原名后被中断）不用改。最后都按记下的网络补上，已经接上的跳过。
func (u *undo) undoRename(ctx context.Context, e Entry, note func(string, ...any)) {
	_, _, err := u.dc.InspectContainer(ctx, e.Backup)
	switch {
	case err == nil:
		if c, _, err := u.dc.InspectContainer(ctx, e.Name); err == nil && c.ID != e.ID {
			_ = u.dc.RemoveContainerAndVolumes(ctx, c.ID)
		}
		if err := u.dc.RenameContainer(ctx, e.ID, e.Name); err != nil {
			note("旧容器 %s 没能改回原名，它现在叫 %s：%v", e.Name, e.Backup, err)
			return
		}
	case !docker.IsNotFound(err):
		note("读不到改名备份的旧容器 %s：%v", e.Backup, err)
		return
	default:
		if c, _, err := u.dc.InspectContainer(ctx, e.Name); err != nil || c.ID != e.ID {
			note("旧容器 %s 和它的备份都找不到了", e.Name)
			return
		}
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

// undoOldCompose 按记录原样重建新服务器上原有的 Compose 容器（不启动，启动见 rollback），
// 返回它的 ID；没能重建时为空。rebuilt 是上次回滚重建它之后的 ID（见 opRebuilt）。nameOf 把配置里
// 引用的旧 ID 换成名字。
func (u *undo) undoOldCompose(ctx context.Context, o OldContainer, rebuilt, image string, nameOf func(string) string, note func(string, ...any)) string {
	// 还没被删除，或者上次回滚已经重建过（之后可能已经运行、写过东西）：只需恢复状态。
	for _, ref := range []string{o.ID, rebuilt} {
		if ref == "" {
			continue
		}
		if c, _, err := u.dc.InspectContainer(ctx, ref); err == nil {
			return c.ID
		}
	}
	// 占着这个名字的是迁入的容器，或者上次回滚重建、没来得及记下的。删掉它不会删掉原来的匿名卷：
	// 重建时按名字挂上它们（见 spec.Rebuild 的 KeepVolumes），Docker 不删按名字挂着的卷。
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
		NameOf:       nameOf,
		MultiNetwork: u.dc.AtLeast("1.44"),
	}.Build()
	if err != nil {
		note("原来的容器 %s 没能重建：%v", o.Name, err)
		return ""
	}
	id, err := u.dc.CreateContainer(ctx, o.Name, built.Body)
	if err != nil {
		note("原来的容器 %s 没能重建：%v", o.Name, err)
		return ""
	}
	connected := true
	for _, c := range built.Connects {
		if err := u.dc.ConnectNetwork(ctx, c.Network, id, c.Endpoint); err != nil {
			note("原来的容器 %s 没能连上网络 %s：%v", o.Name, c.Network, err)
			connected = false
		}
	}
	// 记下重建后的 ID：这次回滚没做完就被结束时，下次再回滚认得它就是原来的服务，不会当成迁入的新
	// 容器删掉。网络没接好的不记，下次删掉重建。没能记下（比如磁盘满了）不算回滚失败：原来的服务
	// 已经回来了；算失败的话回滚资料会留下，下次运行又回滚一遍，反而会把它删掉重建。
	if connected {
		if err := u.record(Entry{Op: opRebuilt, Name: o.Name, ID: id}); err != nil {
			u.logf("回滚：重建的原来的容器 %s 没能记进事务日志：%v", o.Name, err)
		}
	}
	return id
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
