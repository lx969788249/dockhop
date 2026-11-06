// Package restore 是迁入：在新服务器上检查、下载迁移包、恢复数据和服务。
//
// 对新服务器的每一处改动都先记进事务日志（journal.go）。任何一步失败或被中断，
// 都按日志删除这次迁入的内容、把被替换的原有服务放回去；全部服务验证通过后才“提交”。
// 一次迁入多组服务、只有一部分没能启动时，可以选择只删除没启动的（groups.go）。
//
// 这个文件是迁入的骨架：Run 加锁、执行、失败时回滚、收尾、显示结果；run 按屏幕上的六个步骤
// 依次调用各个环节。每个环节在自己的文件里：
//
//	check.go        迁入前检查：取迁移计划，按它检查新服务器
//	download.go     下载、核对，这次迁入的工作目录
//	unpack.go       解开迁移包，检查里面的内容
//	images.go       镜像从哪里来、加载镜像
//	decide.go       改动之前的决定：替换、保留哪些，临时停下哪些
//	transaction.go  事务的开始、提交、回滚，上次中断的补救（日志 journal.go，撤销 rollback.go）
//	prepare.go      改动前的准备：停下共用数据的容器，把要替换的服务改名备份
//	data.go         恢复数据卷、目录、Compose 项目文件（文件操作 files.go，读写数据卷 volumes.go）
//	containers.go   创建容器（独立容器、Compose 项目的容器）和网络
//	startup.go      启动服务：一组一组地按顺序创建、启动，等就绪（ready.go）
//	order.go        启动顺序：找出服务之间的依赖，排出先后
//	groups.go       服务分组，只保留一部分
//	report.go       结果摘要
package restore

import (
	"context"
	"path/filepath"
	"time"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/compose"
	"github.com/lx969788249/dockhop/internal/crypto"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/helper"
	"github.com/lx969788249/dockhop/internal/host"
	"github.com/lx969788249/dockhop/internal/logging"
	"github.com/lx969788249/dockhop/internal/preflight"
	"github.com/lx969788249/dockhop/internal/spec"
	"github.com/lx969788249/dockhop/internal/testpoint"
	"github.com/lx969788249/dockhop/internal/ui"
)

// Options 是迁入的选项。
type Options struct {
	Link          crypto.Link
	Yes           bool
	Existing      string // replace、skip、fail；空表示询问
	KeepSucceeded bool   // 有的服务没能启动时，不提问，保留已经正常运行的
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
	// Succeeded 在迁入成功（已经提交，不会再回滚）时调用一次，Failed 在迁入失败（包括只迁入了
	// 一部分）时调用一次，都可以为空；还没改动就取消、按 Ctrl+C 中断时都不调用（见 ending）。
	// 之后还有收尾：回滚、删除临时文件、通知旧服务器、显示结果。
	Succeeded func()
	Failed    func()
}

// totalSteps 是屏幕上的步骤数（见 steps）。
const totalSteps = 6

// run 保存一次迁入过程中的状态，失败、中断时据此回滚和收拾。
type run struct {
	Deps
	opts    Options
	started time.Time
	tool    *compose.Tool
	info    docker.Info

	workdir    string // 这次迁入的下载和解压目录
	bundlePath string
	dataDir    string
	downloaded bool // 已经开始下载

	plan        *bundle.Plan
	m           *bundle.Manifest
	d           preflight.Decisions
	items       []*citem // 要迁入的容器（去掉跳过的）
	projects    []bundle.Project
	anonVolumes map[string]bundle.Volume // 旧的匿名卷编号 → 迁移包里的数据
	imageAs     map[string]string        // 镜像名 → 实际使用的镜像名（digest 改用迁移包里的镜像时）
	images      map[string]imageSource   // 镜像文件 → 镜像从哪里来（见 images.go）；nil 表示还没决定
	helperImage string                   // 创建临时容器用的镜像（名字）

	j       *Journal
	rbDir   string
	undo    *undo
	vio     *volumeIO
	targets map[string]bool   // 被替换的已有容器名
	writers []writer          // 要临时停下的、和迁入的数据共用的已有容器（decide 找出来的）
	seen    []string          // decide 时和这次迁入有关的已有容器（见 scene），改动之前再核对一次
	shared  []Entry           // 临时停止的、和迁入的数据共用卷或目录的容器
	groups  []*group          // 要迁入的服务分成的组（见 groups.go）
	groupOf map[string]string // 容器、项目、卷、目录、网络 → 所在组

	replaceProjects  []string                             // 要替换的已有同名 Compose 项目
	existingProjects map[string][]docker.ContainerSummary // 新服务器上已有的 Compose 项目 → 它的容器
	noted            map[string]bool                      // 已经在事务日志里记下原来指向的镜像名（见 noteTag）

	done []string // 同一个链接上次已经迁入、这次跳过的服务

	committed      bool
	partial        bool // 只保留了一部分服务
	rollbackDone   bool
	rollbackIssues []string
	cleanupLeft    []string
}

