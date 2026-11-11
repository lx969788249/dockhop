package restore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/host"
	"github.com/lx969788249/dockhop/internal/preflight"
	"github.com/lx969788249/dockhop/internal/spec"
	"github.com/lx969788249/dockhop/internal/ui"
)

// 恢复数据：数据卷、目录（连同软链接）、Compose 项目文件。写之前先把原来的备份好、记进事务日志，
// 文件操作见 files.go，读写数据卷见 volumes.go。

func (r *run) restoreData(ctx context.Context) error {
	s, dc := r.Screen, r.Docker
	vols, binds := dataTargets(r.writes())
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
			return s.StepFailed("读取数据卷 "+v.Name+" 失败", err.Error(), "")
		}
		entry := Entry{Op: opVolume, Name: v.Name, Existed: existed}
		if existed {
			if volumeDriver(cur.Driver) != volumeDriver(meta.Driver) || !maps.Equal(cur.Options, meta.Options) {
				return s.StepFailed("新服务器上已有同名、但存储方式不同的数据卷 "+v.Name+"，拒绝覆盖", "",
					"确认新服务器上的这个卷可以删除后，执行 docker volume rm "+v.Name+" 再重试")
			}
			// 先把新服务器上原有的数据备份到回滚资料里。
			entry.Backup = r.j.file("volume")
			s.Note("备份新服务器上原有的数据卷 · " + v.Name)
			if err := r.saveVolume(ctx, v.Name, entry.Backup); err != nil {
				return s.StepFailed("没能先备份新服务器上原有的数据卷 "+v.Name, err.Error(), "执行 df -h 检查磁盘空间")
			}
		}
		if err := r.j.add(entry); err != nil {
			return s.StepFailed(err.Error(), "", "")
		}
		if !existed {
			if err := dc.CreateVolume(ctx, v.Name, meta.Driver, meta.Options, meta.Labels); err != nil {
				return s.StepFailed("数据卷 "+v.Name+" 创建失败", err.Error(), "不是 local 驱动的卷，需要先在新服务器安装对应的卷插件")
			}
		}
		rc := bundle.OpenStream(r.dataDir, "volumes/"+v.Name, v.Parts)
		p := s.StartProgress("卷 "+v.Name, v.Bytes, "")
		var werr error
		if mustClear(v.Name, existed) {
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
			return s.StepFailed("数据卷 "+v.Name+" 写入失败", werr.Error(), "执行 df -h 检查磁盘空间")
		}
		_ = os.RemoveAll(filepath.Join(r.dataDir, "volumes", v.Name))
		nv++
	}
	for _, b := range r.m.Binds {
		if spec.IsSystemPath(b.Path) {
			s.Detail("宿主机系统路径只重建挂载、不覆盖：%s", b.Path)
			continue
		}
		if !slices.Contains(binds, b.Path) {
			continue
		}
		r.in("b:" + b.Path)
		if err := r.restoreBind(ctx, b); err != nil {
			return s.StepFailed("目录 "+b.Path+" 恢复失败", err.Error(), "")
		}
		nb++
	}
	for _, p := range r.projects {
		r.in("p:" + p.Name)
		if err := r.installProject(p); err != nil {
			return s.StepFailed("Compose 项目 "+p.Name+" 的文件没能放回原来的位置", err.Error(), "")
		}
	}
	r.in("")
	if r.testFail("after-data") {
		return s.StepFailed("（测试）模拟恢复数据之后失败", "", "")
	}
	r.testPause(ctx, "after-data")
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var parts []string
	if nv > 0 {
		parts = append(parts, fmt.Sprintf("%d 个数据卷", nv))
	}
	if nb > 0 {
		parts = append(parts, fmt.Sprintf("%d 个目录", nb))
	}
	if len(r.projects) > 0 {
		parts = append(parts, fmt.Sprintf("%d 个 Compose 项目", len(r.projects)))
	}
	s.End(strings.Join(parts, " · "))
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
	// 接下来就要清空这个卷：备份必须先确实写到磁盘，否则断电后只剩半份。
	_, err = io.Copy(f, rc)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = host.SyncDir(filepath.Dir(path))
	}
	return err
}

func (r *run) restoreBind(ctx context.Context, b bundle.Bind) error {
	// 迁入前检查已经在路径本身是软链接时停下（见 preflight.LinkedBind），这里是动手之前最后再看一次：
	// 检查之后才被换成软链接的，同样不迁入。
	if dest, err := os.Readlink(b.Path); err == nil {
		return errors.New(preflight.LinkedBind(b.Path, dest))
	}
	// 整个替换，还是原地换内容（单独挂载的目录不能改名），见 dirs.go。
	swap, err := r.newDirSwap(b.Path, b.Dir)
	if err != nil {
		return err
	}
	if err := r.mkdirAll(filepath.Dir(b.Path), 0o755); err != nil {
		return err
	}
	// 解压用的临时目录先记进日志：程序被强制结束时它会留下来，回滚时删掉。
	if err := r.j.add(Entry{Op: opStage, Path: swap.stage}); err != nil {
		return err
	}
	_ = os.RemoveAll(swap.stage)
	if err := os.Mkdir(swap.stage, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(swap.stage)
	rc := bundle.OpenStream(r.dataDir, "binds/"+b.ID, b.Parts)
	p := r.Screen.StartProgress("目录 "+ui.ShortPath(b.Path), b.Bytes, "")
	err = bundle.ExtractTree(io.TeeReader(rc, p), swap.stage)
	rc.Close()
	p.Done(err)
	if err == nil && r.testFail("bind:"+b.Path) {
		err = errors.New("（测试）模拟写入失败")
	}
	if err != nil {
		return err
	}
	staged := filepath.Join(swap.stage, strings.TrimPrefix(b.Path, "/"))
	st, err := os.Lstat(staged)
	if err != nil {
		return fmt.Errorf("迁移包里缺少 %s", b.Path)
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("迁移包里的 %s 是软链接，拒绝使用", b.Path)
	}
	if err := r.swapIn(swap, staged); err != nil {
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
			if host.RealPath(link) == host.RealPath(target) {
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
	// 迁入前检查已经在要放文件的地方是软链接时停下（见 preflight.LinkedProject），这里是动手之前最后再看
	// 一次：检查之后才被换成软链接的，同样不放。
	var paths []string
	if p.DirParts > 0 {
		paths = append(paths, p.WorkingDir)
	}
	if p.OutParts > 0 {
		paths = append(paths, p.Outside...)
	}
	for _, path := range paths {
		if dest, err := os.Readlink(path); err == nil {
			return errors.New(preflight.LinkedProject(p.Name, path, dest))
		}
	}
	skip := func(path string) bool { return spec.Within(path, r.Home) || spec.Within(path, r.workdir) }
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
