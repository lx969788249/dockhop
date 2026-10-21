// Package backup 是迁出：在旧服务器上选择容器、读取配置、短暂停机打包数据、
// 保存镜像，生成加密的迁移包，并提供下载，直到新服务器迁入完成。
package backup

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/compose"
	"github.com/lx969788249/dockhop/internal/crypto"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/host"
	"github.com/lx969788249/dockhop/internal/logging"
	"github.com/lx969788249/dockhop/internal/transfer"
	"github.com/lx969788249/dockhop/internal/ui"
)

// Options 是迁出的选项。
type Options struct {
	Include    []string
	NoStop     bool
	StopShared bool
	Snapshot   string // auto、always、never
	Port       int
	Host       string
	Workdir    string
}

// Deps 是迁出需要的外部条件。
type Deps struct {
	Docker   *docker.Client
	Screen   *ui.Screen
	Prompt   *ui.Prompter
	Log      *logging.Logger
	Redactor *logging.Redactor
	Home     string
	Version  string
	Getenv   func(string) string
}

// ErrReported 表示失败原因已经显示在结果摘要里。
var ErrReported = errors.New("迁出失败")

// run 保存一次迁出过程中的状态，出错或中断时据此收拾。
type run struct {
	Deps
	opts      Options
	started   time.Time
	rid       string
	workdir   string
	stopped   []StoppedContainer // 已经停下、还没恢复的源容器
	restored  int
	toRestore int
	downtime  time.Duration
	tempTags  []string // 为导出旧版本镜像临时加的名字
	server    *transfer.Server
	status    string // 新服务器报告的迁入结果
	summary   []string
}

// Run 执行迁出。不管成功、失败还是被中断，都会把源容器恢复原状、删除临时文件，
// 最后打印结果摘要。
func Run(ctx context.Context, d Deps, o Options) error {
	r := &run{Deps: d, opts: o, started: time.Now(), rid: randomID()}
	lock, err := host.TryLock(filepath.Join(d.Home, "locks", "out.lock"))
	if err != nil {
		d.Screen.Error(err.Error(), "")
		return ErrReported
	}
	defer lock.Unlock()
	removeHelpers(ctx, d.Docker)

	err = r.steps(ctx)
	r.cleanup()
	return r.report(ctx, err)
}

