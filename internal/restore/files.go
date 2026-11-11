package restore

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/lx969788249/dockhop/internal/host"
)

// 宿主机文件操作：目录整体替换、Compose 项目文件逐个安装，以及它们的回滚。

// readMeta 读出 path 本身（是软链接时不跟过去）的属主、权限和修改时间。
func readMeta(path string) (FileMeta, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return FileMeta{}, err
	}
	return metaOf(info), nil
}

func metaOf(info fs.FileInfo) FileMeta {
	m := FileMeta{Mode: info.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky), MTime: info.ModTime().UnixNano(),
		Link: info.Mode()&fs.ModeSymlink != 0}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		m.UID, m.GID = int(st.Uid), int(st.Gid)
	}
	return m
}

// applyMeta 把属主、权限和修改时间设到 path 本身上（是软链接时不跟过去）：属主只有 root 能改成别人；
// 改属主会清掉 setuid 位，所以权限放在属主之后设置，软链接没有自己的权限；修改时间最后设置，设不上
// 不算失败（见 host.SetModTime）。
func applyMeta(path string, m FileMeta) error {
	if os.Geteuid() == 0 {
		if err := os.Lchown(path, m.UID, m.GID); err != nil {
			return err
		}
	}
	if !m.Link {
		if err := os.Chmod(path, m.Mode); err != nil {
			return err
		}
	}
	host.SetModTime(path, time.Unix(0, m.MTime))
	return nil
}

// siblingName 是 path 旁边的一个临时名字，例如 /srv/.data.dockhop-old-ab12cd。
func siblingName(path, kind string) string {
	return filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".dockhop-"+kind+"-"+runID)
}

// innerName 是目录 path 里面的一个临时名字，例如 /data/.dockhop-old-ab12cd（见 replaceContents）。
func innerName(path, kind string) string {
	return filepath.Join(path, ".dockhop-"+kind+"-"+runID)
}

// runID 区分不同次的迁入，临时文件名里用到。
var runID = fmt.Sprintf("%d", os.Getpid())

