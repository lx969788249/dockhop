package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/crypto"
	"github.com/lx969788249/dockhop/internal/host"
	"github.com/lx969788249/dockhop/internal/ui"
)

// 停机打包（屏幕上的第 4、5 步）：分批停下选中的容器，把配置、数据卷、目录、Compose 项目文件和
// 容器里的改动写进加密的迁移包；再把每个镜像单独导出成一个加密文件，新服务器能从镜像仓库按
// 摘要拉到的就不用下载。

// pack 是第 4、5 步：停机打包数据，打包镜像并写完迁移包。返回清单、迁移包的大小和 SHA-256。
func (r *run) pack(ctx context.Context, col *collected, link crypto.Link, path string) (*bundle.Manifest, int64, string, error) {
	s := r.Screen
	s.Begin(4, totalSteps, "停机打包数据")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return nil, 0, "", &ui.Failure{Reason: "无法创建迁移包", Err: err}
	}
	defer f.Close()
	sum := sha256.New()
	w, err := bundle.NewWriter(io.MultiWriter(f, sum), link)
	if err != nil {
		return nil, 0, "", &ui.Failure{Reason: "无法创建迁移包", Err: err}
	}
	m := r.newManifest(ctx)
	if err := addConfigs(w, m, col); err != nil {
		return nil, 0, "", &ui.Failure{Reason: "写入迁移包失败", Err: err}
	}
	if err := r.packData(ctx, w, m, col); err != nil {
		return nil, 0, "", err
	}

	s.Begin(5, totalSteps, "打包镜像")
	if err := r.packImages(ctx, link, m, col); err != nil {
		return nil, 0, "", err
	}
	if err := w.AddJSON("manifest.json", m); err != nil {
		return nil, 0, "", &ui.Failure{Reason: "写入迁移包失败", Err: err}
	}
	if err := w.Close(); err != nil {
		return nil, 0, "", &ui.Failure{Reason: "写入迁移包失败", Err: err, Hint: "执行 df -h 检查磁盘空间"}
	}
	if err := f.Close(); err != nil {
		return nil, 0, "", &ui.Failure{Reason: "写入迁移包失败", Err: err, Hint: "执行 df -h 检查磁盘空间"}
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, 0, "", &ui.Failure{Reason: "写入迁移包失败", Err: err}
	}
	r.summary = append(r.summary, fmt.Sprintf("迁移的服务：%s · 迁移包 %s", r.chosen, ui.HumanBytes(st.Size()+imageFileBytes(m.ImageFiles))))
	return m, st.Size(), hex.EncodeToString(sum.Sum(nil)), nil
}

// newManifest 是迁移包清单的开头：格式版本和旧服务器的环境，其余的打包时填。
func (r *run) newManifest(ctx context.Context) *bundle.Manifest {
	dc := r.Docker
	info, _ := dc.Info(ctx)
	hostname, _ := os.Hostname()
	return &bundle.Manifest{Format: bundle.Format, Version: bundle.Version, CreatedAt: time.Now().UTC(), DockHop: r.Version,
		Source: bundle.Source{Hostname: hostname, Docker: dc.Server().Version, API: dc.APIVersion(), Arch: info.Architecture, Compose: r.composeVersion,
			OS: info.OperatingSystem, CgroupVersion: info.CgroupVersion, LoggingDriver: info.LoggingDriver,
			DefaultRuntime: info.DefaultRuntime, StorageDriver: info.Driver},
		Images: bundle.Images{Refs: map[string]string{}}}
}

// addConfigs 把选中的容器、网络、数据卷的配置写进迁移包。
func addConfigs(w *bundle.Writer, m *bundle.Manifest, col *collected) error {
	for _, it := range col.sel.items {
		if err := w.AddJSON("containers/"+it.name()+".json", it.raw); err != nil {
			return err
		}
	}
	for _, n := range col.networks {
		m.Networks = append(m.Networks, n.name)
		if err := w.AddJSON("networks/"+n.name+".json", n.raw); err != nil {
			return err
		}
	}
	for _, v := range col.volumes {
		if err := w.AddJSON("volumes/"+v.name+".json", v.raw); err != nil {
			return err
		}
	}
	return nil
}

// packData 是第 4 步里停机的那一段：先核对容器还是读的时候的样子（见 recheck），停下要打包的容器，
// 打包容器里的改动、数据卷和宿主机上的数据，再立即恢复停下的容器。
func (r *run) packData(ctx context.Context, w *bundle.Writer, m *bundle.Manifest, col *collected) error {
	if err := r.recheck(ctx, col); err != nil {
		return err
	}
	downStart := time.Now()
	if err := r.stopSources(ctx, col); err != nil {
		return err
	}
	changes, err := r.packContainers(ctx, w, m, col)
	if err != nil {
		return err
	}
	volumes, err := r.packVolumes(ctx, w, m, col)
	if err != nil {
		return err
	}
	host, err := r.packHost(ctx, w, m, col)
	if err != nil {
		return err
	}
	if err := r.resumeSources(ctx, downStart); err != nil {
		return err
	}
	summary := ui.HumanBytes(changes + volumes + host)
	if r.downtime > 0 {
		summary += " · 服务已恢复运行（停了" + downtimeText(r.downtime) + "）"
	}
	r.Screen.End(summary)
	return nil
}

