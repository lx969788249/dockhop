package backup

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/spec"
	"github.com/lx969788249/dockhop/internal/ui"
)

// helperLabel 标记 DockHop 创建的临时容器，程序被中断后可以据此清理。
const helperLabel = "dockhop.helper"

// stopWaves 把要停的容器分批：依赖别人的容器（借用网络等）在前面的批次，被依赖的
// 在后面；同一批里的容器同时停止，停机时间取决于最慢的一个，而不是全部加起来。
// 恢复时按相反的顺序。
func stopWaves(inv *inventory, items []*item) [][]*item {
	in := map[*item]bool{}
	for _, it := range items {
		in[it] = true
	}
	dependents := map[*item][]*item{}
	for _, it := range items {
		for _, ref := range spec.Dependencies(it.c) {
			if dep := inv.lookup(ref); dep != nil && in[dep] && dep != it {
				dependents[dep] = append(dependents[dep], it)
			}
		}
	}
	level := map[*item]int{}
	var depth func(it *item, guard int) int
	depth = func(it *item, guard int) int {
		if l, ok := level[it]; ok {
			return l
		}
		l := 0
		if guard < len(items) { // 循环依赖时不再往下找
			for _, d := range dependents[it] {
				l = max(l, depth(d, guard+1)+1)
			}
		}
		level[it] = l
		return l
	}
	var waves [][]*item
	for _, it := range items {
		l := depth(it, 0)
		for len(waves) <= l {
			waves = append(waves, nil)
		}
		waves[l] = append(waves[l], it)
	}
	return waves
}

// stopContainers 按批停止容器（同一批同时停，各自用自己的停止等待时间），
// 并逐个确认确实停了。
func stopContainers(ctx context.Context, dc *docker.Client, waves [][]StoppedContainer, screen *ui.Screen) error {
	total := 0
	for _, w := range waves {
		total += len(w)
	}
	var done atomic.Int32
	for _, wave := range waves {
		errs := make([]error, len(wave))
		var wg sync.WaitGroup
		for i, c := range wave {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = stopOne(ctx, dc, c)
				screen.Count("停止容器", int(done.Add(1)), total, c.Name)
			}()
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			return err
		}
	}
	return nil
}

func stopOne(ctx context.Context, dc *docker.Client, c StoppedContainer) error {
	if c.State == "paused" {
		if err := dc.UnpauseContainer(ctx, c.ID); err != nil {
			return fmt.Errorf("容器 %s 没能从暂停中恢复，无法停止：%v", c.Name, err)
		}
	}
	if err := dc.StopContainer(ctx, c.ID, nil); err != nil {
		return fmt.Errorf("容器 %s 没能停止：%v", c.Name, err)
	}
	ct, _, err := dc.InspectContainer(ctx, c.ID)
	if err != nil {
		return fmt.Errorf("停止后读不到容器 %s 的状态：%v", c.Name, err)
	}
	if ct.State.Running {
		return fmt.Errorf("容器 %s 停止后仍在运行", c.Name)
	}
	return nil
}

// snapshotReason 说明容器为什么需要快照；不需要时返回空。默认只在可写层确实有
// 改动、或原镜像标签已经指向别的版本时快照，其他容器保留原镜像名，以后照常升级。
func snapshotReason(ctx context.Context, dc *docker.Client, it *item, mode string) (string, error) {
	img, err := dc.InspectImage(ctx, it.c.Config.Image)
	switch {
	case err != nil && docker.IsNotFound(err):
		return "原镜像标签已经不存在", nil
	case err != nil:
		return "", err
	case img.ID != it.c.Image:
		return "原镜像标签已经指向别的版本", nil
	}
	switch mode {
	case "always":
		return "按 --snapshot always", nil
	case "never":
		return "", nil
	}
	changes, err := dc.ContainerChanges(ctx, it.c.ID)
	if err != nil {
		return "", err
	}
	if spec.WritableChanged(changes) {
		return "容器里有没挂载出来的改动", nil
	}
	return "", nil
}

// openVolume 读出卷里的全部内容（tar 流，路径以 dockhop-data/ 开头）。用迁移涉及的
// 镜像创建一个不启动的临时容器挂载这个卷，通过 Docker 接口读取；不需要额外的镜像，
// 也不需要联网。close 会删掉临时容器。
func openVolume(ctx context.Context, dc *docker.Client, v *volumeInfo, name string) (io.ReadCloser, error) {
	spec := map[string]any{
		"Image":      v.imageID,
		"Entrypoint": []string{"/dockhop-helper-never-started"},
		"Labels":     map[string]string{helperLabel: "1"},
		"HostConfig": map[string]any{
			"NetworkMode": "none",
			"Mounts":      []map[string]any{{"Type": "volume", "Source": v.name, "Target": "/dockhop-data", "ReadOnly": true}},
		},
	}
	id, err := dc.CreateContainer(ctx, name, spec)
	if err != nil {
		return nil, fmt.Errorf("创建读取数据卷 %s 的临时容器失败：%v", v.name, err)
	}
	rc, err := dc.GetArchive(ctx, id, "/dockhop-data")
	if err != nil {
		_ = dc.RemoveContainer(context.WithoutCancel(ctx), id, true)
		return nil, fmt.Errorf("读取数据卷 %s 失败：%v", v.name, err)
	}
	return &cleanupReader{ReadCloser: rc, cleanup: func() { _ = dc.RemoveContainer(context.WithoutCancel(ctx), id, true) }}, nil
}

