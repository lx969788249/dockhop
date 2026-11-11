package restore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/compose"
	"github.com/lx969788249/dockhop/internal/crypto"
	"github.com/lx969788249/dockhop/internal/preflight"
	"github.com/lx969788249/dockhop/internal/spec"
	"github.com/lx969788249/dockhop/internal/ui"
)

// 解开迁移包，在改动任何东西之前检查里面的每一份数据：路径越没越界、有没有通过软链接写到
// 别处、会不会盖住 DockHop 自己的目录，Compose 项目的配置读不读得懂。内容有问题的迁移包重新
// 下载也用不上，直接删掉。

// unpack 解密、解开迁移包，并在改动任何东西之前检查里面的每一份数据。返回这一步的摘要。
func (r *run) unpack(ctx context.Context, chk *checked, pkg *bundleFiles) (string, error) {
	s := r.Screen
	if err := r.verifyDownload(pkg.bundlePath, chk.plan.BundleSHA256, "迁移包"); err != nil {
		return "", err
	}
	f, err := os.Open(pkg.bundlePath)
	if err != nil {
		return "", s.StepFailed("打不开迁移包 "+pkg.bundlePath, err.Error(), "")
	}
	st, _ := f.Stat()
	p := s.StartProgress("校验并解开", st.Size(), "")
	err = bundle.Extract(f, r.opts.Link, pkg.dataDir, p)
	f.Close()
	p.Done(err)
	if err != nil {
		_ = os.RemoveAll(pkg.dataDir)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		switch {
		case errors.Is(err, crypto.ErrWrongKey):
			return "", s.StepFailed(err.Error(), "", "")
		case errors.Is(err, crypto.ErrCorrupted), errors.Is(err, bundle.ErrUnsafe):
			return "", r.badBundle(chk, pkg, err)
		default:
			return "", s.StepFailed("解开迁移包失败："+err.Error(), "", "执行 df -h 检查磁盘空间")
		}
	}
	m, err := bundle.ReadManifest(pkg.dataDir)
	if err != nil {
		return "", s.StepFailed(err.Error(), "", "")
	}
	pkg.m = m
	if err := r.scan(pkg); err != nil {
		return "", r.badBundle(chk, pkg, err)
	}
	if is := r.homeClash(pkg); is != nil {
		pkg.dropDownload()
		return "", s.StepFailed(is.Title, "", is.Hint)
	}
	return fmt.Sprintf("%s · 校验通过", ui.HumanBytes(st.Size())), nil
}

// badBundle 报告迁移包里的内容有问题（解不开，或者有不合规的内容）。下载的文件重新运行也用不上，删掉。
// 解密时每一段都经过认证：能解开、只是内容不合规的，就是旧服务器打包的内容；解密发现被改坏的，
// 迁移计划记了 SHA-256 并且核对过，说明旧服务器上的文件本身就坏了。这些重新下载还是一样，只能
// 在旧服务器重新迁出。只有旧版本的计划没有记 SHA-256 时，被改坏才可能是下载时出的错，先重新下载。
func (r *run) badBundle(chk *checked, pkg *bundleFiles, err error) error {
	pkg.dropDownload()
	hint := "在旧服务器重新运行迁出"
	if pkg.downloaded && errors.Is(err, crypto.ErrCorrupted) && chk.plan.BundleSHA256 == "" {
		hint = "已删除下载的文件，重新运行即可重新下载；如果再次出现，请在旧服务器重新运行迁出"
	}
	return r.Screen.StepFailed(err.Error(), "", hint)
}

// homeClash 按迁移包里实际要写的目录，再确认一遍不会盖住 DockHop 自己的目录（和迁入前检查同一条规则）。
// 这是新服务器上的设置问题；换了 DOCKHOP_HOME，下载的文件也会放到别处，这里下载的同样用不上。
func (r *run) homeClash(pkg *bundleFiles) *preflight.Issue {
	for _, b := range pkg.m.Binds {
		paths := []string{b.Path}
		for _, l := range b.AllLinks() {
			paths = append(paths, l.Link)
		}
		for _, p := range paths {
			if is := preflight.HomeClash(p, r.Home); is != nil {
				return is
			}
		}
	}
	return nil
}

// missingIn 表示报错 msg 提到了 dir 下面一个不存在的路径。
func missingIn(msg, dir string) bool {
	for _, path := range regexp.MustCompile(regexp.QuoteMeta(dir)+`/[^\s"':,]+`).FindAllString(msg, -1) {
		if _, err := os.Lstat(path); err != nil {
			return true
		}
	}
	return false
}