// stopSources 停下要打包的容器和与它们共用数据的容器（col.shared），按依赖分批（见 stopWaves）。
// 先登记再停，程序在任何时候被强制结束，下次运行都能恢复它们。
func (r *run) stopSources(ctx context.Context, col *collected) error {
	s := r.Screen
	var stopList []StoppedContainer
	var waves [][]StoppedContainer
	all := append(append([]*item(nil), col.shared...), col.sel.items...)
	for _, wave := range stopWaves(&inventory{byName: indexByName(all), byID: indexByID(all)}, all) {
		var w []StoppedContainer
		for _, it := range wave {
			if it.c.State.Running {
				w = append(w, StoppedContainer{Name: it.name(), ID: it.c.ID, State: stateName("running", it.c.State.Paused)})
			}
		}
		if len(w) > 0 {
			waves = append(waves, w)
			stopList = append(stopList, w...)
		}
	}
	if len(stopList) == 0 {
		return nil
	}
	if err := writeStopped(r.Home, stopList); err != nil {
		return &ui.Failure{Reason: "无法写入停机记录，为了安全没有停止任何容器", Err: err, Hint: "检查 " + r.Home + " 是否可写"}
	}
	r.stopped, r.toRestore = stopList, len(stopList)
	if err := stopContainers(ctx, r.Docker, waves, s); err != nil {
		return &ui.Failure{Err: err, Hint: "执行 docker ps 检查容器状态"}
	}
	r.testPause(ctx, "after-stop")
	return nil
}

// resumeSources 在数据都已经打包之后立即恢复停下的源容器：后面的镜像导出、压缩和下载都不需要停机。
func (r *run) resumeSources(ctx context.Context, downStart time.Time) error {
	if len(r.stopped) == 0 {
		return nil
	}
	r.downtime = time.Since(downStart)
	var failed []string
	r.Screen.Finish(ctx, func(ctx context.Context) { failed = restoreStates(ctx, r.Docker, r.stopped, r.Screen) })
	if len(failed) > 0 {
		return &ui.Failure{Reason: fmt.Sprintf("有 %d 个源容器没能恢复原来的状态：%s", len(failed), strings.Join(failed, "、")),
			Hint: "立即执行 docker ps -a 检查，并用 docker start 容器名 手动启动"}
	}
	r.restored = len(r.stopped)
	r.stopped = nil
	_ = removeStopped(r.Home)
	return nil
}

// packContainers 把选中的容器记进清单，打包容器里没挂载出来的改动：只打包改动的文件，迁入时写进
// 新建的容器。镜像名已经指向别的版本的，给容器实际用的旧版本临时起个名字，导出它。返回打包的大小。
func (r *run) packContainers(ctx context.Context, w *bundle.Writer, m *bundle.Manifest, col *collected) (int64, error) {
	s, dc := r.Screen, r.Docker
	var packed int64
	for i, it := range col.sel.items {
		s.Count("检查容器里的改动", i+1, len(col.sel.items), it.name())
		moved, keep, deleted, err := layerState(ctx, dc, it)
		if err != nil {
			return 0, &ui.Failure{Reason: "检查容器 " + it.name() + " 的改动失败", Err: err}
		}
		mc := bundle.Container{Name: it.name(), ID: it.c.ID, Image: it.c.Config.Image, ImageID: it.c.Image,
			Project: projectOf(col, it), Service: it.c.Config.Labels[labelService],
			State: stateName(it.c.State.Status, it.c.State.Paused)}
		if moved != "" {
			tag := strings.ToLower(it.c.ID[:12])
			if err := dc.TagImage(ctx, it.c.Image, "dockhop-image", tag); err != nil {
				return 0, &ui.Failure{Reason: "容器 " + it.name() + " 的镜像没法导出", Err: err}
			}
			r.tempTags = append(r.tempTags, "dockhop-image:"+tag)
			mc.OldImage = "dockhop-image:" + tag
			s.Detail("容器 %s 的%s，迁移它实际用的旧版本镜像", it.name(), moved)
		}
		if len(keep)+len(deleted) > 0 {
			s.Detail("容器 %s 里有没挂载出来的改动：%d 处新增或修改、%d 处删除，一起迁移", it.name(), len(keep), len(deleted))
			rc := changesStream(ctx, dc, it.c.ID, keep)
			parts, n, err := r.addStream(w, "changes/"+it.name(), "容器 "+it.name()+" 里的改动", rc, col.layerBytes[it])
			rc.Close()
			if err != nil {
				return 0, &ui.Failure{Reason: "打包容器 " + it.name() + " 里的改动失败", Err: err}
			}
			packed += n
			mc.Changes = &bundle.Changes{Paths: keep, Deleted: deleted, Bytes: n, Parts: parts}
		}
		m.Containers = append(m.Containers, mc)
	}
	return packed, nil
}