type cleanupReader struct {
	io.ReadCloser
	cleanup func()
}

func (c *cleanupReader) Close() error {
	err := c.ReadCloser.Close()
	c.cleanup()
	return err
}

// hostTar 把宿主机上的 roots（目录或文件）写成一个 tar 流。base 为 "/" 时，名字是完整
// 路径去掉开头的 /（挂载目录）；否则是相对于 base 的路径（Compose 项目目录）。
// 保留属主、权限、修改时间、软链接和设备文件；socket 没法放进 tar，跳过并记日志。
type hostTar struct {
	ctx      context.Context
	roots    []string
	base     string
	excludes []string
	bigLimit int64
	logf     func(string, ...any)
}

// stream 返回 tar 流；在另一个协程里边读文件边写，出错时读取端会收到这个错误。
func (h hostTar) stream() io.ReadCloser {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(h.write(pw)) }()
	return pr
}

func (h hostTar) write(w io.Writer) error {
	tw := tar.NewWriter(w)
	for _, root := range h.roots {
		if err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			return h.entry(tw, p, d, err)
		}); err != nil {
			return err
		}
	}
	return tw.Close()
}

func (h hostTar) entry(tw *tar.Writer, p string, d fs.DirEntry, err error) error {
	if err != nil {
		return fmt.Errorf("读取 %s 失败：%v", p, err)
	}
	if err := h.ctx.Err(); err != nil {
		return err
	}
	for _, e := range h.excludes {
		if p == e {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
	}
	info, err := os.Lstat(p)
	if err != nil {
		return fmt.Errorf("读取 %s 失败：%v", p, err)
	}
	mode := info.Mode()
	if mode&fs.ModeSocket != 0 {
		h.logf("跳过 socket 文件：%s", p)
		return nil
	}
	if h.bigLimit > 0 && mode.IsRegular() && info.Size() > h.bigLimit {
		return nil
	}
	name, err := h.name(p)
	if err != nil || name == "" {
		return err
	}
	link := ""
	if mode&fs.ModeSymlink != 0 {
		if link, err = os.Readlink(p); err != nil {
			return err
		}
	}
	hdr, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return err
	}
	hdr.Name = name
	if d.IsDir() {
		hdr.Name += "/"
	}
	hdr.Format = tar.FormatPAX
	hdr.Uname, hdr.Gname = "", "" // 只用数字属主，两台服务器的用户名可能不一样
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		hdr.Uid, hdr.Gid = int(st.Uid), int(st.Gid)
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if mode.IsRegular() {
		return copyFile(tw, p, hdr.Size, h.logf)
	}
	return nil
}

func (h hostTar) name(p string) (string, error) {
	if h.base == "/" {
		return strings.TrimPrefix(p, "/"), nil
	}
	rel, err := filepath.Rel(h.base, p)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return "", nil // 项目目录本身不用记，迁入时会创建
	}
	return filepath.ToSlash(rel), nil
}

// copyFile 写入一个文件的内容。不停机打包时文件可能正在变化：变长了只取开头
// size 字节，变短了用 0 补齐，保证 tar 格式完整，并记日志提醒。
func copyFile(w io.Writer, p string, size int64, logf func(string, ...any)) error {
	f, err := os.Open(p)
	if err != nil {
		return fmt.Errorf("读取 %s 失败：%v", p, err)
	}
	defer f.Close()
	n, err := io.CopyN(w, f, size)
	if err == nil {
		return nil
	}
	if !errors.Is(err, io.EOF) {
		return fmt.Errorf("读取 %s 失败：%v", p, err)
	}
	logf("文件 %s 在打包时变短了（%d → %d 字节），用 0 补齐", p, size, n)
	_, err = io.CopyN(w, zeroReader{}, size-n)
	return err
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// removeHelpers 删除上次被中断时留下的临时容器。
func removeHelpers(ctx context.Context, dc *docker.Client) {
	list, err := dc.Containers(ctx, true)
	if err != nil {
		return
	}
	for _, c := range list {
		if c.Labels[helperLabel] == "1" {
			_ = dc.RemoveContainer(ctx, c.ID, true)
		}
	}
}
