package restore

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/spec"
)

// 改动前的准备（事务里的第一批改动）：临时停下和迁入的数据共用的容器，把要替换的同名服务和
// Compose 项目改名备份，失败时都能放回去。

// prepare 备份要替换的已有服务，临时停下和迁入的数据共用的容器（都是 decide 找出来的）。
func (r *run) prepare(ctx context.Context) error {
	s, dc := r.Screen, r.Docker
	if err := r.recheck(ctx); err != nil {
		return err
	}
	helperBin := ""
	if dc.Local() {
		var err error
		if helperBin, err = prepareHelper(r.Home); err != nil {
			s.Warn("准备清空数据卷用的程序失败：%v", err)
			helperBin = ""
		}
	}
	r.vio = &volumeIO{dc: dc, image: r.helperImage, helperBin: helperBin, lock: lockPath(r.Home)}
	r.undo.vio = r.vio

	stopped := 0
	for _, w := range r.writers {
		name := w.summary.Name()
		e := Entry{Op: opStop, Name: name, ID: w.summary.ID, State: stateOf(w.full), Group: r.groupOf["w:"+name]}
		if err := r.stopContainer(ctx, e); err != nil {
			return err
		}
		r.shared = append(r.shared, e)
		s.Detail("临时停止共用数据的容器：%s", name)
		stopped++
	}

	// 和迁入的容器同名、不随 Compose 项目一起备份的已有容器（见 decide）：停止、改名备份、断开网络
	// （释放固定 IP），失败时改回来。
	backedUp := 0
	for _, it := range r.items {
		if !r.targets[it.m.Name] {
			continue
		}
		r.in("c:" + it.m.Name)
		c, _, err := dc.InspectContainer(ctx, it.m.Name)
		if err != nil {
			return s.StepFailed("读取已有容器 "+it.m.Name+" 失败", err.Error(), "")
		}
		if err := r.stopContainer(ctx, Entry{Op: opStop, Name: c.ShortName(), ID: c.ID, State: stateOf(c)}); err != nil {
			return err
		}
		var nets struct {
			NetworkSettings struct {
				Networks json.RawMessage `json:"Networks"`
			} `json:"NetworkSettings"`
		}
		_, raw, _ := dc.InspectContainer(ctx, c.ID)
		_ = json.Unmarshal(raw, &nets)
		backup := it.m.Name + ".dockhop-backup-" + runID
		if err := r.j.add(Entry{Op: opRename, Name: it.m.Name, ID: c.ID, Backup: backup, Networks: nets.NetworkSettings.Networks}); err != nil {
			return s.StepFailed(err.Error(), "", "")
		}
		if err := dc.RenameContainer(ctx, c.ID, backup); err != nil {
			return s.StepFailed("没能先备份已有的容器 "+it.m.Name+"（改名失败）", err.Error(), "")
		}
		mode := c.HostConfig.NetworkMode
		if mode != "host" && mode != "none" && !strings.HasPrefix(mode, "container:") {
			for n := range c.NetworkSettings.Networks {
				if err := dc.DisconnectNetwork(ctx, n, c.ID); err != nil && !docker.IsNotFound(err) {
					return s.StepFailed("没能释放已有容器 "+it.m.Name+" 占用的网络地址", err.Error(), "")
				}
			}
		}
		backedUp++
	}

	// 已有的同名 Compose 项目：记下每个容器的完整配置，停止，把有改动的存成回滚镜像，再删除。
	for _, p := range r.replaceProjects {
		r.in("p:" + p)
		if err := r.backupProject(ctx, p, r.existingProjects[p]); err != nil {
			return err
		}
		backedUp += len(r.existingProjects[p])
	}
	r.in("")
	if r.testFail("prepare") {
		return s.StepFailed("（测试）模拟备份已有服务时失败", "", "")
	}
	if backedUp > 0 {
		s.Info("已备份 %d 个同名的容器，迁入失败时会放回原来的", backedUp)
	}
	if stopped > 0 {
		s.Info("临时停止了 %d 个和迁入的服务共用数据的容器，迁入完成后自动恢复", stopped)
	}
	return nil
}

