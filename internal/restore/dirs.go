package restore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// 换掉宿主机上的一个目录有两种办法，每种的做、撤销（rollback.go 的第 2 遍）、提交后清理放在一起：
//   - 整个替换（日志记录 bind）：解到旁边的临时目录，原来的目录改名成备份，再把解好的改名过来；
//   - 原地换内容（日志记录 contents）：目录是单独挂载的（例如一块数据盘，不能改名），解在它里面，原来的
//     东西挪进它里面的备份目录，再把解好的挪进来。都在同一块盘上，每一步都只是改名。

// dirSwap 是换掉 path 这个目录的办法（见 newDirSwap）。
type dirSwap struct {
	path     string
	stage    string // 解压用的临时目录：整个替换时在旁边，原地换内容时在它里面（同一块盘上）
	contents *Entry // 原地换内容时，日志里记着原来有哪些东西的那一条；整个替换时为空
}

// newDirSwap 按 path 是不是单独挂载的目录选办法。判断不了时不动它：选错了办法，挂载点改名会失败，普通
// 目录也不该在里面换。原地换内容的，先把原来有哪些东西记进日志（要在建解压目录之前，见 recordContents）。
func (r *run) newDirSwap(path string, isDir bool) (*dirSwap, error) {
	d := &dirSwap{path: path, stage: siblingName(path, "stage")}
	if !isDir {
		return d, nil
	}
	mounted, err := r.MountPoint(path)
	if err != nil {
		return nil, fmt.Errorf("没法判断 %s 是不是单独挂载的目录：%v", path, err)
	}
	if !mounted {
		return d, nil
	}
	e, err := r.recordContents(path)
	if err != nil {
		return nil, err
	}
	d.stage, d.contents = innerName(path, "stage"), &e
	return d, nil
}

// swapIn 把解好的 staged 换到 d.path 上。
func (r *run) swapIn(d *dirSwap, staged string) error {
	if d.contents != nil {
		return r.replaceContents(*d.contents, d.stage, staged)
	}
	return r.replaceDir(staged, d.path)
}

// ---------- 整个替换（bind） ----------

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

// undoBind 撤销整个替换（见 replaceDir）：迁移过来的目录删掉，备份改回原来的名字；原来没有的，删掉新建的。
func (u *undo) undoBind(e Entry, note func(string, ...any)) {
	if e.Path == "" || e.Path == "/" {
		return
	}
	// 下面的做法会整个删除 Path：它是挂载点（单独挂载的磁盘）时绝不能这样做，那样会把整块盘上的
	// 东西删掉；挂载点只会原地换内容。判断不了时也不动。
	if mounted, err := u.mountPoint(e.Path); err != nil || mounted {
		note("%s 是单独挂载的目录（或者判断不了：%v），不能整个删除再放回，没有动它，请按回滚资料人工处理", e.Path, err)
		return
	}
	_, curErr := os.Lstat(e.Path)
	if !e.Existed {
		if curErr == nil {
			if err := os.RemoveAll(e.Path); err != nil {
				note("本次新建的目录 %s 没能删除：%v", e.Path, err)
			}
		}
		return
	}
	if _, err := os.Lstat(e.Backup); err != nil {
		if curErr == nil {
			return // 记下了但还没挪开原来的目录：现在的就是原来的
		}
		note("目录 %s 和它的备份都找不到了", e.Path)
		return
	}
	if curErr == nil {
		if err := os.RemoveAll(e.Path); err != nil {
			note("没能删除迁入的目录 %s：%v", e.Path, err)
			return
		}
	}
	if err := os.Rename(e.Backup, e.Path); err != nil {
		note("目录 %s 没能放回原来的：%v", e.Path, err)
	}
}

// cleanupBind 提交后删掉整个替换时留下的原来的目录。
func cleanupBind(e Entry) error {
	if !e.Existed {
		return nil
	}
	if err := os.RemoveAll(e.Backup); err != nil {
		return fmt.Errorf("旧目录 %s：%v", e.Backup, err)
	}
	return nil
}

// ---------- 原地换内容（contents） ----------

// recordContents 在原地换 dst 的内容之前（dst 是单独挂载的目录，不能改名，见 restoreBind），把 dst
// 原来有哪些东西和 dst 本身原来的属主、权限、修改时间记进日志，返回这一条。要在动 dst 之前记：
// 解压目录也建在 dst 里，建了它，dst 的修改时间就变了。回滚见 undoContents。
func (r *run) recordContents(dst string) (Entry, error) {
	orig, err := readMeta(dst)
	if err != nil {
		return Entry{}, err
	}
	cur, err := os.ReadDir(dst)
	if err != nil {
		return Entry{}, err
	}
	names := make([]string, 0, len(cur))
	for _, e := range cur {
		names = append(names, e.Name())
	}
	e := Entry{Op: opContents, Path: dst, Backup: innerName(dst, "old"), Names: names, Meta: &orig}
	return e, r.j.add(e)
}

