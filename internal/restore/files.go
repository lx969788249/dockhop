package restore

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// 宿主机文件操作：目录整体替换、Compose 项目文件逐个安装，以及它们的回滚。

// replaceDir 把解好的 staged 换到 dst：先在日志里记下，再把原来的 dst 改名成 old，
// 最后把 staged 改名成 dst。三个路径在同一个目录下，改名是原子的。
func (r *run) replaceDir(staged, dst string) error {
	old := siblingName(dst, "old")
	_, err := os.Lstat(dst)
	existed := err == nil
	if err := r.j.add(Entry{Op: opBind, Path: dst, Existed: existed, Backup: old}); err != nil {
		return err
	}
	if existed {
		if err := os.Rename(dst, old); err != nil {
			return fmt.Errorf("没能先把原来的 %s 挪开：%v", dst, err)
		}
	}
	if err := os.Rename(staged, dst); err != nil {
		if existed {
			_ = os.Rename(old, dst)
		}
		return fmt.Errorf("没能把迁移过来的数据放到 %s：%v", dst, err)
	}
	return nil
}

// siblingName 是 path 旁边的一个临时名字，例如 /srv/.data.dockhop-old-ab12cd。
func siblingName(path, kind string) string {
	return filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".dockhop-"+kind+"-"+runID)
}

// runID 区分不同次的迁入，临时文件名里用到。
var runID = fmt.Sprintf("%d", os.Getpid())

// mkdirAll 逐级创建目录，新建的每一级都记进日志（回滚时删掉空的）。
func (r *run) mkdirAll(dir string, mode os.FileMode) error {
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
func (r *run) installTree(staging, dest string, skip func(string) bool) error {
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
			return copyMeta(p, target, info)
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
func (r *run) preserve(target string) error {
	backup := r.j.file("file")
	if err := r.j.add(Entry{Op: opFile, Path: target, Existed: true, Backup: backup}); err != nil {
		return err
	}
	return moveOrCopy(target, backup)
}

// moveOrCopy 移动文件或目录；不在同一块磁盘上时复制后删除原来的。
func moveOrCopy(src, dst string) error {
	err := os.Rename(src, dst)
	if err == nil || !errors.Is(err, syscall.EXDEV) {
		return err
	}
	if err := copyTree(src, dst); err != nil {
		return err
	}
	return os.RemoveAll(src)
}

// copyTree 复制文件或目录，保留属主、权限、修改时间和软链接。
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
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
			return copyMeta(p, target, info)
		}
		return copyEntry(p, target, info)
	})
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
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	default:
		return nil // 设备文件、socket 不复制
	}
	return copyMeta(src, dst, info)
}

func copyMeta(src, dst string, info fs.FileInfo) error {
	if st, ok := info.Sys().(*syscall.Stat_t); ok && os.Geteuid() == 0 {
		if err := os.Lchown(dst, int(st.Uid), int(st.Gid)); err != nil {
			return err
		}
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		if err := os.Chmod(dst, info.Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky)); err != nil {
			return err
		}
	}
	mt := unix.NsecToTimespec(info.ModTime().UnixNano())
	_ = unix.UtimesNanoAt(unix.AT_FDCWD, dst, []unix.Timespec{mt, mt}, unix.AT_SYMLINK_NOFOLLOW)
	return nil
}

// within 表示 p 是否就是 dir 或在 dir 里面。
func within(p, dir string) bool {
	p, dir = filepath.Clean(p), filepath.Clean(dir)
	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
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