// mkdirAll 逐级创建目录，新建的每一级都记进日志（回滚时删掉空的）。
func (r *migrate) mkdirAll(dir string, mode os.FileMode) error {
	if st, err := os.Lstat(dir); err == nil {
		if st.IsDir() {
			return nil
		}
		return fmt.Errorf("%s 已存在，但不是目录", dir)
	}
	if parent := filepath.Dir(dir); parent != dir {
		if err := r.mkdirAll(parent, 0o755); err != nil {
			return err
		}
	}
	if err := r.j.add(Entry{Op: opMkdir, Path: dir}); err != nil {
		return err
	}
	if err := os.Mkdir(dir, mode); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

// installTree 把 staging 里的文件逐个放到 dest 下（合并，不删除 dest 里原有的其他文件）。
// 覆盖的文件先备份到回滚资料目录；skip 返回 true 的路径不动。
func (r *migrate) installTree(staging, dest string, skip func(string) bool) error {
	return filepath.WalkDir(staging, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(staging, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		if rel == "." {
			return r.mkdirAll(target, 0o755)
		}
		if skip != nil && skip(target) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		cur, cerr := os.Lstat(target)
		if d.IsDir() {
			if cerr == nil && cur.IsDir() {
				return nil // 已有的目录保持原样
			}
			if cerr == nil {
				if err := r.preserve(target); err != nil {
					return err
				}
			}
			if err := r.mkdirAll(target, info.Mode().Perm()); err != nil {
				return err
			}
			return applyMeta(target, metaOf(info))
		}
		if cerr == nil {
			if cur.IsDir() {
				return fmt.Errorf("%s 在新服务器上是目录，迁移过来的是文件，没法覆盖", target)
			}
			if err := r.preserve(target); err != nil {
				return err
			}
		} else if err := r.j.add(Entry{Op: opFile, Path: target}); err != nil {
			return err
		}
		if err := r.mkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return copyEntry(p, target, info)
	})
}

// preserve 把将被覆盖的文件挪到回滚资料目录，并记进日志。
func (r *migrate) preserve(target string) error {
	backup := r.j.file("file")
	if err := r.j.add(Entry{Op: opFile, Path: target, Existed: true, Backup: backup}); err != nil {
		return err
	}
	return moveOrCopy(target, backup)
}

// undoFile 删除这次写入的文件（见 installTree），原来有的（见 preserve）放回去。
func undoFile(e Entry, note func(string, ...any)) {
	if e.Existed {
		if _, err := os.Lstat(e.Backup); err != nil {
			return // 备份已经放回去了（之前撤销过），或者还没来得及挪走原来的文件
		}
	}
	if err := os.RemoveAll(e.Path); err != nil {
		note("没能删除 %s：%v", e.Path, err)
		return
	}
	if e.Existed {
		if err := moveOrCopy(e.Backup, e.Path); err != nil {
			note("文件 %s 没能放回原来的：%v", e.Path, err)
		}
	}
}

// moveOrCopy 移动文件或目录；不在同一块磁盘上时复制后删除原来的。复制先写到 dst 旁边的
// 临时名字，完整写完才改名成 dst：回滚只看 dst 在不在，复制到一半失败或被强制结束时，
// 不能留下一个看起来像完整备份、实际只有一半的 dst，否则回滚会拿它覆盖完整的原件。
func moveOrCopy(src, dst string) error {
	err := rename(src, dst)
	if err == nil || !errors.Is(err, syscall.EXDEV) {
		return err
	}
	tmp := dst + ".dockhop-partial"
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := copyTree(src, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	// 复制出来的这一份要先确实写到磁盘，才能删掉原来的。
	if err := host.SyncDir(filepath.Dir(dst)); err != nil {
		return err
	}
	return os.RemoveAll(src)
}

// rename 测试时换成模拟跨磁盘的版本。
var rename = os.Rename

// copyTree 复制文件或目录，保留属主、权限、修改时间和软链接。
func copyTree(src, dst string) error {
	var dirs []string
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			dirs = append(dirs, target)
			return applyMeta(target, metaOf(info))
		}
		return copyEntry(p, target, info)
	})
	if err != nil {
		return err
	}
	// 文件写完时已经各自写到磁盘；再把每个目录里的变化也写到磁盘，从里往外。
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := host.SyncDir(dirs[i]); err != nil {
			return err
		}
	}
	return nil
}

// copyEntry 复制一个非目录的文件（普通文件、软链接、FIFO）。
func copyEntry(src, dst string, info fs.FileInfo) error {
	_ = os.Remove(dst)
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		link, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if err := os.Symlink(link, dst); err != nil {
			return err
		}
	case info.Mode()&fs.ModeNamedPipe != 0:
		if err := unix.Mkfifo(dst, uint32(info.Mode().Perm())); err != nil {
			return err
		}
	case info.Mode().IsRegular():
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		if err == nil {
			err = out.Sync()
		}
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	default:
		return nil // 设备文件、socket 不复制
	}
	return applyMeta(dst, metaOf(info))
}

// clearDir 删除目录里的全部内容（保留目录本身）。在临时容器里运行，用来清空数据卷。
func clearDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// Helper 是在临时容器里运行的内部命令：__clear 目录（清空目录里的内容）、
// __remove 路径……（删除这些路径）。
func Helper(cmd string, args []string) error {
	switch cmd {
	case "__clear":
		if len(args) != 1 {
			return errors.New("用法：__clear 目录")
		}
		return clearDir(args[0])
	case "__remove":
		for _, p := range args {
			if !filepath.IsAbs(p) || filepath.Clean(p) == "/" {
				return fmt.Errorf("拒绝删除 %q", p)
			}
			if err := os.RemoveAll(p); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("不认识的内部命令 %s", cmd)
}