func (r *run) steps(ctx context.Context) error {
	s, dc := r.Screen, r.Docker

	s.Begin(1, 7, "检查环境")
	info, err := dc.Info(ctx)
	if err != nil {
		return fail(s, "读取 Docker 信息失败", err.Error(), "")
	}
	tool, terr := compose.Detect(ctx)
	envSummary := "Docker " + info.ServerVersion
	if terr == nil {
		envSummary += " · Compose " + tool.Version
		r.Log.Printf("Compose：%s", tool)
	} else {
		tool = nil
	}
	s.End(envSummary)

	s.Begin(2, 7, "选择容器")
	inv, err := buildInventory(ctx, dc, s)
	if err != nil {
		return fail(s, err.Error(), "", "")
	}
	chosen, err := chooseUnits(inv, r.opts.Include, s, r.Prompt)
	if err != nil {
		return fail(s, err.Error(), "", "")
	}
	sel, err := withDependencies(inv, chosen, s)
	if err != nil {
		return fail(s, err.Error(), "", "")
	}
	if tool == nil && sel.projectCount() > 0 {
		s.Warn("这台服务器上找不到 docker compose 命令，无法核对 Compose 配置，将直接迁移项目目录")
	}
	s.End(countText(sel))

	s.Begin(3, 7, "读取配置")
	col, err := collect(ctx, dc, tool, inv, sel, s, r.ignored())
	if err != nil {
		return fail(s, err.Error(), "", "")
	}
	for _, v := range col.secrets {
		r.Redactor.AddSecret(v)
	}
	if !dc.Local() && (len(col.binds) > 0 || len(col.projects) > 0) {
		return fail(s, "Docker 不在这台服务器上（DOCKER_HOST 指向了别的机器），没法读取容器挂载的目录和 Compose 项目文件", "",
			"请在 Docker 所在的服务器上直接运行 DockHop")
	}
	for _, p := range col.systemOnly {
		s.Detail("宿主机系统路径只在新服务器重建挂载、不迁移内容：%s", p)
	}
	if err := r.confirmDrift(col); err != nil {
		return err
	}
	if err := r.confirmShared(col); err != nil {
		return err
	}
	for _, p := range col.projects {
		for _, f := range p.skipped {
			s.Warn("Compose 项目 %s 目录里的大文件 %s 没有迁移（超过 100MB）；需要的话请手动复制", p.u.project, f)
		}
	}
	r.workdir = r.opts.Workdir
	if r.workdir == "" {
		r.workdir = filepath.Join(r.Home, "work")
	}
	r.workdir = filepath.Join(r.workdir, "out-"+r.rid)
	need := col.dataBytes() + col.imageBytes
	disk, err := host.DiskOf(r.workdir)
	if err == nil && disk.Free < uint64(need) {
		return fail(s, fmt.Sprintf("磁盘空间可能不够：迁移包最多需要约 %s（压缩前），%s 所在磁盘只剩 %s",
			ui.HumanBytes(need), disk.Path, ui.HumanBytes(int64(disk.Free))),
			"清理磁盘，或用 --workdir 指定一个空间更大的目录", "")
	}
	if err := os.MkdirAll(r.workdir, 0o700); err != nil {
		return fail(s, "无法创建工作目录 "+r.workdir, err.Error(), "")
	}
	ln, port, err := transfer.Listen(r.opts.Port)
	if err != nil {
		return fail(s, err.Error(), "用 --port 换一个端口", "")
	}
	addr := transfer.Address{Host: r.opts.Host}
	if addr.Host == "" {
		addr = transfer.DetectAddress(ctx)
	}
	if addr.Note != "" {
		s.Warn("%s", addr.Note)
	}
	link, err := crypto.NewLink(addr.Host, port)
	if err != nil {
		ln.Close()
		return fail(s, "生成迁移链接失败", err.Error(), "")
	}
	r.Redactor.AddSecret(link.Secret())
	free := "剩余空间未知"
	if disk.Total > 0 {
		free = "剩余 " + ui.HumanBytes(int64(disk.Free))
	}
	s.End(fmt.Sprintf("需要约 %s · %s", ui.HumanBytes(need), free))

	bundlePath := filepath.Join(r.workdir, "bundle")
	planPath := filepath.Join(r.workdir, "plan")
	m, size, err := r.pack(ctx, col, link, bundlePath)
	if err != nil {
		ln.Close()
		return err
	}

	pf, err := os.OpenFile(planPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err == nil {
		err = bundle.EncryptPlan(pf, link, buildPlan(m, col, size))
		pf.Close()
	}
	if err != nil {
		s.Warn("迁移计划生成失败，新服务器将跳过迁入前检查：%v", err)
		planPath = ""
	}
	s.End(ui.HumanBytes(size))

	s.Begin(7, 7, "启动下载服务")
	return r.serve(ctx, ln, link, bundlePath, planPath, size)
}

// pack 是第 4 到第 6 步：停机打包数据、保存镜像、写完迁移包。
func (r *run) pack(ctx context.Context, col *collected, link crypto.Link, path string) (*bundle.Manifest, int64, error) {
	s, dc := r.Screen, r.Docker
	s.Begin(4, 7, "停机并打包数据")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return nil, 0, fail(s, "无法创建迁移包", err.Error(), "")
	}
	defer f.Close()
	w, err := bundle.NewWriter(f, link)
	if err != nil {
		return nil, 0, fail(s, "无法创建迁移包", err.Error(), "")
	}
	info, _ := dc.Info(ctx)
	hostname, _ := os.Hostname()
	m := &bundle.Manifest{Format: bundle.Format, Version: bundle.Version, CreatedAt: time.Now().UTC(), DockHop: r.Version,
		Source: bundle.Source{Hostname: hostname, Docker: dc.Server().Version, API: dc.APIVersion(), Arch: info.Architecture,
			OS: info.OperatingSystem, CgroupVersion: info.CgroupVersion, LoggingDriver: info.LoggingDriver,
			DefaultRuntime: info.DefaultRuntime, StorageDriver: info.Driver},
		Images: bundle.Images{Refs: map[string]string{}}}
	for _, it := range col.sel.items {
		if err := w.AddJSON("containers/"+it.name()+".json", it.raw); err != nil {
			return nil, 0, fail(s, "写入迁移包失败", err.Error(), "")
		}
	}
	for _, n := range col.networks {
		m.Networks = append(m.Networks, n.name)
		if err := w.AddJSON("networks/"+n.name+".json", n.raw); err != nil {
			return nil, 0, fail(s, "写入迁移包失败", err.Error(), "")
		}
	}
	for _, v := range col.volumes {
		if err := w.AddJSON("volumes/"+v.name+".json", v.raw); err != nil {
			return nil, 0, fail(s, "写入迁移包失败", err.Error(), "")
		}
	}

	// 停机：先登记再停，程序在任何时候被强制结束，下次运行都能恢复它们。
	var stopList []StoppedContainer
	var waves [][]StoppedContainer
	if r.opts.NoStop {
		s.Warn("不停机打包：数据库等正在写入的数据可能不一致")
	} else {
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
	}
	downStart := time.Now()
	if len(stopList) > 0 {
		if err := writeStopped(r.Home, stopList); err != nil {
			return nil, 0, fail(s, "无法写入停机记录，为了安全没有停止任何容器", err.Error(), "检查 "+r.Home+" 是否可写")
		}
		r.stopped, r.toRestore = stopList, len(stopList)
		if err := stopContainers(ctx, dc, waves, s); err != nil {
			return nil, 0, fail(s, err.Error(), "执行 docker ps 检查容器状态", "")
		}
		r.testPause(ctx, "after-stop")
	}

	// 容器里没挂载出来的改动：只打包改动的文件，迁入时写进新建的容器。
	oldImageOf := map[*item]string{}
	changesOf := map[*item]*bundle.Changes{}
	var packed int64
	for i, it := range col.sel.items {
		s.Count("检查容器里的改动", i+1, len(col.sel.items), it.name())
		moved, keep, deleted, err := layerState(ctx, dc, it, r.opts.Snapshot)
		if err != nil {
			return nil, 0, fail(s, "检查容器 "+it.name()+" 的改动失败", err.Error(), "")
		}
		if moved != "" {
			// 镜像名已经指向别的版本：给容器实际用的旧版本临时起个名字，导出它。
			tag := strings.ToLower(it.c.ID[:12])
			if err := dc.TagImage(ctx, it.c.Image, "dockhop-image", tag); err != nil {
				return nil, 0, fail(s, "容器 "+it.name()+" 的镜像没法导出", err.Error(), "")
			}
			r.tempTags = append(r.tempTags, "dockhop-image:"+tag)
			oldImageOf[it] = "dockhop-image:" + tag
			s.Detail("容器 %s 的%s，迁移它实际用的旧版本镜像", it.name(), moved)
		}
		if len(keep)+len(deleted) == 0 {
			continue
		}
		s.Detail("容器 %s 里有没挂载出来的改动：%d 处新增或修改、%d 处删除，一起迁移", it.name(), len(keep), len(deleted))
		rc := changesStream(ctx, dc, it.c.ID, keep)
		parts, n, err := r.addStream(w, "changes/"+it.name(), "容器 "+it.name()+" 里的改动", rc, 0)
		rc.Close()
		if err != nil {
			return nil, 0, fail(s, "打包容器 "+it.name()+" 里的改动失败", err.Error(), "")
		}
		packed += n
		changesOf[it] = &bundle.Changes{Paths: keep, Deleted: deleted, Bytes: n, Parts: parts}
	}

	for i, v := range col.volumes {
		rc, err := openVolume(ctx, dc, v, fmt.Sprintf("dockhop-helper-%s-%d", r.rid, i+1))
		if err != nil {
			return nil, 0, fail(s, err.Error(), "", "")
		}
		parts, n, err := r.addStream(w, "volumes/"+v.name, "卷 "+displayVolume(v), rc, max(v.bytes, 0))
		rc.Close()
		if err != nil {
			return nil, 0, fail(s, "打包数据卷 "+displayVolume(v)+" 失败", err.Error(), "")
		}
		packed += n
		m.Volumes = append(m.Volumes, bundle.Volume{Name: v.name, Driver: v.driver, Anonymous: v.anonymous, Bytes: n, Parts: parts})
	}
	for _, b := range col.binds {
		rc := hostTar{ctx: ctx, roots: []string{b.path}, base: "/", excludes: r.ownDirs(b.path), logf: r.Log.Printf}.stream()
		parts, n, err := r.addStream(w, "binds/"+b.id, "目录 "+b.path, rc, b.bytes)
		rc.Close()
		if err != nil {
			return nil, 0, fail(s, "打包目录 "+b.path+" 失败", err.Error(), "")
		}
		packed += n
		m.Binds = append(m.Binds, bundle.Bind{ID: b.id, Path: b.path, Link: b.link, Dir: b.dir, Bytes: b.bytes, Parts: parts})
	}
	for _, p := range col.projects {
		mp := bundle.Project{Name: p.u.project, WorkingDir: p.u.workingDir, ConfigFiles: p.u.files, ConfigArgs: p.u.fileArgs,
			Outside: p.outside, Skipped: p.skipped, ConfigDrift: p.drift}
		for _, it := range p.u.items {
			mp.Services = append(mp.Services, it.c.Config.Labels[labelService])
		}
		if !p.inBind {
			excludes := append(append([]string(nil), p.excludes...), r.ownDirs(p.u.workingDir)...)
			rc := hostTar{ctx: ctx, roots: []string{p.u.workingDir}, base: p.u.workingDir, excludes: excludes, bigLimit: bigFileLimit, logf: r.Log.Printf}.stream()
			parts, n, err := r.addStream(w, "compose/"+p.u.project+"/dir", "Compose 项目 "+p.u.project, rc, p.dirBytes)
			rc.Close()
			if err != nil {
				return nil, 0, fail(s, "打包 Compose 项目 "+p.u.project+" 失败", err.Error(), "")
			}
			mp.DirParts = parts
			packed += n
		}
		if len(p.outside) > 0 {
			rc := hostTar{ctx: ctx, roots: p.outside, base: "/", logf: r.Log.Printf}.stream()
			parts, n, err := r.addStream(w, "compose/"+p.u.project+"/outside", "Compose 项目 "+p.u.project+" 引用的文件", rc, p.outBytes)
			rc.Close()
			if err != nil {
				return nil, 0, fail(s, "打包 Compose 项目 "+p.u.project+" 引用的文件失败", err.Error(), "")
			}
			mp.OutParts = parts
			packed += n
		}
		m.Projects = append(m.Projects, mp)
	}

	// 数据都已经打包，立即恢复源容器：后面的镜像导出、压缩和下载都不需要停机。
	if len(r.stopped) > 0 {
		r.downtime = time.Since(downStart)
		if failed := restoreStates(context.WithoutCancel(ctx), dc, r.stopped, s); len(failed) > 0 {
			return nil, 0, fail(s, fmt.Sprintf("有 %d 个源容器没能恢复原来的状态：%s", len(failed), strings.Join(failed, "、")),
				"立即执行 docker ps -a 检查，并用 docker start 容器名 手动启动", "")
		}
		r.restored = len(r.stopped)
		r.stopped = nil
		_ = removeStopped(r.Home)
	}
	summary := fmt.Sprintf("%d 个卷 · %d 个目录 · %s", len(m.Volumes), len(m.Binds), ui.HumanBytes(packed))
	if r.downtime > 0 {
		summary += " · 停机 " + ui.Elapsed(r.downtime)
	}
	s.End(summary)

	s.Begin(5, 7, "保存镜像")
	refSet := map[string]string{}
	for _, it := range col.sel.items {
		ref := it.c.Config.Image
		if old := oldImageOf[it]; old != "" {
			ref = old
		}
		refSet[ref] = it.c.Image
		m.Containers = append(m.Containers, bundle.Container{Name: it.name(), ID: it.c.ID, Image: it.c.Config.Image, ImageID: it.c.Image,
			OldImage: oldImageOf[it], Changes: changesOf[it], Project: projectOf(col, it), Service: it.c.Config.Labels[labelService],
			State: stateName(it.c.State.Status, it.c.State.Paused)})
	}
	refs := make([]string, 0, len(refSet))
	for ref := range refSet {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	m.Images.Refs = refSet
	rc, err := dc.SaveImages(ctx, refs)
	if err != nil {
		return nil, 0, fail(s, "导出镜像失败", err.Error(), "")
	}
	parts, n, err := r.addStream(w, "images", "导出镜像", rc, col.imageBytes)
	rc.Close()
	if err != nil {
		return nil, 0, fail(s, "导出镜像失败", err.Error(), "执行 df -h 检查磁盘空间，并确认 Docker 运行正常")
	}
	m.Images.Bytes, m.Images.Parts = n, parts
	r.removeTempTags()
	s.End(fmt.Sprintf("%d 个镜像 · %s", len(refs), ui.HumanBytes(n)))

	s.Begin(6, 7, "生成迁移包")
	// 包里也放一份迁移计划：用 --file 迁入时没有下载服务，靠它做迁入前检查。
	if err := w.AddJSON("plan.json", buildPlan(m, col, 0)); err != nil {
		return nil, 0, fail(s, "写入迁移包失败", err.Error(), "")
	}
	if err := w.AddJSON("manifest.json", m); err != nil {
		return nil, 0, fail(s, "写入迁移包失败", err.Error(), "")
	}
	if err := w.Close(); err != nil {
		return nil, 0, fail(s, "写入迁移包失败", err.Error(), "执行 df -h 检查磁盘空间")
	}
	if err := f.Close(); err != nil {
		return nil, 0, fail(s, "写入迁移包失败", err.Error(), "执行 df -h 检查磁盘空间")
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, 0, fail(s, "写入迁移包失败", err.Error(), "")
	}
	r.summary = append(r.summary, fmt.Sprintf("迁移包：%d 个容器 · %d 个卷 · %d 个目录 · %d 个镜像 · %s",
		len(m.Containers), len(m.Volumes), len(m.Binds), len(refs), ui.HumanBytes(st.Size())))
	return m, st.Size(), nil
}

// ownDirs 是 root 里面属于 DockHop 自己的目录（日志、工作目录、正在写的迁移包），
// 打包挂载目录或项目目录时跳过，例如项目目录就是 /root、DockHop 的目录是 /root/.dockhop。
func (r *run) ownDirs(root string) []string {
	var out []string
	for _, d := range []string{r.Home, filepath.Dir(r.workdir)} {
		if d != "" && (d == root || strings.HasPrefix(d, root+"/")) {
			out = append(out, d)
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

// serve 是第 7 步：提供下载，直到新服务器迁入成功、用户按回车或程序被结束。
func (r *run) serve(ctx context.Context, ln net.Listener, link crypto.Link, bundlePath, planPath string, size int64) error {
	s := r.Screen
	var mu sync.Mutex
	var prog *ui.Progress
	statusCh := make(chan transfer.Status, 4)
	completeCh := make(chan struct{}, 1)
	notify := func(e transfer.Event) {
		switch e.Kind {
		case transfer.DownloadProgress:
			mu.Lock()
			if prog == nil {
				prog = s.StartProgress("新服务器正在下载", e.Size, "下载停住了：新服务器可能断开了，它会自动从断点继续")
			}
			prog.Set(e.Position)
			mu.Unlock()
		case transfer.DownloadComplete:
			select {
			case completeCh <- struct{}{}:
			default:
			}
		case transfer.StatusReceived:
			statusCh <- e.Status
		}
	}
	srv, err := transfer.NewServer(ln, link, bundlePath, planPath, notify, r.Log.Printf)
	if err != nil {
		ln.Close()
		return fail(s, "启动下载服务失败", err.Error(), "")
	}
	r.server = srv
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()
	s.End(fmt.Sprintf("端口 %d", link.Port))
	r.summary = append(r.summary, "下载链接：已提供")

	s.Println(r.instructions(ctx, link))
	enter := make(chan struct{}, 1)
	if r.Prompt.Interactive() {
		go func() {
			if _, err := r.Prompt.ReadLine(); err == nil {
				enter <- struct{}{}
			}
		}()
	}
	stopProgress := func() {
		mu.Lock()
		if prog != nil {
			prog.Done(nil)
			prog = nil
		}
		mu.Unlock()
	}
	for {
		select {
		case <-ctx.Done():
			stopProgress()
			s.Detail("收到结束信号，结束传输")
			return nil
		case <-enter:
			stopProgress()
			s.Detail("用户按回车结束传输")
			return nil
		case <-completeCh:
			stopProgress()
			s.Println("新服务器已经下载完迁移包，正在迁入……")
		case st := <-statusCh:
			stopProgress()
			if st.Result == "success" {
				r.status = "success"
				s.Println("新服务器已迁入成功，自动结束传输。")
				return nil
			}
			r.status = "failed"
			msg := "新服务器迁入失败"
			if st.Message != "" {
				msg += "：" + st.Message
			}
			s.Warn("%s。下载服务继续保留，处理好之后在新服务器重新运行迁入即可", msg)
		case err := <-serveErr:
			stopProgress()
			if err != nil {
				return fail(nil, "下载服务意外停止", err.Error(), "重新运行迁出")
			}
			return nil
		}
	}
}

func (r *run) instructions(ctx context.Context, link crypto.Link) string {
	var b strings.Builder
	b.WriteString("\n━━━━━━━━━━ 迁移链接（请完整复制）━━━━━━━━━━\n")
	b.WriteString(link.String() + "\n\n")
	b.WriteString("在新服务器上运行：\n")
	b.WriteString("bash <(curl -fsSL https://raw.githubusercontent.com/lx969788249/dockhop/main/run.sh) in '" + link.String() + "'\n\n")
	b.WriteString(transfer.FirewallHint(ctx, link.Port) + "。\n")
	if r.Prompt.Interactive() {
		b.WriteString("新服务器迁入成功后这里会自动结束；也可以随时按回车结束传输。\n")
	} else {
		b.WriteString(fmt.Sprintf("新服务器迁入成功后这里会自动结束；也可以执行 kill -TERM %d 结束传输。\n", os.Getpid()))
	}
	b.WriteString("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	return b.String()
}

// cleanup 收尾：恢复还停着的源容器、删掉临时快照和临时容器、关闭下载服务、
// 删除工作目录。中断时也会执行。
func (r *run) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if r.server != nil {
		_ = r.server.Shutdown(ctx)
	}
	if len(r.stopped) > 0 {
		failed := restoreStates(ctx, r.Docker, r.stopped, r.Screen)
		r.restored = r.toRestore - len(failed)
		if len(failed) == 0 {
			_ = removeStopped(r.Home)
		} else {
			r.Screen.Error(fmt.Sprintf("有 %d 个源容器没能恢复原来的状态：%s", len(failed), strings.Join(failed, "、")),
				"立即执行 docker ps -a 检查，并用 docker start 容器名 手动启动")
		}
		r.stopped = nil
	}
	r.removeTempTags()
	removeHelpers(ctx, r.Docker)
	if r.workdir != "" {
		if err := os.RemoveAll(r.workdir); err != nil {
			r.Screen.Warn("临时文件没能删除，请手动删除：%s", r.workdir)
		}
	}
}

// removeTempTags 删掉为导出旧版本镜像临时加的名字（只删名字，镜像本身还在用）。
func (r *run) removeTempTags() {
	ctx := context.Background()
	var left []string
	for _, ref := range r.tempTags {
		if err := r.Docker.RemoveImage(ctx, ref); err != nil && !docker.IsNotFound(err) {
			left = append(left, ref)
		}
	}
	r.tempTags = left
}

// report 打印结果摘要。
func (r *run) report(ctx context.Context, err error) error {
	interrupted := ctx.Err() != nil && r.server == nil
	success := err == nil && !interrupted
	lines := append([]string(nil), r.summary...)
	if r.toRestore > 0 {
		line := fmt.Sprintf("源容器：已恢复 %d/%d", r.restored, r.toRestore)
		if r.downtime > 0 {
			line += "（停机 " + ui.Elapsed(r.downtime) + "）"
		}
		if r.restored < r.toRestore {
			line = fmt.Sprintf("源容器：只恢复了 %d/%d，请立即人工检查", r.restored, r.toRestore)
			success = false
		}
		lines = append(lines, line)
	}
	switch r.status {
	case "success":
		lines = append(lines, "新服务器：迁入成功")
	case "failed":
		lines = append(lines, "新服务器：迁入失败（详情见新服务器上的结果）")
	}
	rep := ui.Report{Title: "迁出结果", Lines: lines, Elapsed: time.Since(r.started), Success: success}
	switch {
	case success:
		rep.Result = "✅ 迁出完成"
	case interrupted:
		rep.Result = "⚠️ 迁出已中断，源容器已恢复原状"
	default:
		rep.Result = "❌ 迁出失败"
		r.Screen.Fail()
	}
	if !success {
		rep.LogPath = r.Log.Path()
		rep.LogTail = r.Log.Tail(15)
	}
	r.Screen.PrintReport(rep)
	if success {
		return nil
	}
	return ErrReported
}

// fail 记录失败原因（显示在结果摘要里）并结束当前步骤。
func fail(s *ui.Screen, reason, detail, hint string) error {
	msg := reason
	if detail != "" && detail != reason {
		msg += "：" + detail
	}
	if s != nil {
		s.Error(msg, hint)
	}
	return ErrReported
}

func (r *run) confirmDrift(col *collected) error {
	for _, p := range col.projects {
		if len(p.drift) == 0 {
			continue
		}
		msg := fmt.Sprintf("Compose 项目 %s 的服务 %s 现在运行的配置和目录里的文件对不上（启动后改过文件，或启动时用了临时设置的变量）",
			p.u.project, strings.Join(p.drift, "、"))
		if !r.Prompt.Interactive() {
			r.Screen.Warn("%s；迁入后会按现在的文件启动", msg)
			continue
		}
		r.Screen.Hold()
		r.Screen.Println(msg)
		n, err := r.Prompt.Choose([]string{
			"取消，我先在 " + p.u.workingDir + " 执行 docker compose up -d，让运行的容器和文件一致（推荐）",
			"仍然迁移，迁入后按现在的文件启动",
		}, 1)
		if err != nil {
			return err
		}
		if n == 1 {
			return fail(r.Screen, "已取消：Compose 项目 "+p.u.project+" 的配置和运行中的容器不一致", "", "处理好之后重新运行迁出")
		}
		r.Screen.Warn("%s；迁入后会按现在的文件启动", msg)
	}
	return nil
}

func (r *run) confirmShared(col *collected) error {
	if len(col.shared) == 0 {
		return nil
	}
	names := make([]string, len(col.shared))
	for i, it := range col.shared {
		names[i] = it.name()
	}
	list := strings.Join(names, "、")
	if r.opts.NoStop {
		return fail(r.Screen, "没有选中的容器 "+list+" 和选中的容器共用数据或网络，不停机打包无法保证数据一致，已取消", "",
			"去掉 --no-stop，让 DockHop 临时停止并自动恢复它们")
	}
	if r.opts.StopShared {
		r.Screen.Warn("没有选中的容器 %s 和选中的容器共用数据或网络，打包时会临时停止，打包完自动恢复", list)
		return nil
	}
	if !r.Prompt.Interactive() {
		return fail(r.Screen, "没有选中的容器 "+list+" 和选中的容器共用数据或网络", "",
			"加上 --stop-shared，允许打包时临时停止它们（打包完自动恢复）")
	}
	r.Screen.Hold()
	ok, err := r.Prompt.Confirm("没有选中的容器 "+list+" 和选中的容器共用数据或网络。打包时临时停止它们吗？打包完会自动恢复", true)
	if err != nil {
		return err
	}
	if !ok {
		return fail(r.Screen, "已取消：和选中的容器共用数据的容器不能停止，数据无法保证一致", "", "")
	}
	return nil
}

// ignored 是测试时要忽略的容器（例如运行测试的容器自己）。
func (r *run) ignored() map[string]bool {
	m := map[string]bool{}
	for _, n := range strings.Split(r.Getenv("DOCKHOP_TEST_IGNORE"), ",") {
		if n = strings.TrimSpace(n); n != "" {
			m[n] = true
		}
	}
	return m
}

// testPause 只在测试时使用：在指定位置停住，等测试程序删掉标记文件后再继续，
// 用来模拟“停机期间被强制结束”。
func (r *run) testPause(ctx context.Context, point string) {
	if r.Getenv("DOCKHOP_TEST_PAUSE") != point {
		return
	}
	marker := filepath.Join(r.Home, "test-paused-"+point)
	_ = os.WriteFile(marker, nil, 0o600)
	for {
		if _, err := os.Stat(marker); err != nil || ctx.Err() != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func countText(sel *selection) string {
	t := fmt.Sprintf("%d 个容器", len(sel.items))
	if n := sel.projectCount(); n > 0 {
		t += fmt.Sprintf("（%d 个 Compose 项目）", n)
	}
	return t
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

func randomID() string {
	const alphabet = "abcdefghijkmnopqrstuvwxyz23456789"
	b := make([]byte, 10)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b[i] = alphabet[n.Int64()]
	}
	return string(b)
}
