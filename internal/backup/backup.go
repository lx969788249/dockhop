// Package backup 是迁出：在旧服务器上选择容器、读取配置、短暂停机打包数据、
// 保存镜像，生成加密的迁移包，并提供下载，直到新服务器迁入完成。
//
// 这个文件是迁出的骨架：Run 加锁、执行、收尾（恢复停下的容器、删掉工作文件）、显示结果；
// steps 按屏幕上的六个步骤依次调用各个环节。每个环节在自己的文件里：
//
//	select.go    选择要迁移的服务
//	collect.go   读取配置：容器、数据卷、目录、Compose 项目
//	confirm.go   停机之前的确认
//	capture.go   分批停下容器，读出要打包的数据（容器里的改动、数据卷、目录）
//	pack.go      停机打包、导出镜像
//	plan.go      迁移计划（新服务器下载之前据此检查）
//	serve.go     提供下载，等新服务器迁入
//	state.go     停机记录：被强制结束后把停下的容器恢复原状
//	report.go    结果摘要
package backup

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lx969788249/dockhop/internal/compose"
	"github.com/lx969788249/dockhop/internal/crypto"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/helper"
	"github.com/lx969788249/dockhop/internal/host"
	"github.com/lx969788249/dockhop/internal/logging"
	"github.com/lx969788249/dockhop/internal/testpoint"
	"github.com/lx969788249/dockhop/internal/transfer"
	"github.com/lx969788249/dockhop/internal/ui"
)

// Options 是迁出的选项。
type Options struct {
	Include    []string
	StopShared bool
	Port       int
	Host       string
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
	// Failed 在停机打包之后迁出失败时调用一次，可以为空；还没改动就取消或者停下、按 Ctrl+C 中断、
	// 传输开始之后自己结束传输时都不调用（见 ending）。之后还有收尾：显示结果。
	Failed func()
}

// totalSteps 是迁出一共几步。
const totalSteps = 6

// run 保存一次迁出过程中的状态，出错或中断时据此收拾。
type run struct {
	Deps
	opts           Options
	started        time.Time
	rid            string
	workdir        string
	stopped        []StoppedContainer // 已经停下、还没恢复的源容器
	chosen         string             // 迁移哪些服务，写在步骤结果和最终结果里
	restored       int
	toRestore      int
	downtime       time.Duration
	tempTags       []string // 为导出旧版本镜像临时加的名字
	composeVersion string
	server         *transfer.Server
	status         string // 新服务器报告的迁入结果
	summary        []string
}

// Run 执行迁出。不管成功、失败还是被中断，都会把源容器恢复原状、删除临时文件，
// 最后打印结果摘要。
func Run(ctx context.Context, d Deps, o Options) error {
	r := &run{Deps: d, opts: o, started: time.Now(), rid: randomID()}
	lock, err := host.TryLock(lockPath(d.Home))
	if err != nil {
		d.Screen.Problem(err.Error(), "")
		return ui.ErrReported
	}
	defer lock.Unlock()
	helper.Clean(ctx, d.Docker, lockPath(d.Home))
	dropStale(d.Home)

	err = r.steps(ctx)
	r.cleanup()
	end := r.ending(ctx, err)
	if end == endFailed && d.Failed != nil {
		d.Failed()
	}
	return r.report(end)
}

// dropStale 删掉以前的迁出被强制结束时留下的工作目录（迁移包和镜像文件）。正常结束时 cleanup
// 会删掉；同一个目录里同时只能有一个迁出，拿到锁之后还在的都是遗留的。
func dropStale(home string) {
	old, _ := filepath.Glob(filepath.Join(home, "work", "out-*"))
	for _, dir := range old {
		_ = os.RemoveAll(dir)
	}
}

