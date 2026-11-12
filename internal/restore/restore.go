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
	Test     testpoint.Settings // 测试用的设置，照常运行时是零值（见 testpoint）
	// MountPoint 判断一个目录本身是不是挂载点：挂载点只能原地换内容。为空时用 host.IsMountPoint，
	// 按这台机器的实际情况判断；测试时换成假的。
	MountPoint func(path string) (bool, error)
	// Succeeded 在迁入成功（已经提交，不会再回滚）时调用一次，Failed 在改动之后迁入失败（包括只迁入
	// 了一部分）时调用一次，都可以为空；还没改动就取消或者停下、按 Ctrl+C 中断时都不调用（见 ending）。
	// 之后还有收尾：回滚、删除临时文件、通知旧服务器、显示结果。
	Succeeded func()
	Failed    func()
}

// totalSteps 是屏幕上的步骤数（见 steps）。
const totalSteps = 6

// run 是一次迁入。开始就定下的：依赖、选项、回滚资料的目录、撤销用的 undo；结局（out）开始是零值，提交、
// 回滚时填。每一步的结果单独成一个结构，按顺序显式传给下一步（见 steps）：检查的结果（checked）、下载解开的
// 迁移包（bundleFiles）、定下的迁入范围（scope），开始改动之后的几步都在 migrate 上。
type run struct {
	Deps
	opts    Options
	started time.Time
	rbDir   string
	undo    *undo
	out     outcome  // 结局：开始是零值，提交、回滚时填
	at      progress // 走到了哪一步（见 progress）
}

// progress 是这次迁入走到了哪一步：各步的结果，没走到的为空。只有 steps 往里记；结局（ending）、收尾（cleanup）、
// 撤销（rollback）和结果摘要（report）据此判断，别处要用哪一步的结果，由 steps 显式传过去。
type progress struct {
	chk *checked
	pkg *bundleFiles
	sc  *scope
	mg  *migrate
}

// checked 是迁入前检查的结果（第 2 步）。
type checked struct {
	plan   *bundle.Plan
	d      preflight.Decisions    // 检查时的选择；定迁入范围时（decide）补上按新服务器现在的样子跳过的
	done   []string               // 同一个链接上次已经迁入、这次跳过的服务
	images map[string]imageSource // 镜像文件 → 镜像从哪里来（见 images.go）
}

// bundleFiles 是下载、解开的迁移包（第 3 步）。下载到一半出错时也据此清理、写结果摘要。
type bundleFiles struct {
	workdir    string // 这次迁入的下载和解压目录
	bundlePath string
	dataDir    string
	downloaded bool // 已经开始下载
	m          *bundle.Manifest
}

// scope 是按新服务器现在的样子定下的这次迁入（第 3 步，见 decide）：迁哪些容器和 Compose 项目，分成哪几组，
// 要替换、临时停下哪些已有的容器。
type scope struct {
	items            []*citem                             // 要迁入的容器（去掉跳过的）
	projects         []bundle.Project                     // 要迁入的 Compose 项目
	anonVolumes      map[string]bundle.Volume             // 旧的匿名卷编号 → 迁移包里的数据
	targets          map[string]bool                      // 被替换的已有容器名
	writers          []writer                             // 要临时停下的、和迁入的数据共用的已有容器
	seen             []string                             // 和这次迁入有关的已有容器（见 scene），改动之前再核对一次
	groups           []*group                             // 要迁入的服务分成的组（见 groups.go）
	groupOf          map[string]string                    // 容器、项目、卷、目录、网络 → 所在组
	replaceProjects  []string                             // 要替换的已有同名 Compose 项目
	existingProjects map[string][]docker.ContainerSummary // 新服务器上已有的 Compose 项目 → 它的容器
}

// migrate 是开始改动新服务器之后的几步（第 4–6 步）：加载镜像、备份要替换的服务、恢复数据、启动、提交，
// 失败时撤销。begin 开始事务时建；要用的都在这之前定下了，不会是空的。
type migrate struct {
	*run
	tool *compose.Tool // 新服务器上的 Docker Compose，没有时为空
	chk  *checked
	pkg  *bundleFiles
	sc   *scope
	j    *Journal

	vio     *volumeIO         // 读写数据卷，第一次用到时建（见 volumes）
	shared  []Entry           // 临时停下的、和迁入的数据共用卷或目录的容器
	imageAs map[string]string // 镜像名 → 实际使用的镜像名（digest 改用迁移包里的镜像时）
	noted   map[string]bool   // 已经在事务日志里记下原来指向的镜像名（见 noteTag）
}

// outcome 是一次迁入的结局：提交了没有、是不是只保留了一部分服务，撤销过迁入的内容没有、没能撤销和没能
// 清理的地方。结果摘要（report）和迁移次数（ending）按它定。
type outcome struct {
	committed      bool
	partial        bool // 只保留了一部分服务
	rollbackDone   bool
	rollbackIssues []string
	cleanupLeft    []string
}

