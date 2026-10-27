// Package restore 是迁入：在新服务器上检查、下载迁移包、恢复数据和服务。
//
// 对新服务器的每一处改动都先记进事务日志（journal.go）。任何一步失败或被中断，
// 都按日志删除这次迁入的内容、把被替换的原有服务放回去；全部服务验证通过后才“提交”。
// 一次迁入多组服务、只有一部分没能启动时，可以选择只删除没启动的（groups.go）。
package restore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/compose"
	"github.com/lx969788249/dockhop/internal/crypto"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/host"
	"github.com/lx969788249/dockhop/internal/logging"
	"github.com/lx969788249/dockhop/internal/preflight"
	"github.com/lx969788249/dockhop/internal/spec"
	"github.com/lx969788249/dockhop/internal/transfer"
	"github.com/lx969788249/dockhop/internal/ui"
)

// Options 是迁入的选项。
type Options struct {
	Link          crypto.Link
	File          string // 用别的方式带过来的迁移包
	Yes           bool
	Existing      string // replace、skip、fail；空表示询问
	SkipCheck     bool
	HealthTimeout time.Duration
	KeepFiles     bool
	CheckOnly     bool // 只检查，不下载、不改动
	KeepSucceeded bool // 有的服务没能启动时，不提问，保留已经正常运行的
}

// Deps 是迁入需要的外部条件。
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
var ErrReported = errors.New("迁入失败")

type run struct {
	Deps
	opts    Options
	started time.Time
	steps   int
	tool    *compose.Tool
	info    docker.Info

	workdir    string // 这次迁入的下载和解压目录
	bundlePath string
	dataDir    string
	downloaded bool // 迁移包是这次下载的（不是 --file 给的）

	plan        *bundle.Plan
	m           *bundle.Manifest
	d           preflight.Decisions
	items       []*citem // 要迁入的容器（去掉跳过的）
	projects    []bundle.Project
	anonVolumes map[string]bundle.Volume // 旧的匿名卷编号 → 迁移包里的数据
	imageAs     map[string]string        // 镜像名 → 实际使用的镜像名（digest 改用迁移包里的镜像时）
	helperImage string                   // 创建临时容器用的镜像（名字）

	j       *Journal
	rbDir   string
	undo    *undo
	vio     *volumeIO
	targets map[string]bool   // 被替换的已有容器名
	shared  []Entry           // 临时停止的、和迁入的数据共用卷或目录的容器
	groups  []*group          // 要迁入的服务分成的组（见 groups.go）
	groupOf map[string]string // 容器、项目、卷、目录、网络 → 所在组

	done []string // 同一个链接上次已经迁入、这次跳过的服务

	committed      bool
	partial        bool // 只保留了一部分服务
	rollbackDone   bool
	rollbackIssues []string
	cleanupLeft    []string
}

// Run 执行迁入（或者只检查）。
func Run(ctx context.Context, d Deps, o Options) error {
	r := &run{Deps: d, opts: o, started: time.Now(), imageAs: map[string]string{}, anonVolumes: map[string]bundle.Volume{},
		rbDir: filepath.Join(d.Home, "rollback", "current")}
	r.steps = 8
	if o.CheckOnly {
		r.steps = 2
		if o.File != "" {
			r.steps = 3
		}
	}
	lock, err := host.TryLock(filepath.Join(d.Home, "locks", "in.lock"))
	if err != nil {
		d.Screen.Error(err.Error(), "")
		return ErrReported
	}
	defer lock.Unlock()
	if !o.CheckOnly {
		removeHelpers(ctx, d.Docker)
	}
	err = r.run(ctx)
	if err != nil && r.j != nil && !r.committed {
		r.rollback(context.WithoutCancel(ctx))
	}
	r.j.close()
	r.cleanFiles(err == nil)
	return r.report(ctx, err)
}

func (r *run) testFail(point string) bool {
	for _, p := range strings.Split(r.Getenv("DOCKHOP_TEST_FAIL"), ",") {
		if strings.TrimSpace(p) == point {
			return true
		}
	}
	return false
}

// testPause 只在测试时使用：在指定位置停住，等测试程序删掉标记文件后再继续，
// 用来模拟“迁入到一半被 Ctrl+C 或 kill -9”。
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

func (r *run) run(ctx context.Context) error {
	s := r.Screen
	s.Begin(1, r.steps, "检查环境")
	info, err := r.Docker.Info(ctx)
	if err != nil {
		return fail(s, "读取 Docker 信息失败", err.Error(), "")
	}
	r.info = info
	r.undo = &undo{dc: r.Docker, screen: s, logf: r.Log.Printf, fail: r.testFail,
		defaults: spec.SourceDefaults{CgroupVersion: info.CgroupVersion, LoggingDriver: info.LoggingDriver, DefaultRuntime: info.DefaultRuntime}}
	summary := "Docker " + info.ServerVersion
	if t, err := compose.Detect(ctx); err == nil {
		r.tool = t
		summary += " · Compose " + t.Version
	}
	if !r.opts.CheckOnly {
		if err := r.recoverInterrupted(ctx); err != nil {
			return err
		}
	}
	s.End(summary)

	if r.opts.File == "" {
		s.Begin(2, r.steps, "迁入前检查")
		if err := r.fetchPlan(ctx); err != nil {
			return err
		}
		summary, err := r.check(ctx)
		if err != nil {
			return err
		}
		s.End(summary)
		if r.opts.CheckOnly {
			return nil
		}
		s.Begin(3, r.steps, "下载迁移包")
		if err := r.download(ctx); err != nil {
			return err
		}
		summary, err = r.unpack(ctx)
		if err != nil {
			return err
		}
		if err := r.checkCompose(ctx); err != nil {
			return err
		}
		s.End(summary)
	} else {
		s.Begin(2, r.steps, "解开迁移包")
		r.bundlePath = r.opts.File
		if err := r.prepareWorkdir(""); err != nil {
			return err
		}
		summary, err := r.unpack(ctx)
		if err != nil {
			return err
		}
		s.End(summary)
		s.Begin(3, r.steps, "迁入前检查")
		summary, err = r.check(ctx)
		if err != nil {
			return err
		}
		if r.opts.CheckOnly {
			s.End(summary)
			return nil
		}
		if err := r.checkCompose(ctx); err != nil {
			return err
		}
		s.End(summary)
	}

	s.Begin(4, r.steps, "加载镜像")
	if err := r.loadImages(ctx); err != nil {
		return err
	}
	s.Begin(5, r.steps, "备份已有服务")
	if err := r.prepare(ctx); err != nil {
		return err
	}
	s.Begin(6, r.steps, "恢复数据")
	if err := r.restoreData(ctx); err != nil {
		return err
	}
	s.Begin(7, r.steps, "启动服务")
	if err := r.startServices(ctx); err != nil {
		return err
	}
	s.Begin(8, r.steps, "完成迁入")
	return r.commit(ctx)
}

// ---------- 迁入前检查和下载 ----------

func (r *run) client() *transfer.Client {
	timeout := 15 * time.Second
	if v := r.Getenv("DOCKHOP_TEST_CONNECT_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			timeout = d
		}
	}
	return transfer.NewClient(timeout, r.Log.Printf)
}

func (r *run) fetchPlan(ctx context.Context) error {
	s := r.Screen
	s.Note("连接旧服务器 " + r.opts.Link.Host)
	raw, err := r.client().FetchPlan(ctx, r.opts.Link)
	if errors.Is(err, transfer.ErrNoPlan) {
		s.Warn("旧服务器没有提供迁移计划，跳过迁入前检查（有问题会在迁入过程中发现，并自动删除迁入的内容）")
		return nil
	}
	if err != nil {
		var ce *transfer.ConnError
		if errors.As(err, &ce) {
			return fail(s, ce.Reason, "", ce.Hint)
		}
		return fail(s, "连接旧服务器失败", err.Error(), "")
	}
	p, err := bundle.DecryptPlan(bytes.NewReader(raw), r.opts.Link)
	if err != nil {
		return fail(s, "迁移计划解不开："+err.Error(), "", "确认用的是旧服务器这次显示的完整链接")
	}
	r.plan = p
	return nil
}

