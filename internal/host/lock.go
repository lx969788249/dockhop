package host

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// ErrLocked 表示同一台服务器上已经有另一个任务在运行。
var ErrLocked = errors.New("这台服务器上已经有另一个 DockHop 任务在运行，请等它结束后再试")

// Lock 是一个文件锁。程序退出（包括被强制结束）时系统会自动释放它。锁文件只在拿着锁的时候
// 存在：Unlock 时删掉，不在服务器上留东西。
type Lock struct {
	f    *os.File
	path string
}

// TryLock 尝试加锁，已被占用时返回 ErrLocked。
func TryLock(path string) (*Lock, error) {
	// 拿到锁之后要确认锁文件还是打开的那一个：上一个拿着它的任务结束时会删掉它，在删掉之前打开、
	// 删掉之后才拿到锁的，拿到的是一个已经不在目录里的文件，后来的人还能新建一个再拿到锁。这时
	// 重新打开再拿。目录刚被收拾掉（打开时不存在）也重新建。
	for range 10 {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			f.Close()
			if errors.Is(err, unix.EWOULDBLOCK) {
				return nil, ErrLocked
			}
			return nil, err
		}
		if sameFile(f, path) {
			return &Lock{f: f, path: path}, nil
		}
		f.Close()
	}
	return nil, ErrLocked // 锁文件一直在变：有别的任务在频繁地拿、放
}

// sameFile 表示 f 就是现在 path 指向的那个文件。
func sameFile(f *os.File, path string) bool {
	a, err := f.Stat()
	if err != nil {
		return false
	}
	b, err := os.Stat(path)
	return err == nil && os.SameFile(a, b)
}

// Held 表示 path 这个锁现在被另一个打开者拿着（拿着它的任务还在运行）。只有确定没人拿着
// （锁文件不存在，或者自己能拿到）才返回 false；打不开、判断不了的当作有人拿着：宁可留着
// 一个已经没用的临时容器，也不能删掉别的任务正在用的。
func Held(path string) bool {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return !errors.Is(err, os.ErrNotExist)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.Is(err, unix.EWOULDBLOCK)
	}
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return false
}

// Unlock 删掉锁文件，再释放锁。先删后放：在这之间打开它的任务拿不到锁，之后的会新建一个。
func (l *Lock) Unlock() {
	if l == nil || l.f == nil {
		return
	}
	_ = os.Remove(l.path)
	_ = unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	l.f.Close()
	l.f = nil
}