// Run 执行迁入。任何一步失败或被中断，都按事务日志回滚；不管结果如何，都会删掉临时文件和
// 临时容器，最后打印结果摘要。
func Run(ctx context.Context, d Deps, o Options) error {
	r := newRun(d, o)
	lock, err := host.TryLock(lockPath(d.Home))
	if err != nil {
		d.Screen.Problem(err.Error(), "")
		return ui.ErrReported
	}
	defer lock.Unlock()
	helper.Clean(ctx, d.Docker, lockPath(d.Home))
	r.dropStale()

	err = r.steps(ctx)
	r.Screen.StepFailed(err)
	end := r.ending(ctx, err)
	switch {
	case (end == endSucceeded || end == endCommitted) && d.Succeeded != nil:
		d.Succeeded()
	case (end == endPartial || end == endFailed) && d.Failed != nil:
		d.Failed()
	}
	if mg := r.at.mg; err != nil && mg != nil && !r.out.committed {
		r.Screen.Finish(ctx, mg.rollback)
	}
	r.cleanup(err == nil)
	return r.report(ctx, end)
}

// newRun 按 d、o 准备一次迁入，d 里为空的 MountPoint 补上默认的。撤销用的 undo 开始就有：上次迁入被强制
// 结束留下的，第 1 步就要撤销。
func newRun(d Deps, o Options) *run {
	if d.MountPoint == nil {
		d.MountPoint = host.IsMountPoint
	}
	r := &run{Deps: d, opts: o, started: time.Now(), rbDir: filepath.Join(d.Home, "rollback", "current")}
	r.undo = &undo{dc: d.Docker, screen: d.Screen, logf: d.Log.Printf, fail: d.Test.Fails, mountPoint: d.MountPoint,
		record: func(e Entry) error { return appendJournal(r.rbDir, e) }}
	return r
}

// cleanup 收尾：关掉事务日志，删掉解密出来的数据（成功时连下载的文件一起）、临时容器和
// 复制给它们的程序。
func (r *run) cleanup(success bool) {
	if r.at.mg != nil {
		r.at.mg.j.close()
	}
	if r.at.pkg != nil {
		r.cleanFiles(r.at.pkg, success)
	}
	r.cleanHelpers(context.Background())
	removeHelper(r.Home)
}

// steps 按屏幕上的六个步骤依次执行各个环节。检查都在改动之前做完（begin 之前），之后对新服务器
// 的每一处改动都在事务里。
func (r *run) steps(ctx context.Context) error {
	s := r.Screen
	s.Begin(1, totalSteps, "检查环境")
	info, err := r.Docker.Info(ctx)
	if err != nil {
		return &ui.Failure{Reason: "读取 Docker 信息失败", Err: err}
	}
	summary := "Docker " + info.ServerVersion
	var tool *compose.Tool
	if t, err := compose.Detect(ctx); err == nil {
		tool = t
		summary += " · Compose " + t.Version
	}
	if err := r.recoverInterrupted(ctx); err != nil {
		return err
	}
	s.End(summary)

	s.Begin(2, totalSteps, "迁入前检查")
	plan, err := r.fetchPlan(ctx)
	if err != nil {
		return err
	}
	chk := &checked{plan: plan}
	r.at.chk = chk
	if summary, err = r.check(ctx, chk, tool); err != nil {
		return err
	}
	chk.images = r.decideImages(ctx, plan.Images)
	s.End(summary)

	s.Begin(3, totalSteps, "下载迁移包")
	pkg := &bundleFiles{}
	r.at.pkg = pkg
	if err := r.download(ctx, chk, pkg); err != nil {
		return err
	}
	if summary, err = r.unpack(ctx, chk, pkg); err != nil {
		return err
	}
	sc, err := r.decide(ctx, chk, pkg)
	if err != nil {
		return err
	}
	r.at.sc = sc
	if err := r.checkCompose(ctx, tool, pkg, sc); err != nil {
		return err
	}
	s.End(summary)

	// 检查都做完了，从这里开始改动新服务器：每一处改动都先记进事务日志，镜像也不例外。
	mg, err := r.begin(tool, chk, pkg, sc)
	if err != nil {
		return err
	}
	r.at.mg = mg
	s.Begin(4, totalSteps, "加载镜像")
	if err := mg.loadImages(ctx); err != nil {
		return err
	}
	// 备份同名服务通常一闪而过（新服务器上多半没有），不单独占一步。
	s.Begin(5, totalSteps, "恢复数据")
	if err := mg.prepare(ctx); err != nil {
		return err
	}
	if err := mg.restoreData(ctx); err != nil {
		return err
	}
	s.Begin(6, totalSteps, "启动服务")
	if err := mg.startServices(ctx); err != nil {
		return err
	}
	return mg.commit(ctx)
}

// lockPath 是迁入任务的锁：同一台服务器上同时只能有一个迁入。
func lockPath(home string) string { return filepath.Join(home, "locks", "in.lock") }

func (r *run) cleanHelpers(ctx context.Context) {
	helper.Clean(ctx, r.Docker, lockPath(r.Home))
}
