package restore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/crypto"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/transfer"
)

// ---------- 镜像从哪里来 ----------
//
// 迁移包格式版本 2 起，每个镜像单独成一个文件，由旧服务器的下载服务提供（见 bundle.ImageFile）。
// 新服务器在下载之前逐个决定：本机已经有一模一样的镜像就直接用；能从镜像仓库（或本机配置的
// 镜像加速）按摘要拉到一模一样的镜像就自己拉；都不行才从旧服务器下载。拉取失败、卡住或太慢时
// 自动改从旧服务器下载。按摘要拉到的一定是同一个镜像，和从旧服务器传过来的完全一样；容器里
// 改过的文件不在镜像里，照旧从旧服务器传。这些都不显示在屏幕上，只写进日志。

// imageSource 是一个镜像文件对应的镜像从哪里来；两项都没有表示从旧服务器下载。
type imageSource struct {
	local string // 本机已经有一模一样的镜像：它在本机的 ID
	pull  bool   // 能从镜像仓库按摘要拉到
}

var (
	// pullCheckTimeout 是查询一个镜像能不能拉到最多等多久（只查询、不下载）。
	pullCheckTimeout = 15 * time.Second
	// 拉取在 pullWindow 里进展不到 pullMinBytes（下载和解压合起来）就算卡住或太慢，
	// 放弃拉取，改从旧服务器下载：一分钟不到 3MB，约 50KB/s。
	pullWindow   = time.Minute
	pullMinBytes = int64(3 << 20)
)

// decideImages 决定每个镜像文件从哪里来。查询镜像仓库同时进行：连不上镜像仓库时也只多等
// 一次超时。
func (r *run) decideImages(ctx context.Context, files []bundle.ImageFile) {
	r.images = map[string]imageSource{}
	// 向镜像仓库的查询同时进行，各自只写自己那一格，全部查完再填表。
	pullable := make([]bool, len(files))
	var wg sync.WaitGroup
	for i, f := range files {
		names := strings.Join(f.Refs, "、")
		if id := r.localImage(ctx, f); id != "" {
			r.images[f.File] = imageSource{local: id}
			r.Log.Printf("镜像 %s：本机已经有一模一样的，直接用", names)
			continue
		}
		if f.Pull == "" {
			r.Log.Printf("镜像 %s：没有仓库摘要（自己构建或导入的），从旧服务器下载", names)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, pullCheckTimeout)
			err := r.Docker.DistributionInspect(c, f.Pull)
			cancel()
			if err != nil {
				r.Log.Printf("镜像 %s：镜像仓库里查不到 %s（%v），从旧服务器下载", names, f.Pull, err)
				return
			}
			pullable[i] = true
			r.Log.Printf("镜像 %s：从镜像仓库按摘要拉取 %s", names, f.Pull)
		}()
	}
	wg.Wait()
	for i, f := range files {
		if pullable[i] {
			r.images[f.File] = imageSource{pull: true}
		}
	}
}

// localImage 返回本机上和 f 一模一样的镜像的 ID：镜像 ID 相同，或者有同一个仓库摘要。
func (r *run) localImage(ctx context.Context, f bundle.ImageFile) string {
	for _, ref := range []string{f.ID, f.Pull} {
		if ref == "" {
			continue
		}
		if img, err := r.Docker.InspectImage(ctx, ref); err == nil {
			return img.ID
		}
	}
	return ""
}

// fromOldServer 表示这个镜像文件要从旧服务器下载。
func (r *run) fromOldServer(f bundle.ImageFile) bool {
	src := r.images[f.File]
	return src.local == "" && !src.pull
}

func (r *run) imagePath(f bundle.ImageFile) string {
	return filepath.Join(r.workdir, "images", f.File)
}

// fetchPart 是一轮下载里的一个文件。
type fetchPart struct {
	url, path string
	size      int64 // 0 表示事先不知道
}