// check 是迁入前检查，返回这一步的摘要。
func (r *run) check(ctx context.Context) (string, error) {
	s := r.Screen
	r.skipDone(ctx)
	if r.plan == nil {
		return "跳过", nil
	}
	if r.opts.SkipCheck {
		s.Warn("按 --skip-check 跳过迁入前检查")
		return "跳过", nil
	}
	runner := &preflight.Runner{Screen: s, Prompt: r.Prompt, Auto: r.opts.Yes,
		Collect: func() (*preflight.Target, error) {
			v := ""
			if r.tool != nil {
				v = r.tool.Version
			}
			return preflight.Collect(context.Background(), r.Docker, v)
		}}
	res, err := runner.Run(r.plan, &r.d, preflight.Options{Existing: r.opts.Existing, BaseDir: r.workBase(),
		BundleOnDisk: r.opts.File != "", Home: r.Home})
	if err != nil {
		var p *preflight.Problem
		if errors.As(err, &p) {
			return "", fail(s, p.Reason, "", p.Hint)
		}
		return "", fail(s, err.Error(), "", "")
	}
	b, _ := json.Marshal(r.d)
	r.Log.Printf("检查时的选择：%s", b)
	summary := fmt.Sprintf("%d 个容器", res.Containers)
	if res.NeedBytes > 0 {
		summary += " · 需要约 " + ui.HumanBytes(res.NeedBytes)
	}
	return summary + " · 检查通过", nil
}

func (r *run) workBase() string {
	if r.d.Workdir != "" {
		return r.d.Workdir
	}
	return filepath.Join(r.Home, "work")
}

func (r *run) prepareWorkdir(token string) error {
	if token == "" {
		token = r.opts.Link.Token
	}
	r.workdir = filepath.Join(r.workBase(), "in-"+token)
	r.dataDir = filepath.Join(r.workdir, "data")
	if err := os.MkdirAll(r.workdir, 0o700); err != nil {
		return fail(r.Screen, "无法创建工作目录 "+r.workdir, err.Error(), "")
	}
	// 上次失败留下的解密数据先删掉，重新解开（加密的下载文件保留，不用重新下载）。
	if err := os.RemoveAll(r.dataDir); err != nil {
		return fail(r.Screen, "无法清理上次留下的文件 "+r.dataDir, err.Error(), "")
	}
	return nil
}