// replaceContents 把解好的 staged 里的东西换进 e.Path（e 是 recordContents 记下的那一条）：先把原来的
// 东西挪进 e.Path 里的备份目录 e.Backup，再把 staged 里的东西挪进来，最后 e.Path 本身改成迁移过来的
// 属主、权限、修改时间（挪动之前就读好：挪走里面的东西会改掉目录的修改时间）。都在同一块盘上，
// 每一步都只是改名。
func (r *run) replaceContents(e Entry, stage, staged string) error {
	migratedMeta, err := readMeta(staged)
	if err != nil {
		return err
	}
	if err := os.Mkdir(e.Backup, 0o700); err != nil {
		return err
	}
	for _, n := range e.Names {
		if err := os.Rename(filepath.Join(e.Path, n), filepath.Join(e.Backup, n)); err != nil {
			return fmt.Errorf("没能先把 %s 里原来的 %s 挪开：%v", e.Path, n, err)
		}
	}
	migrated, err := os.ReadDir(staged)
	if err != nil {
		return err
	}
	for _, m := range migrated {
		if err := os.Rename(filepath.Join(staged, m.Name()), filepath.Join(e.Path, m.Name())); err != nil {
			return fmt.Errorf("没能把迁移过来的 %s 放进 %s：%v", m.Name(), e.Path, err)
		}
	}
	// 先删掉已经空了的解压目录（它在 e.Path 里，删它会改掉修改时间），再改 e.Path 本身的属性。
	if err := os.RemoveAll(stage); err != nil {
		return err
	}
	return applyMeta(e.Path, migratedMeta)
}

// undoContents 撤销原地换内容（见 replaceContents）：Path 里迁移过来的东西删掉，备份目录里原来的
// 挪回去，Path 本身改回日志里记的属主、权限、修改时间，最后删掉空了的备份目录。日志里记着 Path
// 原来有哪些东西（Names）：名字不在里面的，或者原来的那个已经挪进了备份目录的，才是迁移过来的；
// 原来的那个还没挪走（或者已经挪回来了）的留着。所以换到一半、回滚到一半被打断，从头再来一遍都
// 是安全的。
func (u *undo) undoContents(e Entry, note func(string, ...any)) {
	backed, err := os.ReadDir(e.Backup)
	if errors.Is(err, os.ErrNotExist) {
		// 还没开始挪原来的东西，或者已经全部放回去了（可能刚删掉备份目录就被打断，属性还没改回，
		// 再改一遍）；也可能是数据盘没挂上（备份在盘上，看不到）。只有 Path 还是挂载点，才能当成前两种。
		switch mounted, err := u.mountPoint(e.Path); {
		case err != nil:
			note("没法确认 %s 还挂着：%v", e.Path, err)
		case !mounted:
			note("%s 现在不是单独挂载的目录（数据盘没挂上？），原来的内容在盘上的 %s 里，挂上之后重新运行就会放回去",
				e.Path, filepath.Base(e.Backup))
		case e.Meta != nil:
			if err := applyMeta(e.Path, *e.Meta); err != nil {
				note("目录 %s 没能改回原来的属主、权限和修改时间：%v", e.Path, err)
			}
		}
		return
	}
	if err != nil {
		note("目录 %s 的备份读不了：%v", e.Path, err)
		return
	}
	inBackup := make(map[string]bool, len(backed))
	for _, b := range backed {
		inBackup[b.Name()] = true
	}
	original := make(map[string]bool, len(e.Names))
	for _, n := range e.Names {
		original[n] = true
	}
	cur, err := os.ReadDir(e.Path)
	if err != nil {
		note("目录 %s 读不了：%v", e.Path, err)
		return
	}
	for _, c := range cur {
		p := filepath.Join(e.Path, c.Name())
		if p == e.Backup || original[c.Name()] && !inBackup[c.Name()] {
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			note("没能删除迁入的 %s：%v", p, err)
			return
		}
	}
	for _, b := range backed {
		if err := os.Rename(filepath.Join(e.Backup, b.Name()), filepath.Join(e.Path, b.Name())); err != nil {
			note("目录 %s 里原来的 %s 没能放回去：%v", e.Path, b.Name(), err)
			return
		}
	}
	// 先删空了的备份目录再改属性：删它会改掉 Path 的修改时间。
	if err := os.Remove(e.Backup); err != nil {
		note("目录 %s 的备份目录没能删除：%v", e.Path, err)
		return
	}
	if e.Meta != nil {
		if err := applyMeta(e.Path, *e.Meta); err != nil {
			note("目录 %s 没能改回原来的属主、权限和修改时间：%v", e.Path, err)
		}
	}
}

// cleanupContents 提交后删掉原地换内容时挪进备份目录的原来的东西。
func cleanupContents(e Entry) error {
	if err := os.RemoveAll(e.Backup); err != nil {
		return fmt.Errorf("目录 %s 里原来的内容 %s：%v", e.Path, e.Backup, err)
	}
	return nil
}
