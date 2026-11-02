package ui

import (
	"bufio"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// Key 是按下的一个键：KeyEsc、KeyEnter，或者这个键的字符（方向键之类算 KeyOther）。
type Key rune

const (
	KeyOther Key = -1
	KeyEsc   Key = 0x1b
	KeyEnter Key = '\n'
)

// Keys 让终端改为直接读按键：不用按回车，也不显示按了什么；Ctrl+C 照常有效。返回按下的键和
// 恢复终端的函数，用完必须调用。不能交互、输入不是终端时返回 nil：从 nil 读会一直等，
// 就像一直没人按键。
func (p *Prompter) Keys() (<-chan Key, func()) {
	none := func() {}
	if !p.Interactive() || p.tty < 0 || p.lines != nil {
		return nil, none
	}
	old, err := unix.IoctlGetTermios(p.tty, ioctlGetTermios)
	if err != nil {
		return nil, none
	}
	t := *old
	t.Lflag &^= unix.ICANON | unix.ECHO
	t.Cc[unix.VMIN], t.Cc[unix.VTIME] = 1, 0
	// 同时丢掉之前随手按下、还没读走的键，免得一进来就当成按了什么。
	if err := unix.IoctlSetTermios(p.tty, ioctlSetTermiosFlush, &t); err != nil {
		return nil, none
	}
	_, _ = p.in.Discard(p.in.Buffered())

	keys := make(chan Key, 16)
	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			// 没有按键时每 0.1 秒看一眼要不要停：恢复终端后不能再占着输入，后面提问还要用。
			if p.in.Buffered() == 0 {
				n, err := unix.Poll([]unix.PollFd{{Fd: int32(p.tty), Events: unix.POLLIN}}, 100)
				if err != nil && err != unix.EINTR {
					return
				}
				if n <= 0 || stop.Load() {
					continue
				}
			}
			k, err := readKey(p.in)
			if err != nil {
				return
			}
			select {
			case keys <- k:
			default: // 没人来取就算了
			}
		}
	}()
	var once sync.Once
	return keys, func() {
		once.Do(func() {
			stop.Store(true)
			<-done
			_ = unix.IoctlSetTermios(p.tty, ioctlSetTermios, old)
		})
	}
}

// readKey 读一个键。方向键之类发来的是 Esc 开头的一串字符，一次就全到了，算作一个 KeyOther；
// 只按了 Esc 时，后面没有紧跟着的字符。
func readKey(in *bufio.Reader) (Key, error) {
	b, err := in.ReadByte()
	if err != nil {
		return 0, err
	}
	switch b {
	case 0x1b:
		if n := in.Buffered(); n > 0 {
			_, _ = in.Discard(n)
			return KeyOther, nil
		}
		return KeyEsc, nil
	case '\r', '\n':
		return KeyEnter, nil
	}
	return Key(b), nil
}