// scan 检查迁移包里所有的卷、目录和 Compose 文件（路径是否越界、有没有通过软链接
// 写到别处）。
func (r *run) scan(pkg *bundleFiles) error {
	check := func(prefix string, parts int, what string, roots ...string) error {
		rc := bundle.OpenStream(pkg.dataDir, prefix, parts)
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
	for _, v := range pkg.m.Volumes {
		if err := check("volumes/"+v.Name, v.Parts, "数据卷 "+v.Name+" ", "dockhop-data"); err != nil {
			return err
		}
	}
	for _, c := range pkg.m.Containers {
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
	for _, b := range pkg.m.Binds {
		if !filepath.IsAbs(b.Path) || b.Path == "/" {
			return fmt.Errorf("迁移包里的目录路径不对：%q", b.Path)
		}
		if err := check("binds/"+b.ID, b.Parts, "目录 "+b.Path+" ", strings.TrimPrefix(b.Path, "/")); err != nil {
			return err
		}
		for _, l := range b.AllLinks() {
			if !filepath.IsAbs(l.Link) || filepath.Clean(l.Link) != l.Link || l.Link == "/" || !spec.Within(l.Target, b.Path) {
				return fmt.Errorf("迁移包里目录 %s 的软链接不对：%q → %q", b.Path, l.Link, l.Target)
			}
		}
	}
	for _, p := range pkg.m.Projects {
		if !filepath.IsAbs(p.WorkingDir) {
			return fmt.Errorf("迁移包里 Compose 项目 %s 的目录不对：%q", p.Name, p.WorkingDir)
		}
		if p.DirParts > 0 {
			if err := check("compose/"+p.Name+"/dir", p.DirParts, "Compose 项目 "+p.Name+" "); err != nil {
				return err
			}
		}
		if p.CheckParts > 0 {
			if err := check("compose/"+p.Name+"/check", p.CheckParts, "Compose 项目 "+p.Name+" 的配置文件"); err != nil {
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

// checkCompose 在改动新服务器之前，用新服务器上的 docker compose 把要迁入的每个 Compose 项目（decide
// 定下的）的配置文件读一遍。老版本读不懂新写法时在这里就停下，不用等到替换掉已有的服务、写完
// 数据之后才发现。
func (r *run) checkCompose(ctx context.Context, tool *compose.Tool, pkg *bundleFiles, sc *scope) error {
	s := r.Screen
	if len(sc.projects) == 0 {
		return nil
	}
	if tool == nil {
		return s.StepFailed("新服务器没有安装 Docker Compose，没法启动 Compose 项目（还没有改动新服务器）", "",
			"安装 Docker Compose（Docker 官方源的 docker-compose-plugin）后重新运行，迁移包已经下载好，不用重新下载")
	}
	for _, p := range sc.projects {
		s.Note("检查 Compose 项目 " + p.Name + " 的配置文件")
		if err := r.checkProject(ctx, tool, pkg, p); err != nil {
			hint := "检查这个项目的配置文件在新服务器的 docker compose " + tool.Version + " 上能不能用"
			if tool.Legacy() {
				hint = legacyHint(tool)
			}
			return s.StepFailed("新服务器上的 docker compose 读不懂 Compose 项目 "+p.Name+" 的配置文件（还没有改动新服务器）", err.Error(), hint)
		}
	}
	return nil
}

// checkProject 把项目目录解到临时目录里，执行 docker compose config -q。引用了项目目录
// 以外的文件、或者用绝对路径引用文件时，临时目录里没有这些文件，相关的报错不算数。
func (r *run) checkProject(ctx context.Context, tool *compose.Tool, pkg *bundleFiles, p bundle.Project) error {
	files := p.ConfigArgs
	if len(files) == 0 {
		files = p.ConfigFiles
	}
	// 检查用的项目文件：整个项目目录；项目目录随某个挂载目录迁移时，是单独打包的配置文件。
	stream, parts := "dir", p.DirParts
	if parts == 0 {
		stream, parts = "check", p.CheckParts
	}
	if parts == 0 {
		// v1.1.5 及以前的迁移包没有单独打包配置文件：项目文件都在挂载目录里，这里拿不到。
		r.Log.Printf("Compose 项目 %s 的文件随挂载目录迁移，迁移包里没有单独的配置文件，跳过检查", p.Name)
		return nil
	}
	stage := filepath.Join(pkg.workdir, "check-"+p.Name)
	_ = os.RemoveAll(stage)
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return nil
	}
	defer os.RemoveAll(stage)
	rc := bundle.OpenStream(pkg.dataDir, "compose/"+p.Name+"/"+stream, parts)
	err := bundle.ExtractTree(rc, stage)
	rc.Close()
	if err != nil {
		r.Log.Printf("没能解开 Compose 项目 %s 的文件来检查：%v", p.Name, err)
		return nil
	}
	var args []string
	for _, f := range files {
		if filepath.IsAbs(f) {
			if !spec.Within(f, p.WorkingDir) {
				r.Log.Printf("Compose 项目 %s 的配置文件 %s 不在项目目录里，跳过检查", p.Name, f)
				return nil
			}
			f, _ = filepath.Rel(p.WorkingDir, f)
		}
		f = filepath.Join(stage, f)
		if !spec.Within(f, stage) {
			r.Log.Printf("Compose 项目 %s 的配置文件 %s 不在项目目录里，跳过检查", p.Name, f)
			return nil
		}
		args = append(args, f)
	}
	err = tool.Exec(ctx, compose.Project{Name: p.Name, WorkingDir: stage, Files: args}, r.Log.Printf, "config", "-q")
	if err == nil {
		return nil
	}
	// 只检查新服务器的 Compose 读不读得懂配置；报错说的是检查时手边没有的文件（迁入时会和
	// 项目一起放回去，或者本来就在挂载目录里）的，不算数。
	msg := err.Error()
	missing := missingIn(msg, stage)
	for _, path := range append(append([]string{p.WorkingDir + "/"}, p.Outside...), p.Skipped...) {
		missing = missing || strings.Contains(msg, path)
	}
	if missing {
		r.Log.Printf("检查 Compose 项目 %s 时的报错涉及临时目录里没有的文件，不算数", p.Name)
		return nil
	}
	msg = strings.TrimPrefix(msg, "config -q 失败：")
	return errors.New(strings.ReplaceAll(msg, stage, p.WorkingDir))
}

func legacyHint(tool *compose.Tool) string {
	return "新服务器的 docker-compose " + tool.Version + " 是老版本，可能读不懂这个项目配置文件里的新写法。" +
		"安装新版 Docker Compose（Docker 官方源的 docker-compose-plugin，或者把 GitHub 上下载的 docker-compose 放到 " +
		"/usr/local/lib/docker/cli-plugins/）后重新运行，迁移包已经下载好，不用重新下载"
}