// Run 执行迁入。任何一步失败或被中断，都按事务日志回滚；不管结果如何，都会删掉临时文件和
// 临时容器，最后打印结果摘要。
func Run(ctx context.Context, d Deps, o Options) error {
	r := &run{Deps: d, opts: o, started: time.Now(), imageAs: map[string]string{}, anonVolumes: map[string]bundle.Volume{},
		rbDir: filepath.Join(d.Home, "rollback", "current")}
	lock, err := host.TryLock(lockPath(d.Home))
	if err != nil {
		d.Screen.Problem(err.Error(), "")
		return ui.ErrReported
	}
	defer lock.Unlock()
	helper.Clean(ctx, d.Docker, lockPath(d.Home))
	r.dropStale()

	err = r.steps(ctx)
	end := r.ending(ctx, err)
	switch {
	case (end == endSucceeded || end == endCommitted) && d.Succeeded != nil:
		d.Succeeded()
	case (end == endPartial || end == endFailed) && d.Failed != nil:
		d.Failed()
	}
	if err != nil && r.j != nil && !r.committed {
		r.rollback(context.WithoutCancel(ctx))
	}
	r.cleanup(err == nil)
	return r.report(ctx, end)
}

// cleanup 收尾：关掉事务日志，删掉解密出来的数据（成功时连下载的文件一起）、临时容器和
// 复制给它们的程序。
func (r *run) cleanup(success bool) {
	r.j.close()
	r.cleanFiles(success)
	r.cleanHelpers(context.Background())
	removeHelper(r.Home)
}

// testFail、testPause 是测试用的埋点（见 testpoint）。
func (r *run) testFail(point string) bool { return testpoint.Fail(r.Getenv, point) }

func (r *run) testPause(ctx context.Context, point string) {
	testpoint.Pause(ctx, r.Getenv, r.Home, point)
}

// steps 按屏幕上的六个步骤依次执行各个环节。检查都在改动之前做完（begin 之前），之后对新服务器
// 的每一处改动都在事务里。
func (r *run) steps(ctx context.Context) error {
	s := r.Screen
	s.Begin(1, totalSteps, "检查环境")
	info, err := r.Docker.Info(ctx)
	if err != nil {
		return s.StepFailed("读取 Docker 信息失败", err.Error(), "")
	}
	r.info = info
	r.undo = &undo{dc: r.Docker, screen: s, logf: r.Log.Printf, fail: r.testFail,
		defaults: spec.SourceDefaults{CgroupVersion: info.CgroupVersion, LoggingDriver: info.LoggingDriver, DefaultRuntime: info.DefaultRuntime}}
	summary := "Docker " + info.ServerVersion
	if t, err := compose.Detect(ctx); err == nil {
		r.tool = t
		summary += " · Compose " + t.Version
	}
	if err := r.recoverInterrupted(ctx); err != nil {
		return err
	}
	s.End(summary)

	s.Begin(2, totalSteps, "迁入前检查")
	if err := r.fetchPlan(ctx); err != nil {
		return err
	}
	summary, err = r.check(ctx)
	if err != nil {
		return err
	}
	r.decideImages(ctx, r.plan.Images)
	s.End(summary)
	s.Begin(3, totalSteps, "下载迁移包")
	if err := r.download(ctx); err != nil {
		return err
	}
	summary, err = r.unpack(ctx)
	if err != nil {
		return err
	}
	if err := r.decide(ctx); err != nil {
		return err
	}
	if err := r.checkCompose(ctx); err != nil {
		return err
	}
	s.End(summary)

	// 检查都做完了，从这里开始改动新服务器：每一处改动都先记进事务日志，镜像也不例外。
	if err := r.begin(); err != nil {
		return err
	}
	s.Begin(4, totalSteps, "加载镜像")
	if err := r.loadImages(ctx); err != nil {
		return err
	}
	// 备份同名服务通常一闪而过（新服务器上多半没有），不单独占一步。
	s.Begin(5, totalSteps, "恢复数据")
	if err := r.prepare(ctx); err != nil {
		return err
	}
	if err := r.restoreData(ctx); err != nil {
		return err
	}
	s.Begin(6, totalSteps, "启动服务")
	if err := r.startServices(ctx); err != nil {
		return err
	}
	return r.commit(ctx)
}

// lockPath 是迁入任务的锁：同一台服务器上同时只能有一个迁入。
func lockPath(home string) string { return filepath.Join(home, "locks", "in.lock") }

func (r *run) cleanHelpers(ctx context.Context) {
	helper.Clean(ctx, r.Docker, lockPath(r.Home))
}