// fetch 从旧服务器依次下载几个文件，显示一条总的进度；旧服务器也按这一轮的总大小显示进度。
// 文件已经有一部分时从断点继续。
func (r *run) fetch(ctx context.Context, label string, parts []fetchPart) error {
	s := r.Screen
	var total int64
	for _, p := range parts {
		total += p.size
	}
	p := s.StartProgress(label, total, "下载停住了：网络可能断开了，会自动从断点继续")
	var before int64
	for _, part := range parts {
		if err := os.MkdirAll(filepath.Dir(part.path), 0o700); err != nil {
			p.Done(err)
			return fail(s, "无法创建下载目录", err.Error(), "")
		}
		err := r.client().Download(ctx, r.opts.Link, transfer.Part{URL: part.url, Size: part.size, Before: before, Total: total}, part.path,
			func(done, all int64) {
				if total == 0 && all > 0 {
					p.SetTotal(before + all)
				}
				p.Set(before + done)
			})
		if err != nil {
			p.Done(err)
			var ce *transfer.ConnError
			if errors.As(err, &ce) {
				return fail(s, ce.Reason, "", ce.Hint)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fail(s, label+"失败："+err.Error(), "", "检查两台服务器之间的网络后重新运行，会从断点继续")
		}
		if st, err := os.Stat(part.path); err == nil {
			before += st.Size()
		}
	}
	p.Done(nil)
	return nil
}

// loadImageFiles 让每个镜像都在这台服务器上就位：本机已有的加上原来的名字；能拉到的从镜像
// 仓库拉；其余的用从旧服务器下载的文件加载。
func (r *run) loadImageFiles(ctx context.Context) error {
	files := r.m.ImageFiles
	for i, f := range files {
		label := fmt.Sprintf("加载镜像 %d/%d", i+1, len(files))
		src := r.images[f.File]
		// 不管从哪里来，最后都会给它加上原来的名字（加载的文件里也带着这些名字）。
		if err := r.noteTags(ctx, f.Refs); err != nil {
			return err
		}
		var err error
		switch {
		case src.local != "":
			err = r.tagRefs(ctx, src.local, f)
		case src.pull:
			err = r.pullImage(ctx, label, f)
		default:
			err = r.loadImageFile(ctx, label, f)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// pullImage 从镜像仓库按摘要拉取 f，再加上原来的名字。拉取失败、卡住或太慢时改从旧服务器
// 下载：旧服务器的下载服务一直开到迁入成功。
func (r *run) pullImage(ctx context.Context, label string, f bundle.ImageFile) error {
	p := r.Screen.StartProgress(label, 0, "")
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var activity atomic.Int64
	var tooSlow atomic.Bool
	go func() {
		t := time.NewTicker(pullWindow / 12)
		defer t.Stop()
		last, since := int64(0), time.Now()
		for {
			select {
			case <-pctx.Done():
				return
			case <-t.C:
			}
			if time.Since(since) < pullWindow {
				continue
			}
			n := activity.Load()
			if n-last < pullMinBytes {
				tooSlow.Store(true)
				cancel()
				return
			}
			last, since = n, time.Now()
		}
	}()
	err := r.Docker.PullImage(pctx, f.Pull, func(downloaded, active int64) {
		p.Set(downloaded)
		activity.Store(active)
	})
	p.Done(err)
	if err == nil {
		img, ierr := r.Docker.InspectImage(ctx, f.Pull)
		if ierr == nil {
			return r.tagRefs(ctx, img.ID, f)
		}
		err = ierr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if tooSlow.Load() {
		err = fmt.Errorf("%v 内进展不到 %dMB", pullWindow, pullMinBytes>>20)
	}
	r.Log.Printf("从镜像仓库拉取 %s 没成功（%v），改从旧服务器下载", f.Pull, err)
	return r.loadImageFile(ctx, label, f)
}

// loadImageFile 从旧服务器下载 f（还没下载完整的话），解密后加载进 Docker，用完就删。
// 文件里带着镜像原来的名字，加载后就有。
func (r *run) loadImageFile(ctx context.Context, label string, f bundle.ImageFile) error {
	s := r.Screen
	path := r.imagePath(f)
	if st, err := os.Stat(path); err != nil || st.Size() != f.Bytes {
		if err := r.fetch(ctx, "下载镜像", []fetchPart{{url: r.opts.Link.ImageURL(f.File), path: path, size: f.Bytes}}); err != nil {
			return err
		}
	}
	if err := r.verifyDownload(path, f.SHA256, "镜像文件"); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return fail(s, "打不开下载的镜像文件 "+path, err.Error(), "")
	}
	defer file.Close()
	rc, err := bundle.OpenImageFile(file, r.opts.Link)
	if err != nil {
		return fail(s, "镜像文件解不开："+err.Error(), "", "在旧服务器重新运行迁出")
	}
	p := s.StartProgress(label, f.Size, "镜像较大时需要几分钟")
	err = r.Docker.LoadImages(ctx, io.TeeReader(rc, p))
	rc.Close()
	p.Done(err)
	if err != nil {
		if errors.Is(err, crypto.ErrCorrupted) {
			_ = os.Remove(path)
			return fail(s, "下载的镜像文件不完整或被改动过，已删除", "", "重新运行迁入，会重新下载")
		}
		return fail(s, "加载镜像失败："+err.Error(), "", "执行 df -h 检查磁盘空间，并确认 Docker 运行正常")
	}
	_ = os.Remove(path) // 用完就删，省磁盘
	return nil
}

// noteTags 在镜像名被改动之前（加名字，或者 docker load 按文件里记的名字加名字），把它们
// 原来指向哪个镜像记进事务日志：迁入失败时改回去，原来没有的名字删掉（见 undoTag）。
// 按摘要的引用（名字@sha256:…）不是名字，不会被改动。
func (r *run) noteTags(ctx context.Context, refs []string) error {
	if r.noted == nil {
		r.noted = map[string]bool{}
	}
	for _, ref := range refs {
		if strings.Contains(ref, "@sha256:") || r.noted[ref] {
			continue
		}
		id := ""
		img, err := r.Docker.InspectImage(ctx, ref)
		switch {
		case err == nil:
			id = img.ID
		case !docker.IsNotFound(err):
			return fail(r.Screen, "读不到新服务器上的镜像 "+ref, err.Error(), "确认 Docker 运行正常后重新运行")
		}
		if err := r.j.add(Entry{Op: opTag, Name: ref, ID: id}); err != nil {
			return fail(r.Screen, err.Error(), "", "")
		}
		r.noted[ref] = true
	}
	return nil
}

// tagRefs 给本机的镜像 id 加上 f 原来的名字。按摘要引用的名字不用加：拉取时就有了，
// 或者在 resolveImage 里处理。
func (r *run) tagRefs(ctx context.Context, id string, f bundle.ImageFile) error {
	for _, ref := range f.Refs {
		if strings.Contains(ref, "@sha256:") {
			continue
		}
		repo, tag := splitTag(ref)
		if err := r.Docker.TagImage(ctx, id, repo, tag); err != nil {
			return fail(r.Screen, "给镜像 "+ref+" 加名字失败", err.Error(), "")
		}
	}
	return nil
}