func stateOf(c *docker.Container) string {
	if c.State.Paused {
		return "paused"
	}
	return c.State.Status
}

// stopContainer 记下并停止一个容器（暂停的先恢复再停）。
func (r *run) stopContainer(ctx context.Context, e Entry) error {
	if err := r.j.add(e); err != nil {
		return r.Screen.StepFailed(err.Error(), "", "")
	}
	if e.State == "paused" {
		if err := r.Docker.UnpauseContainer(ctx, e.ID); err != nil {
			return r.Screen.StepFailed("容器 "+e.Name+" 没能从暂停中恢复，无法停止", err.Error(), "")
		}
	}
	if err := r.Docker.StopContainer(ctx, e.ID, nil); err != nil {
		return r.Screen.StepFailed("没能停止新服务器上的容器 "+e.Name, err.Error(), "")
	}
	return nil
}

func (r *run) backupProject(ctx context.Context, project string, list []docker.ContainerSummary) error {
	s, dc := r.Screen, r.Docker
	var old []OldContainer
	for _, c := range list {
		full, raw, err := dc.InspectContainer(ctx, c.ID)
		if err != nil {
			return s.StepFailed("读取已有的 Compose 容器 "+c.Name()+" 失败", err.Error(), "")
		}
		o := OldContainer{Name: full.ShortName(), ID: full.ID, State: stateOf(full), Raw: raw, Volumes: map[string]string{}}
		for _, m := range full.Mounts {
			if m.Type == "volume" && spec.IsAnonymousVolume(m.Name) {
				o.Volumes[m.Destination] = m.Name
			}
		}
		old = append(old, o)
	}
	if err := r.j.add(Entry{Op: opComposeOld, Name: project, Old: old}); err != nil {
		return s.StepFailed(err.Error(), "", "")
	}
	s.Detail("备份新服务器上已有的 Compose 项目：%s", project)
	images := map[string]string{}
	for _, o := range old {
		if o.State == "paused" {
			_ = dc.UnpauseContainer(ctx, o.ID)
		}
		if err := dc.StopContainer(ctx, o.ID, nil); err != nil {
			return s.StepFailed("没能停止已有的 Compose 容器 "+o.Name, err.Error(), "")
		}
		// 读不到容器里有没有改动时不能当作没有：接下来要删除这个容器，没备份的改动就找不回来了。
		changes, err := dc.ContainerChanges(ctx, o.ID)
		if err != nil {
			return s.StepFailed("读不到已有的 Compose 容器 "+o.Name+" 里有没有改动，为了安全没有删除它", err.Error(), "")
		}
		full, _, err := dc.InspectContainer(ctx, o.ID)
		if err != nil {
			return s.StepFailed("读取已有的 Compose 容器 "+o.Name+" 失败，为了安全没有删除它", err.Error(), "")
		}
		var mounts []string
		for _, m := range full.Mounts {
			mounts = append(mounts, m.Destination)
		}
		if !spec.WritableChanged(changes, mounts) {
			continue
		}
		tag := strings.ToLower(runID + "-" + o.ID[:12])
		if _, err := dc.Commit(ctx, o.ID, "dockhop-rollback", tag, false); err != nil {
			return s.StepFailed("没能备份已有的 Compose 容器 "+o.Name, err.Error(), "")
		}
		images[o.ID] = "dockhop-rollback:" + tag
	}
	if err := r.j.add(Entry{Op: opOldImages, Name: project, Images: images}); err != nil {
		return s.StepFailed(err.Error(), "", "")
	}
	for _, o := range old {
		// 不删匿名卷：回滚重建旧容器时还要用。
		if err := dc.RemoveContainer(ctx, o.ID, true); err != nil && !docker.IsNotFound(err) {
			return s.StepFailed("没能移开已有的 Compose 容器 "+o.Name, err.Error(), "")
		}
	}
	return nil
}
