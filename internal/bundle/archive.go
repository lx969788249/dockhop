package bundle

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// 迁移包里卷、目录、Compose 项目的数据都是 tar。迁入时先完整检查一遍（ScanArchive），
// 没问题才开始改动新服务器；解开时只解到一个全新的临时目录（ExtractTree），
// 再整体换到目标位置。

// ScanArchive 检查一个 tar 里的每个成员，在改动任何数据之前发现不安全的内容：
//   - 路径必须是相对路径，不能有 ..，必须在某个 roots 下面（没有给 roots 时不限制）；
//   - 不能通过软链接或普通文件“穿过去”写到别处（例如先放一个指向 /etc 的软链接 a，
//     再放 a/passwd）；
//   - 硬链接只能指向包里的普通文件；同一个路径不能出现两次（目录除外）。
//
// 软链接本身原样保留，指向哪里（外部、悬空、循环）都不跟随。
func ScanArchive(r io.Reader, roots ...string) error { return scan(r, roots, true) }

// ScanChanges 和 ScanArchive 一样，只是不要求每个根都在包里、也允许根是软链接
// （容器里改动的路径：打包时可能已经被删掉，也可能本身就是软链接）。
func ScanChanges(r io.Reader, roots ...string) error { return scan(r, roots, false) }

func scan(r io.Reader, roots []string, rootsMustExist bool) error {
	roots = append([]string(nil), roots...)
	for i, root := range roots {
		roots[i] = strings.Trim(path.Clean("/"+root), "/")
	}
	kinds := map[string]byte{}
	links := map[string]string{}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("%w：数据读不了（%v）", ErrUnsafe, err)
		}
		name, err := memberPath(h.Name)
		if err != nil {
			return err
		}
		if len(roots) > 0 && !underAny(name, roots) {
			return fmt.Errorf("%w：%q 不在 %s 下面", ErrUnsafe, h.Name, strings.Join(roots, "、"))
		}
		if name == "" && h.Typeflag != tar.TypeDir {
			return fmt.Errorf("%w：根目录必须是目录", ErrUnsafe)
		}
		var kind byte
		switch h.Typeflag {
		case tar.TypeDir:
			kind = 'd'
		case tar.TypeReg:
			kind = 'f'
		case tar.TypeSymlink:
			if h.Linkname == "" || strings.ContainsRune(h.Linkname, 0) {
				return fmt.Errorf("%w：软链接 %q 的目标为空", ErrUnsafe, h.Name)
			}
			kind = 'l'
		case tar.TypeLink:
			target, err := memberPath(h.Linkname)
			if err != nil {
				return err
			}
			kind = 'h'
			links[name] = target
		case tar.TypeFifo, tar.TypeChar, tar.TypeBlock:
			kind = 's'
		default:
			return fmt.Errorf("%w：不支持的成员类型 %q（%s）", ErrUnsafe, h.Typeflag, h.Name)
		}
		if old, ok := kinds[name]; ok && !(old == 'd' && kind == 'd') {
			return fmt.Errorf("%w：%q 出现了两次", ErrUnsafe, h.Name)
		}
		kinds[name] = kind
	}
	for name := range kinds {
		for p := path.Dir(name); p != "." && p != "/"; p = path.Dir(p) {
			if k, ok := kinds[p]; ok && k != 'd' {
				return fmt.Errorf("%w：%q 要通过软链接或文件 %q 写入", ErrUnsafe, name, p)
			}
		}
	}
	for name, target := range links {
		seen := map[string]bool{}
		for kinds[target] == 'h' && !seen[target] {
			seen[target] = true
			target = links[target]
		}
		if kinds[target] != 'f' {
			return fmt.Errorf("%w：硬链接 %q 必须指向包里的普通文件", ErrUnsafe, name)
		}
	}
	for _, root := range roots {
		if !rootsMustExist {
			break
		}
		if k := kinds[root]; k != 'd' && k != 'f' {
			return fmt.Errorf("%w：缺少 %s 本身，或者它不是普通文件或目录", ErrUnsafe, root)
		}
	}
	return nil
}

