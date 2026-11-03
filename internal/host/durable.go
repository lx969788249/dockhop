package host

import (
	"os"
	"path/filepath"
)

// 断电时，写进文件的内容和改名、新建这些目录里的变化，可能还在内存里、没有写到磁盘上。
// 回滚要靠的东西（事务日志、停机记录、原有数据的备份）必须在接下来的改动之前真正写到磁盘：
// 文件写完先 Sync，再让它出现在目录里（改名、新建）之后 Sync 所在的目录。

// SyncDir 把目录里的变化（新建、改名、删除）写到磁盘。
func SyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// WriteFileDurable 把 data 写到 path：先写临时文件并写到磁盘，再改名替换，最后把目录里的
// 变化也写到磁盘。任何时候断电，path 要么是原来的内容，要么是完整的新内容。
func WriteFileDurable(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return SyncDir(filepath.Dir(path))
}