// packVolumes 打包选中的数据卷，记进清单。返回打包的大小。
func (r *run) packVolumes(ctx context.Context, w *bundle.Writer, m *bundle.Manifest, col *collected) (int64, error) {
	var packed int64
	for i, v := range col.volumes {
		rc, err := openVolume(ctx, r.Docker, v, fmt.Sprintf("dockhop-helper-%s-%d", r.rid, i+1), lockPath(r.Home))
		if err != nil {
			return 0, &ui.Failure{Err: err}
		}
		parts, n, err := r.addStream(w, "volumes/"+v.name, "卷 "+displayVolume(v), rc, max(v.bytes, 0))
		rc.Close()
		if err != nil {
			return 0, &ui.Failure{Reason: "打包数据卷 " + displayVolume(v) + " 失败", Err: err}
		}
		packed += n
		m.Volumes = append(m.Volumes, bundle.Volume{Name: v.name, Driver: v.driver, Anonymous: v.anonymous, Bytes: n, Parts: parts})
	}
	return packed, nil
}

// packHost 打包宿主机上的数据（见 hostRoots），把挂载的目录和 Compose 项目记进清单，连同每一份分了
// 几段。返回打包的大小。
func (r *run) packHost(ctx context.Context, w *bundle.Writer, m *bundle.Manifest, col *collected) (int64, error) {
	var packed int64
	parts := map[string]int{}
	for _, root := range col.hostRoots() {
		excludes := append(append([]string(nil), root.excludes...), r.ownDirs(root.paths...)...)
		rc := hostTar{ctx: ctx, roots: root.paths, base: root.base, excludes: excludes, bigLimit: root.bigLimit, logf: r.Log.Printf}.stream()
		n, size, err := r.addStream(w, root.stream, root.label, rc, root.bytes)
		rc.Close()
		if err != nil {
			return 0, &ui.Failure{Reason: root.fail, Err: err}
		}
		parts[root.stream] = n
		packed += size
	}
	for _, b := range col.binds {
		m.Binds = append(m.Binds, bundle.Bind{ID: b.id, Path: b.path, Link: b.link(), Links: b.links, Dir: b.dir, Bytes: b.bytes, Parts: parts[bindStream(b)]})
	}
	for _, p := range col.projects {
		mp := bundle.Project{Name: p.u.project, WorkingDir: p.u.workingDir, ConfigFiles: p.u.files, ConfigArgs: p.u.fileArgs,
			Outside: p.outside, Skipped: p.skipped, ConfigDrift: p.drift,
			DirParts: parts[projectStream(p, "dir")], CheckParts: parts[projectStream(p, "check")], OutParts: parts[projectStream(p, "outside")]}
		for _, it := range p.u.items {
			mp.Services = append(mp.Services, it.c.Config.Labels[labelService])
		}
		m.Projects = append(m.Projects, mp)
	}
	return packed, nil
}

// packImages 是第 5 步：把容器用到的每个镜像单独导出成加密文件（见 exportImages），记进清单。
func (r *run) packImages(ctx context.Context, link crypto.Link, m *bundle.Manifest, col *collected) error {
	refSet := map[string]string{}
	for _, c := range m.Containers {
		ref := c.Image
		if c.OldImage != "" {
			ref = c.OldImage
		}
		refSet[ref] = c.ImageID
	}
	m.Images.Refs = refSet
	files, n, err := r.exportImages(ctx, link, refSet, col.imageBytes)
	if err != nil {
		return &ui.Failure{Reason: "导出镜像失败", Err: err, Hint: "执行 df -h 检查磁盘空间，并确认 Docker 运行正常"}
	}
	m.Images.Bytes, m.ImageFiles = n, files
	r.removeTempTags()
	return nil
}