func underAny(name string, roots []string) bool {
	for _, root := range roots {
		if root == "" || name == root || strings.HasPrefix(name, root+"/") {
			return true
		}
	}
	return false
}

func memberPath(raw string) (string, error) {
	if strings.HasPrefix(raw, "/") || strings.ContainsRune(raw, 0) {
		return "", fmt.Errorf("%w：%q 是绝对路径", ErrUnsafe, raw)
	}
	for _, seg := range strings.Split(raw, "/") {
		if seg == ".." {
			return "", fmt.Errorf("%w：%q 里有“..”", ErrUnsafe, raw)
		}
	}
	p := path.Clean("/" + raw)
	return strings.TrimPrefix(p, "/"), nil
}

// ExtractTree 把检查过的 tar 解到 dst（必须是新建的空目录）下，保留属主、权限、修改
// 时间、软链接、硬链接和特殊文件。不是 root 时没法改属主，按当前用户保存。
func ExtractTree(r io.Reader, dst string) error {
	root := os.Geteuid() == 0
	type dirMeta struct {
		path  string
		h     *tar.Header
		mtime time.Time
	}
	var dirs []dirMeta
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return wrapCorrupt(err)
		}
		name, err := memberPath(h.Name)
		if err != nil {
			return err
		}
		p := filepath.Join(dst, filepath.FromSlash(name))
		if name != "" {
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
		}
		mode := os.FileMode(h.Mode & 0o777)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o700); err != nil {
				return err
			}
			dirs = append(dirs, dirMeta{p, h, h.ModTime})
			continue
		case tar.TypeReg:
			f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return wrapCorrupt(err)
			}
		case tar.TypeSymlink:
			if err := os.Symlink(h.Linkname, p); err != nil {
				return err
			}
		case tar.TypeLink:
			target, _ := memberPath(h.Linkname)
			if err := os.Link(filepath.Join(dst, filepath.FromSlash(target)), p); err != nil {
				return err
			}
			continue
		case tar.TypeFifo:
			if err := unix.Mkfifo(p, uint32(mode)); err != nil {
				return err
			}
		case tar.TypeChar, tar.TypeBlock:
			if !root {
				continue // 只有 root 能创建设备文件
			}
			typ := uint32(unix.S_IFCHR)
			if h.Typeflag == tar.TypeBlock {
				typ = unix.S_IFBLK
			}
			if err := unix.Mknod(p, typ|uint32(mode), int(unix.Mkdev(uint32(h.Devmajor), uint32(h.Devminor)))); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w：不支持的成员类型（%s）", ErrUnsafe, h.Name)
		}
		if err := applyMeta(p, h, root); err != nil {
			return err
		}
	}
	// 目录最后处理：往目录里放文件会改变它的修改时间，权限也可能不允许再写入。
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := applyMeta(dirs[i].path, dirs[i].h, root); err != nil {
			return err
		}
	}
	return nil
}

func applyMeta(p string, h *tar.Header, root bool) error {
	if root {
		if err := os.Lchown(p, h.Uid, h.Gid); err != nil {
			return err
		}
	}
	if h.Typeflag != tar.TypeSymlink {
		// 改属主会清掉 setuid 位，所以权限放在属主之后设置。
		mode := os.FileMode(h.Mode & 0o777)
		if h.Mode&0o4000 != 0 {
			mode |= os.ModeSetuid
		}
		if h.Mode&0o2000 != 0 {
			mode |= os.ModeSetgid
		}
		if h.Mode&0o1000 != 0 {
			mode |= os.ModeSticky
		}
		if err := os.Chmod(p, mode); err != nil {
			return err
		}
	}
	ts := []unix.Timespec{unix.NsecToTimespec(h.ModTime.UnixNano()), unix.NsecToTimespec(h.ModTime.UnixNano())}
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, p, ts, unix.AT_SYMLINK_NOFOLLOW); err != nil && !errors.Is(err, unix.ENOTSUP) {
		return err
	}
	return nil
}