// steps 按屏幕上的六个步骤依次执行各个环节。停机之前的检查和确认都在打包（第 4 步）之前做完。
func (r *run) steps(ctx context.Context) error {
	s, dc := r.Screen, r.Docker

	s.Begin(1, totalSteps, "检查环境")
	info, err := dc.Info(ctx)
	if err != nil {
		return s.StepFailed("读取 Docker 信息失败", err.Error(), "")
	}
	tool, terr := compose.Detect(ctx)
	envSummary := "Docker " + info.ServerVersion
	if terr == nil {
		r.composeVersion = tool.Version
		envSummary += " · Compose " + tool.Version
		r.Log.Printf("Compose：%s", tool)
	} else {
		tool = nil
	}
	s.End(envSummary)

	s.Begin(2, totalSteps, "选择要迁移的服务")
	inv, err := buildInventory(ctx, dc, s)
	if err != nil {
		return s.StepFailed(err.Error(), "", "")
	}
	chosen, err := chooseUnits(inv, r.opts.Include, s, r.Prompt)
	if ui.Canceled(err) {
		return err
	}
	if err != nil {
		return s.StepFailed(err.Error(), "", "")
	}
	sel, err := withDependencies(inv, chosen, s)
	if err != nil {
		return s.StepFailed(err.Error(), "", "")
	}
	if tool == nil && sel.projectCount() > 0 {
		s.Warn("这台服务器上找不到 docker compose 命令，无法核对 Compose 配置，将直接迁移项目目录")
	}
	r.chosen = chosenText(sel)
	s.End(r.chosen)

	s.Begin(3, totalSteps, "读取配置")
	col, err := collect(ctx, dc, tool, inv, sel, s, r.ignored())
	if errors.Is(err, ui.ErrReported) {
		return err
	}
	if err != nil {
		return s.StepFailed(err.Error(), "", "")
	}
	for _, v := range col.secrets {
		r.Redactor.AddSecret(v)
	}
	if !dc.Local() && (len(col.binds) > 0 || len(col.projects) > 0) {
		return s.StepFailed("Docker 不在这台服务器上（DOCKER_HOST 指向了别的机器），没法读取容器挂载的目录和 Compose 项目文件", "",
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
	if err := r.checkAutoRemove(col); err != nil {
		return err
	}
	for _, it := range col.sel.items {
		if size := col.layerBytes[it]; size > largeLayer {
			s.Warn("容器 %s 里没挂载出来的改动有 %s，会在停机期间一起打包，停机时间会因此变长；"+
				"如果是日志、缓存、崩溃转储这类没用的文件，可以先删掉再迁出",
				it.name(), ui.HumanBytes(size))
		}
	}
	for _, p := range col.projects {
		for _, f := range p.skipped {
			s.Warn("Compose 项目 %s 目录里的大文件 %s 没有迁移（超过 100MB）；需要的话请手动复制", p.u.project, f)
		}
	}
	r.workdir = filepath.Join(r.Home, "work", "out-"+r.rid)
	need := col.dataBytes() + col.imageBytes
	disk, err := host.DiskOf(r.workdir)
	if err == nil && disk.Free < uint64(need) {
		return s.StepFailed(fmt.Sprintf("磁盘空间可能不够：迁移包最多需要约 %s（压缩前），%s 所在磁盘只剩 %s",
			ui.HumanBytes(need), disk.Path, ui.HumanBytes(int64(disk.Free))),
			"清理磁盘，或在命令前面加上 DOCKHOP_HOME=空间更大的目录 再运行", "")
	}
	if err := os.MkdirAll(r.workdir, 0o700); err != nil {
		return s.StepFailed("无法创建工作目录 "+r.workdir, err.Error(), "")
	}
	ln, port, err := transfer.Listen(r.opts.Port)
	if err != nil {
		return s.StepFailed(err.Error(), "用 --port 换一个端口", "")
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
		return s.StepFailed("生成迁移链接失败", err.Error(), "")
	}
	r.Redactor.AddSecret(link.Secret())
	free := "剩余空间未知"
	if disk.Total > 0 {
		free = "剩余 " + ui.HumanBytes(int64(disk.Free))
	}
	s.End(fmt.Sprintf("数据和镜像约 %s · 磁盘%s", ui.HumanBytes(need), free))

	bundlePath := filepath.Join(r.workdir, "bundle")
	planPath := filepath.Join(r.workdir, "plan")
	m, size, sum, err := r.pack(ctx, col, link, bundlePath)
	if err != nil {
		ln.Close()
		return err
	}
	images := len(m.Images.Refs)

	// 迁移计划是新服务器做迁入前检查、核对下载的文件的依据，没有它不能迁入。
	if err := writePlan(planPath, link, buildPlan(m, col, size, sum)); err != nil {
		ln.Close()
		return s.StepFailed("生成迁移计划失败", err.Error(), "执行 df -h 检查磁盘空间后重新运行")
	}
	s.End(fmt.Sprintf("%d 个镜像 · 迁移包共 %s", images, ui.HumanBytes(size+imageFileBytes(m.ImageFiles))))

	s.Begin(6, totalSteps, "启动下载服务")
	return r.serve(ctx, ln, link, bundlePath, planPath, m.ImageFiles)
}

// largeLayer：容器里没挂载出来的改动超过这么大时，停机前提醒。
const largeLayer = 100 << 20

// cleanup 收尾：恢复还停着的源容器、删掉临时快照和临时容器、关闭下载服务、
// 删除工作目录。中断时也会执行，开始了就做完（见 ui.Screen.Finish），最多等 5 分钟。
func (r *run) cleanup() {
	r.Screen.Finish(context.Background(), func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
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
		helper.Clean(ctx, r.Docker, lockPath(r.Home))
		if r.workdir != "" {
			if err := os.RemoveAll(r.workdir); err != nil {
				r.Screen.Warn("临时文件没能删除，请手动删除：%s", r.workdir)
			}
		}
	})
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

// testPause 是测试用的埋点（见 testpoint），用来模拟“停机期间被强制结束”。
func (r *run) testPause(ctx context.Context, point string) {
	testpoint.Pause(ctx, r.Getenv, r.Home, point)
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