func (r *run) download(ctx context.Context) error {
	s := r.Screen
	if err := r.prepareWorkdir(""); err != nil {
		return err
	}
	r.bundlePath = filepath.Join(r.workdir, "bundle")
	r.downloaded = true
	total := int64(0)
	if r.plan != nil {
		total = r.plan.Sizes.Bundle
	}
	p := s.StartProgress("下载迁移包", total, "下载停住了：网络可能断开了，会自动从断点继续")
	err := r.client().Download(ctx, r.opts.Link, r.bundlePath, func(done, all int64) {
		if all > 0 {
			p.SetTotal(all)
		}
		p.Set(done)
	})
	p.Done(err)
	if err != nil {
		var ce *transfer.ConnError
		if errors.As(err, &ce) {
			return fail(s, ce.Reason, "", ce.Hint)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fail(s, "下载迁移包失败："+err.Error(), "", "检查两台服务器之间的网络后重新运行，会从断点继续")
	}
	return nil
}

// unpack 解密、解开迁移包，并在改动任何东西之前检查里面的每一份数据。返回这一步的摘要。
func (r *run) unpack(ctx context.Context) (string, error) {
	s := r.Screen
	f, err := os.Open(r.bundlePath)
	if err != nil {
		return "", fail(s, "打不开迁移包 "+r.bundlePath, err.Error(), "")
	}
	st, _ := f.Stat()
	p := s.StartProgress("校验并解开", st.Size(), "")
	err = bundle.Extract(f, r.opts.Link, r.dataDir, p)
	f.Close()
	p.Done(err)
	if err != nil {
		_ = os.RemoveAll(r.dataDir)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		switch {
		case errors.Is(err, crypto.ErrWrongKey):
			return "", fail(s, err.Error(), "", "")
		case errors.Is(err, crypto.ErrCorrupted), errors.Is(err, bundle.ErrUnsafe):
			hint := "在旧服务器重新运行迁出"
			if r.downloaded {
				_ = os.Remove(r.bundlePath)
				hint = "已删除下载的文件，重新运行即可重新下载；如果再次出现，请在旧服务器重新运行迁出"
			}
			return "", fail(s, err.Error(), "", hint)
		default:
			return "", fail(s, "解开迁移包失败："+err.Error(), "", "执行 df -h 检查磁盘空间")
		}
	}
	m, err := bundle.ReadManifest(r.dataDir)
	if err != nil {
		return "", fail(s, err.Error(), "", "")
	}
	r.m = m
	if r.plan == nil {
		var pl bundle.Plan
		if b, err := os.ReadFile(filepath.Join(r.dataDir, "plan.json")); err == nil && json.Unmarshal(b, &pl) == nil {
			r.plan = &pl
		}
	}
	if err := r.scan(); err != nil {
		return "", fail(s, err.Error(), "", "在旧服务器重新运行迁出")
	}
	return fmt.Sprintf("%d 个容器 · %s · 校验通过", len(m.Containers), ui.HumanBytes(st.Size())), nil
}

// scan 检查迁移包里所有的卷、目录和 Compose 文件（路径是否越界、有没有通过软链接
// 写到别处），以及挂载目录会不会盖住 DockHop 自己的文件。
func (r *run) scan() error {
	check := func(prefix string, parts int, what string, roots ...string) error {
		rc := bundle.OpenStream(r.dataDir, prefix, parts)
		defer rc.Close()
		scan := bundle.ScanArchive
		if strings.HasPrefix(prefix, "changes/") {
			scan = bundle.ScanChanges
		}
		if err := scan(rc, roots...); err != nil {
			return fmt.Errorf("%s的数据有问题，已拒绝迁入（还没有改动任何东西）：%v", what, err)
		}
		return nil
	}
	for _, v := range r.m.Volumes {
		if err := check("volumes/"+v.Name, v.Parts, "数据卷 "+v.Name+" ", "dockhop-data"); err != nil {
			return err
		}
	}
	for _, c := range r.m.Containers {
		if c.Changes == nil {
			continue
		}
		for _, p := range append(append([]string(nil), c.Changes.Paths...), c.Changes.Deleted...) {
			if !filepath.IsAbs(p) || filepath.Clean(p) == "/" || strings.Contains(p, "..") {
				return fmt.Errorf("容器 %s 里改动的路径不对：%q", c.Name, p)
			}
		}
		if len(c.Changes.Paths) > 0 {
			if err := check("changes/"+c.Name, c.Changes.Parts, "容器 "+c.Name+" 里改动", c.Changes.Paths...); err != nil {
				return err
			}
		}
	}
	for _, b := range r.m.Binds {
		if !filepath.IsAbs(b.Path) || b.Path == "/" {
			return fmt.Errorf("迁移包里的目录路径不对：%q", b.Path)
		}
		if err := check("binds/"+b.ID, b.Parts, "目录 "+b.Path+" ", strings.TrimPrefix(b.Path, "/")); err != nil {
			return err
		}
		paths := []string{b.Path}
		for _, l := range b.AllLinks() {
			if !filepath.IsAbs(l.Link) || filepath.Clean(l.Link) != l.Link || l.Link == "/" || !within(l.Target, b.Path) {
				return fmt.Errorf("迁移包里目录 %s 的软链接不对：%q → %q", b.Path, l.Link, l.Target)
			}
			paths = append(paths, l.Link)
		}
		for _, p := range paths {
			if p != "" && !spec.IsSystemPath(p) && spec.PathsOverlap(preflight.Normalize(p), preflight.Normalize(r.Home)) {
				return fmt.Errorf("挂载目录 %s 和 DockHop 存放迁移文件、回滚资料的目录 %s 重叠，迁入会把它们覆盖。请设置 DOCKHOP_HOME=其他目录后重新运行", p, r.Home)
			}
		}
	}
	for _, p := range r.m.Projects {
		if !filepath.IsAbs(p.WorkingDir) {
			return fmt.Errorf("迁移包里 Compose 项目 %s 的目录不对：%q", p.Name, p.WorkingDir)
		}
		if p.DirParts > 0 {
			if err := check("compose/"+p.Name+"/dir", p.DirParts, "Compose 项目 "+p.Name+" "); err != nil {
				return err
			}
		}
		if p.OutParts > 0 {
			roots := make([]string, len(p.Outside))
			for i, o := range p.Outside {
				roots[i] = strings.TrimPrefix(o, "/")
			}
			if err := check("compose/"+p.Name+"/outside", p.OutParts, "Compose 项目 "+p.Name+" 引用的文件", roots...); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkCompose 在改动新服务器之前，用新服务器上的 docker compose 把每个 Compose 项目的
// 配置文件读一遍。老版本读不懂新写法时在这里就停下，不用等到替换掉已有的服务、写完
// 数据之后才发现。
func (r *run) checkCompose(ctx context.Context) error {
	s := r.Screen
	var list []bundle.Project
	for _, p := range r.m.Projects {
		if has(r.d.SkipProjects, p.Name) {
			continue
		}
		if r.opts.Existing == "skip" {
			if cur, err := r.Docker.ContainersByLabel(ctx, "com.docker.compose.project="+p.Name); err == nil && len(cur) > 0 {
				continue // 会按 --existing skip 保留新服务器上已有的
			}
		}
		list = append(list, p)
	}
	if len(list) == 0 {
		return nil
	}
	if r.tool == nil {
		return fail(s, "新服务器没有安装 Docker Compose，没法启动 Compose 项目（还没有改动新服务器）", "",
			"安装 Docker Compose（Docker 官方源的 docker-compose-plugin）后重新运行，迁移包已经下载好，不用重新下载")
	}
	for _, p := range list {
		s.Note("检查 Compose 项目 " + p.Name + " 的配置文件")
		if err := r.checkProject(ctx, p); err != nil {
			hint := "检查这个项目的配置文件在新服务器的 docker compose " + r.tool.Version + " 上能不能用"
			if r.tool.Legacy() {
				hint = r.legacyHint()
			}
			return fail(s, "新服务器上的 docker compose 读不懂 Compose 项目 "+p.Name+" 的配置文件（还没有改动新服务器）", err.Error(), hint)
		}
	}
	return nil
}

// checkProject 把项目目录解到临时目录里，执行 docker compose config -q。引用了项目目录
// 以外的文件、或者用绝对路径引用文件时，临时目录里没有这些文件，相关的报错不算数。
func (r *run) checkProject(ctx context.Context, p bundle.Project) error {
	files := p.ConfigArgs
	if len(files) == 0 {
		files = p.ConfigFiles
	}
	stage := filepath.Join(r.workdir, "check-"+p.Name)
	_ = os.RemoveAll(stage)
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return nil
	}
	defer os.RemoveAll(stage)
	if p.DirParts > 0 {
		rc := bundle.OpenStream(r.dataDir, "compose/"+p.Name+"/dir", p.DirParts)
		err := bundle.ExtractTree(rc, stage)
		rc.Close()
		if err != nil {
			r.Log.Printf("没能解开 Compose 项目 %s 的文件来检查：%v", p.Name, err)
			return nil
		}
	}
	var args []string
	for _, f := range files {
		if filepath.IsAbs(f) {
			if !within(f, p.WorkingDir) {
				r.Log.Printf("Compose 项目 %s 的配置文件 %s 不在项目目录里，跳过检查", p.Name, f)
				return nil
			}
			f, _ = filepath.Rel(p.WorkingDir, f)
		}
		f = filepath.Join(stage, f)
		if !within(f, stage) {
			r.Log.Printf("Compose 项目 %s 的配置文件 %s 不在项目目录里，跳过检查", p.Name, f)
			return nil
		}
		args = append(args, f)
	}
	err := r.tool.Exec(ctx, compose.Project{Name: p.Name, WorkingDir: stage, Files: args}, r.Log.Printf, "config", "-q")
	if err == nil {
		return nil
	}
	msg := err.Error()
	for _, path := range append(append([]string{p.WorkingDir + "/"}, p.Outside...), p.Skipped...) {
		if strings.Contains(msg, path) {
			r.Log.Printf("检查 Compose 项目 %s 时的报错涉及临时目录里没有的文件，不算数", p.Name)
			return nil
		}
	}
	msg = strings.TrimPrefix(msg, "config -q 失败：")
	return errors.New(strings.ReplaceAll(msg, stage, p.WorkingDir))
}

// ---------- 加载镜像 ----------

func (r *run) loadImages(ctx context.Context) error {
	s := r.Screen
	rc := bundle.OpenStream(r.dataDir, "images", r.m.Images.Parts)
	p := s.StartProgress("加载镜像", r.m.Images.Bytes, "镜像较大时需要几分钟")
	err := r.Docker.LoadImages(ctx, io.TeeReader(rc, p))
	rc.Close()
	p.Done(err)
	if err != nil {
		return fail(s, "加载镜像失败："+err.Error(), "", "执行 df -h 检查磁盘空间，并确认 Docker 运行正常")
	}
	_ = os.RemoveAll(filepath.Join(r.dataDir, "images")) // 用完就删，省磁盘
	refs := make([]string, 0, len(r.m.Images.Refs))
	for ref := range r.m.Images.Refs {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	for _, ref := range refs {
		if !strings.Contains(ref, "@sha256:") {
			if _, err := r.Docker.InspectImage(ctx, ref); err != nil {
				return fail(s, "镜像 "+ref+" 没有加载成功", err.Error(), "")
			}
			if r.helperImage == "" {
				r.helperImage = ref // 用名字：containerd 存储里按多平台镜像的 ID 创建容器会选错平台
			}
			continue
		}
		// digest 固定的镜像：导出文件里没有仓库清单，加载后按摘要找不到（实验 3、6）。
		if !r.d.UseBundleImage(ref) {
			s.Note("按摘要补齐镜像清单 · " + ref)
			pctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			err := r.Docker.PullImage(pctx, ref)
			cancel()
			if err == nil {
				continue
			}
			s.Warn("镜像 %s 没能按摘要补齐（%v），改用迁移包里的镜像", ref, err)
		}
		// 加载后的镜像只能按 ID 找到：旧版 Docker 的 ID 是配置的摘要，新版（containerd 存储）
		// 的 ID 就是引用里的摘要。
		id := ""
		for _, cand := range []string{r.m.Images.Refs[ref], "sha256:" + ref[strings.LastIndex(ref, "@sha256:")+8:]} {
			if img, err := r.Docker.InspectImage(ctx, cand); err == nil {
				id = img.ID
				break
			}
		}
		if id == "" {
			return fail(s, "镜像 "+ref+" 没有加载成功", "", "")
		}
		as := preflight.StripDigest(ref)
		repo, tag := splitTag(as)
		if err := r.Docker.TagImage(ctx, id, repo, tag); err != nil {
			return fail(s, "给镜像 "+ref+" 加名字失败", err.Error(), "")
		}
		r.imageAs[ref] = as
		if r.helperImage == "" {
			r.helperImage = as
		}
	}
	s.End(fmt.Sprintf("%d 个镜像 · %s", len(refs), ui.HumanBytes(r.m.Images.Bytes)))
	return nil
}

func splitTag(ref string) (string, string) {
	i := strings.LastIndex(ref, ":")
	if i < 0 || strings.Contains(ref[i:], "/") {
		return ref, "latest"
	}
	return ref[:i], ref[i+1:]
}

// ---------- 备份已有服务 ----------

func (r *run) prepare(ctx context.Context) error {
	s := r.Screen
	dc := r.Docker
	existing, err := dc.Containers(ctx, true)
	if err != nil {
		return fail(s, "读取容器列表失败", err.Error(), "")
	}
	byName := map[string]docker.ContainerSummary{}
	projects := map[string][]docker.ContainerSummary{}
	for _, c := range existing {
		byName[c.Name()] = c
		if p := c.Labels["com.docker.compose.project"]; p != "" {
			projects[p] = append(projects[p], c)
		}
	}
	policy := r.opts.Existing
	if policy == "" {
		policy = "replace" // 检查时问过了，或者用了 --skip-check
	}

	// 要迁入的内容：去掉检查时选择跳过的，以及 --existing skip 时已经有的。
	r.targets = map[string]bool{}
	var replaceProjects []string
	for _, p := range r.m.Projects {
		if has(r.d.SkipProjects, p.Name) {
			continue
		}
		if len(projects[p.Name]) > 0 {
			switch policy {
			case "skip":
				r.d.SkipProjects = append(r.d.SkipProjects, p.Name)
				r.d.Skipped = append(r.d.Skipped, preflight.Skip{Project: p.Name, Reason: "新服务器上已有同名项目，按 --existing skip 保留"})
				continue
			case "fail":
				return fail(s, "新服务器上已经有同名的 Compose 项目 "+p.Name+"（--existing fail 不允许替换），没有改动任何东西", "", "删除这个项目的容器，或者去掉 --existing fail")
			}
			replaceProjects = append(replaceProjects, p.Name)
		}
		r.projects = append(r.projects, p)
	}
	for _, mc := range r.m.Containers {
		if mc.Project != "" && has(r.d.SkipProjects, mc.Project) || has(r.d.SkipContainers, mc.Name) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(r.dataDir, "containers", mc.Name+".json"))
		if err != nil {
			return fail(s, "迁移包里缺少容器 "+mc.Name+" 的配置", err.Error(), "在旧服务器重新运行迁出")
		}
		var c docker.Container
		if err := json.Unmarshal(raw, &c); err != nil {
			return fail(s, "容器 "+mc.Name+" 的配置读不懂", err.Error(), "")
		}
		inProject := mc.Project != "" && r.projectRestored(mc.Project)
		if !inProject {
			if _, ok := byName[mc.Name]; ok {
				switch policy {
				case "skip":
					r.d.SkipContainers = append(r.d.SkipContainers, mc.Name)
					r.d.Skipped = append(r.d.Skipped, preflight.Skip{Container: mc.Name, Reason: "新服务器上已有同名容器，按 --existing skip 保留"})
					continue
				case "fail":
					return fail(s, "新服务器上已经有同名容器 "+mc.Name+"（--existing fail 不允许替换），没有改动任何东西", "", "删除这个容器，或者去掉 --existing fail")
				}
				r.targets[mc.Name] = true
			}
		}
		r.items = append(r.items, &citem{m: mc, raw: raw, c: &c})
	}
	for _, v := range r.m.Volumes {
		if v.Anonymous {
			r.anonVolumes[v.Name] = v
		}
	}
	if len(r.items) == 0 {
		return fail(s, "按检查时的选择，没有要迁入的容器了", "", "")
	}

	// 新服务器上正在运行、和要写入的数据共用卷或目录的容器：要先临时停止，否则数据不一致。
	volumes, binds := r.dataTargets()
	self, _ := os.Hostname()
	ignore := map[string]bool{}
	for _, n := range strings.Split(r.Getenv("DOCKHOP_TEST_IGNORE"), ",") {
		ignore[strings.TrimSpace(n)] = true
	}
	var writers []writer
	for _, c := range existing {
		name := c.Name()
		if c.State != "running" && c.State != "paused" && c.State != "restarting" {
			continue
		}
		if r.targets[name] || has(replaceProjects, c.Labels["com.docker.compose.project"]) || ignore[name] || (len(self) >= 12 && strings.HasPrefix(c.ID, self)) {
			continue
		}
		full, _, err := dc.InspectContainer(ctx, c.ID)
		if err != nil || !usesData(full, volumes, binds) {
			continue
		}
		writers = append(writers, writer{summary: c, full: full})
	}
	r.buildGroups(writers)

	// 开始事务：之后的每一处改动都先记日志。
	if err := os.RemoveAll(r.rbDir); err != nil {
		return fail(s, "无法清理回滚资料目录 "+r.rbDir, err.Error(), "")
	}
	j, err := openJournal(r.rbDir)
	if err != nil {
		return fail(s, "无法创建事务日志，为了安全没有改动任何东西", err.Error(), "检查 "+r.Home+" 是否可写")
	}
	r.j = j
	helper := ""
	if dc.Local() {
		if helper, err = prepareHelper(r.Home); err != nil {
			s.Warn("准备清空数据卷用的程序失败：%v", err)
			helper = ""
		}
	}
	r.vio = &volumeIO{dc: dc, image: r.helperImage, helperBin: helper}
	r.undo.vio = r.vio

	stopped := 0
	for _, w := range writers {
		name := w.summary.Name()
		e := Entry{Op: opStop, Name: name, ID: w.summary.ID, State: stateOf(w.full), Group: r.groupOf["w:"+name]}
		if err := r.stopContainer(ctx, e); err != nil {
			return err
		}
		r.shared = append(r.shared, e)
		s.Detail("临时停止共用数据的容器：%s", name)
		stopped++
	}

	// 已有的同名独立容器：停止、改名备份、断开网络（释放固定 IP），失败时改回来。
	backedUp := 0
	for _, it := range r.items {
		if !r.targets[it.m.Name] {
			continue
		}
		r.in("c:" + it.m.Name)
		c, _, err := dc.InspectContainer(ctx, it.m.Name)
		if err != nil {
			return fail(s, "读取已有容器 "+it.m.Name+" 失败", err.Error(), "")
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
			return fail(s, err.Error(), "", "")
		}
		if err := dc.RenameContainer(ctx, c.ID, backup); err != nil {
			return fail(s, "没能先备份已有的容器 "+it.m.Name+"（改名失败）", err.Error(), "")
		}
		mode := c.HostConfig.NetworkMode
		if mode != "host" && mode != "none" && !strings.HasPrefix(mode, "container:") {
			for n := range c.NetworkSettings.Networks {
				if err := dc.DisconnectNetwork(ctx, n, c.ID); err != nil && !docker.IsNotFound(err) {
					return fail(s, "没能释放已有容器 "+it.m.Name+" 占用的网络地址", err.Error(), "")
				}
			}
		}
		backedUp++
	}

	// 已有的同名 Compose 项目：记下每个容器的完整配置，停止，把有改动的存成回滚镜像，再删除。
	for _, p := range replaceProjects {
		r.in("p:" + p)
		if err := r.backupProject(ctx, p, projects[p]); err != nil {
			return err
		}
		backedUp += len(projects[p])
	}
	r.in("")
	if r.testFail("prepare") {
		return fail(s, "（测试）模拟备份已有服务时失败", "", "")
	}
	switch {
	case stopped == 0 && backedUp == 0:
		s.End("新服务器上没有同名的服务")
	default:
		s.End(fmt.Sprintf("备份 %d 个同名容器 · 临时停止 %d 个共用数据的容器", backedUp, stopped))
	}
	return nil
}

func (r *run) projectRestored(name string) bool {
	for _, p := range r.projects {
		if p.Name == name {
			return true
		}
	}
	return false
}

func has(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
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
		return fail(r.Screen, err.Error(), "", "")
	}
	if e.State == "paused" {
		if err := r.Docker.UnpauseContainer(ctx, e.ID); err != nil {
			return fail(r.Screen, "容器 "+e.Name+" 没能从暂停中恢复，无法停止", err.Error(), "")
		}
	}
	if err := r.Docker.StopContainer(ctx, e.ID, nil); err != nil {
		return fail(r.Screen, "没能停止新服务器上的容器 "+e.Name, err.Error(), "")
	}
	return nil
}

func (r *run) backupProject(ctx context.Context, project string, list []docker.ContainerSummary) error {
	s, dc := r.Screen, r.Docker
	var old []OldContainer
	for _, c := range list {
		full, raw, err := dc.InspectContainer(ctx, c.ID)
		if err != nil {
			return fail(s, "读取已有的 Compose 容器 "+c.Name()+" 失败", err.Error(), "")
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
		return fail(s, err.Error(), "", "")
	}
	s.Detail("备份新服务器上已有的 Compose 项目：%s", project)
	images := map[string]string{}
	for _, o := range old {
		if o.State == "paused" {
			_ = dc.UnpauseContainer(ctx, o.ID)
		}
		if err := dc.StopContainer(ctx, o.ID, nil); err != nil {
			return fail(s, "没能停止已有的 Compose 容器 "+o.Name, err.Error(), "")
		}
		// 读不到容器里有没有改动时不能当作没有：接下来要删除这个容器，没备份的改动就找不回来了。
		changes, err := dc.ContainerChanges(ctx, o.ID)
		if err != nil {
			return fail(s, "读不到已有的 Compose 容器 "+o.Name+" 里有没有改动，为了安全没有删除它", err.Error(), "")
		}
		full, _, err := dc.InspectContainer(ctx, o.ID)
		if err != nil {
			return fail(s, "读取已有的 Compose 容器 "+o.Name+" 失败，为了安全没有删除它", err.Error(), "")
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
			return fail(s, "没能备份已有的 Compose 容器 "+o.Name, err.Error(), "")
		}
		images[o.ID] = "dockhop-rollback:" + tag
	}
	if err := r.j.add(Entry{Op: opOldImages, Name: project, Images: images}); err != nil {
		return fail(s, err.Error(), "", "")
	}
	for _, o := range old {
		// 不删匿名卷：回滚重建旧容器时还要用。
		if err := dc.RemoveContainer(ctx, o.ID, true); err != nil && !docker.IsNotFound(err) {
			return fail(s, "没能移开已有的 Compose 容器 "+o.Name, err.Error(), "")
		}
	}
	return nil
}

// dataTargets 是这次要写入的数据卷和目录。
func (r *run) dataTargets() (map[string]bool, []string) {
	vols := map[string]bool{}
	var binds []string
	usedV, usedB := r.usedData()
	for _, v := range r.m.Volumes {
		if !v.Anonymous && usedV[v.Name] && !r.d.KeepVolume(v.Name) {
			vols[v.Name] = true
		}
	}
	for _, b := range r.m.Binds {
		if usedB[b.Path] && !spec.IsSystemPath(b.Path) {
			binds = append(binds, b.Path)
		}
	}
	return vols, binds
}

// usedData 是要迁入的容器用到的卷和目录（被跳过的容器独占的数据不写入）。
func (r *run) usedData() (map[string]bool, map[string]bool) {
	vols, binds := map[string]bool{}, map[string]bool{}
	bindOf := map[string]string{}
	for _, b := range r.m.Binds {
		bindOf[b.Path] = b.Path
		for _, l := range b.AllLinks() {
			bindOf[l.Link] = b.Path
		}
	}
	for _, it := range r.items {
		for _, m := range it.c.Mounts {
			switch m.Type {
			case "volume":
				vols[m.Name] = true
			case "bind":
				for src, real := range bindOf {
					if m.Source == src || strings.HasPrefix(m.Source, src+"/") {
						binds[real] = true
					}
				}
			}
		}
	}
	for _, p := range r.projects {
		for _, b := range r.m.Binds {
			if within(p.WorkingDir, b.Path) {
				binds[b.Path] = true // 项目目录在这个挂载目录里面
			}
		}
	}
	return vols, binds
}

func usesData(c *docker.Container, vols map[string]bool, binds []string) bool {
	for _, m := range c.Mounts {
		switch m.Type {
		case "volume":
			if vols[m.Name] {
				return true
			}
		case "bind":
			for _, b := range binds {
				if spec.PathsOverlap(m.Source, b) {
					return true
				}
			}
		}
	}
	return false
}

// ---------- 恢复数据 ----------

func (r *run) restoreData(ctx context.Context) error {
	s, dc := r.Screen, r.Docker
	vols, binds := r.dataTargets()
	nv, nb := 0, 0
	for _, v := range r.m.Volumes {
		if !vols[v.Name] {
			if !v.Anonymous && r.d.KeepVolume(v.Name) {
				s.Detail("按检查时的选择保留已有的数据卷 %s", v.Name)
			}
			continue
		}
		var meta struct {
			Driver  string            `json:"Driver"`
			Options map[string]string `json:"Options"`
			Labels  map[string]string `json:"Labels"`
		}
		if b, err := os.ReadFile(filepath.Join(r.dataDir, "volumes", v.Name+".json")); err == nil {
			_ = json.Unmarshal(b, &meta)
		}
		r.in("v:" + v.Name)
		cur, err := dc.InspectVolume(ctx, v.Name)
		existed := err == nil
		if err != nil && !docker.IsNotFound(err) {
			return fail(s, "读取数据卷 "+v.Name+" 失败", err.Error(), "")
		}
		entry := Entry{Op: opVolume, Name: v.Name, Existed: existed}
		if existed {
			if volumeDriver(cur.Driver) != volumeDriver(meta.Driver) || !sameLabels(cur.Options, meta.Options) {
				return fail(s, "新服务器上已有同名、但存储方式不同的数据卷 "+v.Name+"，拒绝覆盖", "",
					"确认新服务器上的这个卷可以删除后，执行 docker volume rm "+v.Name+" 再重试")
			}
			// 先把新服务器上原有的数据备份到回滚资料里。
			entry.Backup = r.j.file("volume")
			s.Note("备份新服务器上原有的数据卷 · " + v.Name)
			if err := r.saveVolume(ctx, v.Name, entry.Backup); err != nil {
				return fail(s, "没能先备份新服务器上原有的数据卷 "+v.Name, err.Error(), "执行 df -h 检查磁盘空间")
			}
		}
		if err := r.j.add(entry); err != nil {
			return fail(s, err.Error(), "", "")
		}
		if !existed {
			if err := dc.CreateVolume(ctx, v.Name, meta.Driver, meta.Options, meta.Labels); err != nil {
				return fail(s, "数据卷 "+v.Name+" 创建失败", err.Error(), "不是 local 驱动的卷，需要先在新服务器安装对应的卷插件")
			}
		}
		rc := bundle.OpenStream(r.dataDir, "volumes/"+v.Name, v.Parts)
		p := s.StartProgress("卷 "+v.Name, v.Bytes, "")
		var werr error
		if existed {
			werr = r.vio.replace(ctx, v.Name, io.TeeReader(rc, p))
		} else {
			werr = r.vio.write(ctx, v.Name, io.TeeReader(rc, p))
		}
		rc.Close()
		p.Done(werr)
		if werr == nil && r.testFail("volume:"+v.Name) {
			werr = errors.New("（测试）模拟写入失败")
		}
		if werr != nil {
			return fail(s, "数据卷 "+v.Name+" 写入失败", werr.Error(), "执行 df -h 检查磁盘空间")
		}
		_ = os.RemoveAll(filepath.Join(r.dataDir, "volumes", v.Name))
		nv++
	}
	for _, b := range r.m.Binds {
		if spec.IsSystemPath(b.Path) {
			s.Detail("宿主机系统路径只重建挂载、不覆盖：%s", b.Path)
			continue
		}
		if !has(binds, b.Path) {
			continue
		}
		r.in("b:" + b.Path)
		if err := r.restoreBind(ctx, b); err != nil {
			return fail(s, "目录 "+b.Path+" 恢复失败", err.Error(), "")
		}
		nb++
	}
	for _, p := range r.projects {
		r.in("p:" + p.Name)
		if err := r.installProject(p); err != nil {
			return fail(s, "Compose 项目 "+p.Name+" 的文件没能放回原来的位置", err.Error(), "")
		}
	}
	r.in("")
	if r.testFail("after-data") {
		return fail(s, "（测试）模拟恢复数据之后失败", "", "")
	}
	r.testPause(ctx, "after-data")
	if ctx.Err() != nil {
		return ctx.Err()
	}
	summary := fmt.Sprintf("%d 个卷 · %d 个目录", nv, nb)
	if len(r.projects) > 0 {
		summary += fmt.Sprintf(" · %d 个 Compose 项目", len(r.projects))
	}
	s.End(summary)
	return nil
}

func volumeDriver(d string) string {
	if d == "" {
		return "local"
	}
	return d
}

func (r *run) saveVolume(ctx context.Context, name, path string) error {
	rc, err := r.vio.read(ctx, name)
	if err != nil {
		return err
	}
	defer rc.Close()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, rc)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (r *run) restoreBind(ctx context.Context, b bundle.Bind) error {
	parent := filepath.Dir(b.Path)
	if err := r.mkdirAll(parent, 0o755); err != nil {
		return err
	}
	stage := siblingName(b.Path, "stage")
	_ = os.RemoveAll(stage)
	if err := os.Mkdir(stage, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	rc := bundle.OpenStream(r.dataDir, "binds/"+b.ID, b.Parts)
	p := r.Screen.StartProgress("目录 "+b.Path, b.Bytes, "")
	err := bundle.ExtractTree(io.TeeReader(rc, p), stage)
	rc.Close()
	p.Done(err)
	if err == nil && r.testFail("bind:"+b.Path) {
		err = errors.New("（测试）模拟写入失败")
	}
	if err != nil {
		return err
	}
	staged := filepath.Join(stage, strings.TrimPrefix(b.Path, "/"))
	st, err := os.Lstat(staged)
	if err != nil {
		return fmt.Errorf("迁移包里缺少 %s", b.Path)
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("迁移包里的 %s 是软链接，拒绝使用", b.Path)
	}
	if err := r.replaceDir(staged, b.Path); err != nil {
		return err
	}
	_ = os.RemoveAll(filepath.Join(r.dataDir, "binds", b.ID))
	for _, l := range b.AllLinks() {
		if err := r.restoreLink(l.Link, l.Target); err != nil {
			return err
		}
	}
	return nil
}

// restoreLink 源端挂载的是软链接时，数据已经恢复到真实路径，在原路径重建同一个链接。
func (r *run) restoreLink(link, target string) error {
	if link == "" {
		return nil
	}
	if st, err := os.Lstat(link); err == nil {
		if st.Mode()&os.ModeSymlink != 0 {
			if cur, _ := filepath.EvalSymlinks(link); cur == target {
				return nil
			}
			dest, _ := os.Readlink(link)
			return fmt.Errorf("新服务器上已有软链接 %s，但它指向 %s（应该指向 %s）", link, dest, target)
		}
		return fmt.Errorf("新服务器上的 %s 已存在、不是软链接，没法让它指向迁移过来的 %s", link, target)
	}
	if err := r.mkdirAll(filepath.Dir(link), 0o755); err != nil {
		return err
	}
	if err := r.j.add(Entry{Op: opLink, Path: link}); err != nil {
		return err
	}
	return os.Symlink(target, link)
}

// installProject 把 Compose 项目目录和它引用的目录外文件放回原来的位置。逐个文件
// 安装（合并），不删除新服务器上这个目录里原有的其他文件；DockHop 自己的目录不动。
func (r *run) installProject(p bundle.Project) error {
	skip := func(path string) bool { return within(path, r.Home) || within(path, r.workdir) }
	if p.DirParts > 0 {
		if err := r.extractAndInstall("compose/"+p.Name+"/dir", p.DirParts, p.WorkingDir, skip); err != nil {
			return err
		}
	} else if err := r.mkdirAll(p.WorkingDir, 0o755); err != nil {
		return err
	}
	if p.OutParts > 0 {
		if err := r.extractAndInstall("compose/"+p.Name+"/outside", p.OutParts, "/", skip); err != nil {
			return err
		}
	}
	return nil
}

func (r *run) extractAndInstall(prefix string, parts int, dest string, skip func(string) bool) error {
	stage := filepath.Join(r.workdir, "stage-"+strings.ReplaceAll(prefix, "/", "-"))
	_ = os.RemoveAll(stage)
	if err := os.Mkdir(stage, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	rc := bundle.OpenStream(r.dataDir, prefix, parts)
	err := bundle.ExtractTree(rc, stage)
	rc.Close()
	if err != nil {
		return err
	}
	return r.installTree(stage, dest, skip)
}

// ---------- 启动服务 ----------

func (r *run) startServices(ctx context.Context) error {
	s := r.Screen
	// 先建好所有独立网络，再启动 Compose 项目：Compose 自动分配网段时会占用第一个空闲网段，
	// 可能正是独立网络在旧服务器上用的网段。
	for _, n := range r.m.Networks {
		g := r.groupByID(r.groupOf["n:"+n])
		if g == nil || g.reason != "" || n == "bridge" || n == "host" || n == "none" {
			continue // 没有要迁入的容器用它
		}
		r.in("n:" + n)
		raw, err := os.ReadFile(filepath.Join(r.dataDir, "networks", n+".json"))
		if err != nil {
			r.groupFailed(g, "迁移包里缺少网络 "+n+" 的配置："+err.Error(), "")
			continue
		}
		var meta struct {
			Labels map[string]string `json:"Labels"`
		}
		_ = json.Unmarshal(raw, &meta)
		if proj := meta.Labels["com.docker.compose.project"]; proj != "" && r.projectRestored(proj) && meta.Labels["com.docker.compose.network"] != "" {
			continue // 交给 Compose 按项目文件创建
		}
		if err := r.ensureNetwork(ctx, n, raw); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.groupFailed(g, err.Error(), "检查是否和新服务器上已有网络的网段冲突")
		}
	}

	// 一组没能启动时，接着启动其他组，最后再决定怎么处理。
	total, n := 0, 0
	for _, it := range r.items {
		if it.m.Project == "" || !r.projectRestored(it.m.Project) {
			total++
		}
	}
	for _, g := range r.groups {
		if g.reason != "" {
			continue
		}
		if err := r.startGroup(ctx, g, &n, total); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.groupFailed(g, err.Error(), groupHint(err))
		}
	}
	r.in("")
	ok, failed := r.splitGroups()
	switch {
	case len(failed) == 0:
		s.End(r.countText())
		return nil
	case len(ok) == 0:
		return ErrReported
	}
	keep, err := r.decidePartial(ok, failed)
	if err != nil || !keep {
		return ErrReported
	}
	s.End(fmt.Sprintf("%s 正常运行 · %s 没能启动", labels(ok), labels(failed)))
	return nil
}

// startError 是一组服务没能启动的原因和建议。
type startError struct{ reason, hint string }

func (e *startError) Error() string { return e.reason }

func groupHint(err error) string {
	var se *startError
	if errors.As(err, &se) {
		return se.hint
	}
	return ""
}

// groupFailed 记下一组没能启动，并显示原因。
func (r *run) groupFailed(g *group, reason, hint string) {
	g.reason = reason
	if len(r.groups) > 1 {
		r.Log.Printf("这一组没能启动：%s", g.label())
	}
	_ = fail(r.Screen, reason, "", hint)
}

// startGroup 启动一组服务：先启动 Compose 项目，再按依赖顺序创建、启动独立容器。
func (r *run) startGroup(ctx context.Context, g *group, n *int, total int) error {
	for _, p := range g.projects {
		r.in("p:" + p.Name)
		if err := r.startProject(ctx, p); err != nil {
			hint := "执行 docker compose -p " + p.Name + " logs 查看服务日志"
			if r.tool != nil && r.tool.Legacy() {
				hint = r.legacyHint()
			}
			return &startError{"Compose 项目 " + p.Name + " 启动失败：" + err.Error(), hint}
		}
	}
	var standalone []*citem
	for _, it := range g.items {
		if it.m.Project == "" || !r.projectRestored(it.m.Project) {
			standalone = append(standalone, it)
		}
	}
	for _, it := range dependencyOrder(standalone, r.nameOfOld) {
		r.in("c:" + it.m.Name)
		*n++
		r.Screen.Count("启动容器", *n, total, it.m.Name)
		if err := r.createContainer(ctx, it); err != nil {
			return &startError{err.Error(), ""}
		}
		if err := r.transplantAnonymous(ctx, it, it.newID); err != nil {
			return &startError{err.Error(), ""}
		}
		if err := r.applyChanges(ctx, it); err != nil {
			return &startError{err.Error(), ""}
		}
		if it.running() {
			if err := r.startAndVerify(ctx, it, it.newID); err != nil {
				return &startError{err.Error(), "执行 docker logs " + it.m.Name + " 查看应用日志"}
			}
		}
		if r.testFail("container:" + it.m.Name) {
			return &startError{"（测试）模拟容器 " + it.m.Name + " 失败", ""}
		}
	}
	return nil
}

func (r *run) legacyHint() string {
	return "新服务器的 docker-compose " + r.tool.Version + " 是老版本，可能读不懂这个项目配置文件里的新写法。" +
		"安装新版 Docker Compose（Docker 官方源的 docker-compose-plugin，或者把 GitHub 上下载的 docker-compose 放到 " +
		"/usr/local/lib/docker/cli-plugins/）后重新运行，迁移包已经下载好，不用重新下载"
}

func (r *run) startProject(ctx context.Context, p bundle.Project) error {
	if r.tool == nil {
		return errors.New("新服务器没有安装 docker compose")
	}
	var members []*citem
	for _, it := range r.items {
		if it.m.Project == p.Name {
			members = append(members, it)
		}
	}
	// -f 按旧服务器上的原样写法传（在项目目录里运行），容器标签里记的配置文件才和原来一样。
	files := p.ConfigArgs
	if len(files) == 0 {
		files = p.ConfigFiles
	}
	cp := compose.Project{Name: p.Name, WorkingDir: p.WorkingDir, Files: append([]string(nil), files...)}
	// 快照镜像、改名的 digest 镜像：用一个临时的覆盖文件指定镜像，不改用户自己的文件。
	override := map[string]map[string]string{}
	for _, it := range members {
		img, err := r.image(ctx, it)
		if err != nil {
			return err
		}
		if img != it.c.Config.Image {
			override[it.m.Service] = map[string]string{"image": img}
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
	args := []string{"up", "--no-start"}
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
	var start []string
	for _, it := range members {
		if err := r.transplantAnonymous(ctx, it, it.newID); err != nil {
			return err
		}
		if err := r.applyChanges(ctx, it); err != nil {
			return err
		}
		if it.running() {
			start = append(start, it.m.Service)
		}
	}
	if len(start) > 0 {
		if err := r.tool.Exec(ctx, cp, r.Log.Printf, append([]string{"start"}, uniq(start)...)...); err != nil {
			return err
		}
	}
	for _, it := range members {
		if !it.running() {
			continue
		}
		if err := r.waitReady(ctx, it.m.Name, it.newID); err != nil {
			return err
		}
		if it.c.State.Paused {
			if err := r.Docker.PauseContainer(ctx, it.newID); err != nil {
				return fmt.Errorf("没能把 %s 恢复成暂停状态：%v", it.m.Name, err)
			}
		}
	}
	if r.testFail("project:" + p.Name) {
		return errors.New("（测试）模拟项目失败")
	}
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

func (r *run) countText() string {
	running, paused, stopped := 0, 0, 0
	for _, it := range r.items {
		switch {
		case it.c.State.Paused:
			paused++
		case it.running():
			running++
		default:
			stopped++
		}
	}
	t := fmt.Sprintf("%d 个容器（运行 %d / 暂停 %d / 停止 %d）", len(r.items), running, paused, stopped)
	if len(r.projects) > 0 {
		t += fmt.Sprintf(" · %d 个 Compose 项目", len(r.projects))
	}
	return t
}

// ---------- 提交 ----------

func (r *run) commit(ctx context.Context) error {
	s := r.Screen
	if ok, failed := r.splitGroups(); len(failed) > 0 {
		return r.commitPartial(ctx, ok, failed)
	}
	// 先把临时停下的共用数据的容器恢复；恢复不了就全部删除。
	for i := len(r.shared) - 1; i >= 0; i-- {
		e := r.shared[i]
		if err := restoreState(ctx, r.Docker, e.ID, e.State); err != nil {
			return fail(s, "新服务已验证，但临时停下的容器 "+e.Name+" 没能恢复", err.Error(), "")
		}
	}
	// 这是不可逆的提交点：之后不再删除。
	if err := r.j.add(Entry{Op: opCommit}); err != nil {
		return fail(s, err.Error(), "", "")
	}
	r.committed = true
	entries, err := readJournal(r.rbDir)
	if err != nil {
		r.cleanupLeft = []string{"事务日志读不了（" + err.Error() + "），回滚资料没有删除"}
	} else {
		r.cleanupLeft = r.undo.cleanupCommitted(ctx, entries)
	}
	if len(r.cleanupLeft) == 0 {
		r.j.close()
		if err := os.RemoveAll(r.rbDir); err != nil {
			r.cleanupLeft = append(r.cleanupLeft, r.rbDir)
		}
	}
	r.clearDone()
	if len(r.cleanupLeft) > 0 {
		s.Warn("服务和数据都已迁入，但有些回滚资料没能删除：%s", strings.Join(r.cleanupLeft, "；"))
		s.Fail()
		return ErrReported
	}
	r.warnKept(r.items)
	s.End("全部服务验证通过")
	return nil
}

// warnKept 提醒迁入的容器里需要知道的情况。
func (r *run) warnKept(items []*citem) {
	for _, it := range items {
		switch {
		case it.m.OldImage != "":
			r.Screen.Warn("%s 用的是旧版本镜像（旧服务器上的镜像名 %s 已经指向别的版本），迁入后保持原样，镜像名为 %s", it.m.Name, it.c.Config.Image, it.img)
		case it.m.Changes != nil:
			r.Screen.Warn("%s 里没挂载出来的改动已一起迁入；以后用新镜像重建这个容器时，这些改动会丢失", it.m.Name)
		}
	}
}

// ---------- 失败和收尾 ----------

func (r *run) rollback(ctx context.Context) {
	if r.Screen.FailedStage() == "" {
		r.Screen.Fail()
	}
	r.Screen.Println("迁入没有完成，正在删除这次迁入的内容，让新服务器回到迁入前的样子……")
	entries, err := readJournal(r.rbDir)
	if err != nil {
		r.rollbackIssues = []string{"事务日志读不了：" + err.Error()}
		return
	}
	r.rollbackIssues = r.undo.rollback(ctx, entries)
	r.rollbackDone = true
	if len(r.rollbackIssues) == 0 {
		r.j.close()
		if err := os.RemoveAll(r.rbDir); err != nil {
			r.Log.Printf("回滚资料目录没能删除：%v", err)
		}
		r.cleanHelpers(ctx)
	}
}

func (r *run) cleanHelpers(ctx context.Context) {
	removeHelpers(ctx, r.Docker)
}

// cleanFiles 删除工作文件：成功时全部删除（--keep-files 时保留）；失败时只删
// 解密后的数据，保留加密的下载文件，重试时不用重新下载。
func (r *run) cleanFiles(success bool) {
	if r.workdir == "" {
		return
	}
	if success && r.opts.KeepFiles {
		r.Screen.Println("按 --keep-files 保留了下载和解开的文件：" + r.workdir)
		return
	}
	_ = os.RemoveAll(r.dataDir)
	if success {
		if err := os.RemoveAll(r.workdir); err != nil {
			r.Screen.Warn("临时文件没能删除，请手动删除：%s", r.workdir)
		}
	}
	removeHelpers(context.Background(), r.Docker)
}

func (r *run) report(ctx context.Context, err error) error {
	interrupted := ctx.Err() != nil
	success := err == nil && !interrupted
	if !success {
		r.Screen.Fail()
	}
	var lines []string
	if r.opts.CheckOnly {
		rep := ui.Report{Title: "迁入前检查", Elapsed: time.Since(r.started), Success: success}
		if success {
			rep.Result = "✅ 迁入前检查通过"
			if r.plan == nil {
				rep.Result = "⚠️ 旧服务器没有提供迁移计划，没法检查"
			}
		} else {
			rep.Result = "❌ 检查没有通过"
			rep.LogPath = r.Log.Path()
		}
		r.Screen.PrintReport(rep)
		if success {
			return nil
		}
		return ErrReported
	}
	status := transfer.Status{Result: "failed"}
	rep := ui.Report{Title: "迁入结果", Elapsed: time.Since(r.started)}
	switch {
	case success:
		rep.Result = "✅ 迁入完成"
		lines = append(lines, "容器："+r.countText())
		if len(r.done) > 0 {
			lines = append(lines, "上次已经迁入："+strings.Join(r.done, "、"))
		}
		status.Result = "success"
	case r.partial:
		ok, failed := r.splitGroups()
		rep.Result = "⚠️ 只迁入了一部分"
		lines = append(lines, "已经迁入、正常运行："+labels(ok))
		if len(r.done) > 0 {
			lines = append(lines, "上次已经迁入："+strings.Join(r.done, "、"))
		}
		if len(r.rollbackIssues) == 0 {
			lines = append(lines, "没有迁入（已删除）："+labels(failed))
		} else {
			lines = append(lines, "没有迁入、也没能删干净："+labels(failed))
			lines = append(lines, "重要：请先不要启动它们，按下面的情况人工处理")
			for _, p := range r.rollbackIssues {
				lines = append(lines, "  · "+p)
			}
			lines = append(lines, "回滚资料："+r.rbDir+"（下次运行迁入时会提示继续删除）")
		}
		if len(r.cleanupLeft) > 0 {
			left := "有些回滚资料没能删除：" + strings.Join(r.cleanupLeft, "；")
			if len(r.rollbackIssues) == 0 {
				left += "（下次运行迁入时会接着删除）"
			}
			lines = append(lines, left)
		}
		lines = append(lines, "处理好问题后，重新运行同一条迁入命令：已经迁入的会跳过，只迁入剩下的")
		status.Result = "partial"
		status.Message = "没能启动：" + labels(failed)
	case r.committed:
		rep.Result = "⚠️ 服务和数据都已迁入，但清理没有完成"
		lines = append(lines, "待清理的回滚资料："+r.rbDir)
		status.Result = "success"
	case r.rollbackDone && len(r.rollbackIssues) == 0:
		if interrupted {
			rep.Result = "⚠️ 迁入已中断，迁入的内容已全部删除"
		} else {
			rep.Result = "❌ 迁入失败，迁入的内容已全部删除"
		}
		lines = append(lines, "新服务器：回到了迁入前的样子，原来的服务和数据都在")
	case r.rollbackDone || len(r.rollbackIssues) > 0:
		if interrupted {
			rep.Result = "⚠️ 迁入已中断，迁入的内容没能全部删除"
		} else {
			rep.Result = "⚠️ 迁入失败，迁入的内容没能全部删除"
		}
		lines = append(lines, "重要：请先不要启动相关容器，按下面的情况人工处理")
		for _, p := range r.rollbackIssues {
			lines = append(lines, "  · "+p)
		}
		lines = append(lines, "回滚资料："+r.rbDir+"（下次运行迁入时会提示继续删除）")
	case interrupted:
		rep.Result = "⚠️ 迁入已中断（还没有改动新服务器）"
	default:
		rep.Result = "❌ 迁入失败（还没有改动新服务器）"
	}
	if f := r.Screen.Failure(); f != nil && status.Message == "" {
		status.Message = f.Reason
	}
	lines = append(lines, r.d.Summary()...)
	if !success && r.downloaded {
		if _, err := os.Stat(r.bundlePath); err == nil {
			lines = append(lines, "下载的迁移包（加密）保留在 "+r.bundlePath+"，重新运行时不用重新下载")
		}
	}
	rep.Lines = lines
	rep.Success = success
	if !success {
		rep.LogPath = r.Log.Path()
		rep.LogTail = r.Log.Tail(15)
	}
	// 通知旧服务器：成功时它会自动结束；失败时它继续提供下载，方便重试。
	if r.m != nil || r.plan != nil {
		if err := transfer.SendStatusQuiet(ctx, r.opts.Link, status); err != nil {
			r.Log.Printf("没能通知旧服务器（可能已经关闭）：%v", err)
		}
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
	s.Error(msg, hint)
	return ErrReported
}

// ---------- 中断补救 ----------

// recoverInterrupted 检查上次迁入有没有被强制结束、留下没删除的改动。
func (r *run) recoverInterrupted(ctx context.Context) error {
	s := r.Screen
	entries, err := readJournal(r.rbDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fail(s, "上次迁入的事务日志读不了，没法自动删除上次迁入的内容", err.Error(),
			"回滚资料都保留在 "+r.rbDir+"（journal.jsonl 记着每一处改动），请人工处理后删除这个目录再重新运行")
	}
	img := ""
	if list, err := r.Docker.Containers(ctx, true); err == nil && len(list) > 0 {
		img = list[0].Image
	}
	helper := ""
	if r.Docker.Local() {
		helper, _ = prepareHelper(r.Home)
	}
	r.undo.vio = &volumeIO{dc: r.Docker, image: img, helperBin: helper}
	if committed(entries) {
		if left := r.undo.cleanupCommitted(ctx, entries); len(left) > 0 {
			s.Warn("上次迁入已经完成，但有些回滚资料没能删除：%s", strings.Join(left, "；"))
			return nil
		}
		_ = os.RemoveAll(r.rbDir)
		return nil
	}
	changes := 0
	when := ""
	for _, e := range entries {
		if e.Op == opStart {
			when = e.Time.Local().Format("01-02 15:04")
		} else {
			changes++
		}
	}
	if changes == 0 {
		return os.RemoveAll(r.rbDir)
	}
	s.Hold()
	s.Println(fmt.Sprintf("上次迁入（%s）没有完成就被结束了，新服务器上还留着它的 %d 处改动。", when, changes))
	if r.Prompt.Interactive() {
		c, err := r.Prompt.Choose([]string{"删除上次迁入的内容，回到上次迁入之前的样子（推荐）", "保留现在的样子，只删除回滚资料", "退出，我先自己检查"}, 1)
		if err != nil {
			return err
		}
		switch c {
		case 2:
			s.Warn("保留了上次迁入到一半的样子，没有删除")
			return os.RemoveAll(r.rbDir)
		case 3:
			return fail(s, "上次迁入留下的改动还没处理", "", "回滚资料在 "+r.rbDir+"；处理好之后重新运行")
		}
	} else {
		s.Println("没有可以输入的终端，直接删除上次迁入的内容。")
	}
	problems := r.undo.rollback(ctx, entries)
	if len(problems) > 0 {
		for _, p := range problems {
			s.Println("  · " + p)
		}
		return fail(s, "上次迁入的内容没能全部删除", "", "回滚资料在 "+r.rbDir+"，请人工处理后删除这个目录")
	}
	s.Println("已删除上次迁入的内容，回到了上次迁入之前的样子。")
	removeHelpers(ctx, r.Docker)
	return os.RemoveAll(r.rbDir)
}
