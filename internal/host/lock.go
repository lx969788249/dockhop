package host

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// ErrLocked 表示同一台服务器上已经有另一个任务在运行。
var ErrLocked = errors.New("这台服务器上已经有另一个 DockHop 任务在运行，请等它结束后再试")

// Lock 是一个文件锁。程序退出（包括被强制结束）时系统会自动释放它。
type Lock struct{ f *os.File }

// TryLock 尝试加锁，已被占用时返回 ErrLocked。
func TryLock(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
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
	return &Lock{f: f}, nil
}

// Held 表示 path 这个锁现在被另一个打开者拿着（拿着它的任务还在运行）。锁文件不存在时
// 当作没人拿着。
func Held(path string) bool {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.Is(err, unix.EWOULDBLOCK)
	}
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return false
}

// Unlock 释放锁。
func (l *Lock) Unlock() {
	if l == nil || l.f == nil {
		return
	}
	_ = unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	l.f.Close()
	l.f = nil
}