// exportImages 把每个镜像（同一个镜像 ID 的所有名字一起）单独导出成一个压缩、加密的
// 文件，放在工作目录的 images/ 下，由下载服务单独提供：新服务器本机已经有、或者能从
// 镜像仓库按摘要拉到一模一样的镜像时，就不用从这里下载。返回这些文件和导出的总字节数
// （压缩前）。
func (r *run) exportImages(ctx context.Context, link crypto.Link, refSet map[string]string, estimate int64) ([]bundle.ImageFile, int64, error) {
	byID := map[string][]string{}
	for ref, id := range refSet {
		byID[id] = append(byID[id], ref)
	}
	groups := make([][]string, 0, len(byID))
	for _, refs := range byID {
		sort.Strings(refs)
		groups = append(groups, refs)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i][0] < groups[j][0] })
	dir := filepath.Join(r.workdir, "images")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, 0, err
	}
	p := r.Screen.StartProgress("导出镜像", estimate, "")
	var files []bundle.ImageFile
	var total int64
	for i, refs := range groups {
		f := bundle.ImageFile{File: fmt.Sprintf("image-%d", i+1), ID: refSet[refs[0]], Refs: refs}
		f.Pull = r.pullRef(ctx, f.ID, refs)
		n, size, sum, err := r.exportImage(ctx, link, refs, filepath.Join(dir, f.File), p)
		if err != nil {
			p.Done(err)
			return nil, 0, err
		}
		f.Bytes, f.Size, f.SHA256, total = size, n, sum, total+n
		files = append(files, f)
	}
	p.Done(nil)
	return files, total, nil
}

// exportImage 导出一个镜像到 path，返回导出的字节数（压缩前）、文件大小和文件的 SHA-256。
func (r *run) exportImage(ctx context.Context, link crypto.Link, refs []string, path string, progress io.Writer) (int64, int64, string, error) {
	rc, err := r.Docker.SaveImages(ctx, refs)
	if err != nil {
		return 0, 0, "", err
	}
	defer rc.Close()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return 0, 0, "", err
	}
	var n countWriter
	sum := sha256.New()
	err = bundle.WriteImageFile(io.MultiWriter(f, sum), link, io.TeeReader(rc, io.MultiWriter(progress, &n)))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, 0, "", err
	}
	st, err := os.Stat(path)
	if err != nil {
		return 0, 0, "", err
	}
	return int64(n), st.Size(), hex.EncodeToString(sum.Sum(nil)), nil
}

// pullRef 返回能从镜像仓库拉到这个镜像的引用（名字@sha256:摘要），优先用和容器的镜像名
// 同一个仓库的；镜像是自己构建、导入的（没有仓库摘要）时返回空。按摘要拉到的一定是同一个
// 镜像，不会因为镜像名后来指向了新版本而拉错。
func (r *run) pullRef(ctx context.Context, id string, refs []string) string {
	img, err := r.Docker.InspectImage(ctx, id)
	if err != nil || len(img.RepoDigests) == 0 {
		return ""
	}
	for _, d := range img.RepoDigests {
		repo := d[:strings.LastIndex(d, "@")]
		for _, ref := range refs {
			if repoOf(ref) == repo {
				return d
			}
		}
	}
	return img.RepoDigests[0]
}

// repoOf 去掉镜像名里的标签或摘要，只留仓库名（registry.example.com:5000/app:1.0 → registry.example.com:5000/app）。
func repoOf(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	return ref
}

func imageFileBytes(files []bundle.ImageFile) int64 {
	var n int64
	for _, f := range files {
		n += f.Bytes
	}
	return n
}

type countWriter int64

func (c *countWriter) Write(p []byte) (int, error) { *c += countWriter(len(p)); return len(p), nil }

// ownDirs 是 root 里面属于 DockHop 自己的目录（日志、工作目录、正在写的迁移包），
// 打包挂载目录或项目目录时跳过，例如项目目录就是 /root、DockHop 的目录是 /root/.dockhop。
// 按真实路径比较、返回真实路径：root 可能是软链接，打包的是它指向的目录（见 walkRoot）。
func (r *run) ownDirs(roots ...string) []string {
	var out []string
	for _, root := range roots {
		root = host.RealPath(root)
		for _, d := range []string{r.Home, filepath.Dir(r.workdir)} {
			if d = host.RealPath(d); d != "" && (d == root || strings.HasPrefix(d, root+"/")) {
				out = append(out, d)
			}
		}
	}
	return out
}

// addStream 把一条数据流写进迁移包，同时显示进度。
func (r *run) addStream(w *bundle.Writer, prefix, label string, rc io.Reader, total int64) (int, int64, error) {
	p := r.Screen.StartProgress(label, total, "")
	parts, n, err := w.AddStream(prefix, io.TeeReader(rc, p))
	p.Done(err)
	return parts, n, err
}

func displayVolume(v *volumeInfo) string {
	if v.anonymous {
		return v.name[:12] + "（匿名卷）"
	}
	return v.name
}

func indexByName(items []*item) map[string]*item {
	m := map[string]*item{}
	for _, it := range items {
		m[it.name()] = it
	}
	return m
}

func indexByID(items []*item) map[string]*item {
	m := map[string]*item{}
	for _, it := range items {
		m[it.c.ID] = it
	}
	return m
}
