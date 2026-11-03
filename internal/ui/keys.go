package ui

import (
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
//
// 读终端从不阻塞：每 0.1 秒最多等一次输入，有就读走现有的字节（Ctrl+C 可能刚好把输入清空，
// 这时读到的是 0 个字节），没有就看一眼要不要停。这样恢复终端时，读按键的协程总能在 0.1 秒内
// 结束，不会占着输入，后面的提问还要用。
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
	t.Cc[unix.VMIN], t.Cc[unix.VTIME] = 0, 0
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
		var kp keyParser
		buf := make([]byte, 64)
		for !stop.Load() {
			n, err := unix.Poll([]unix.PollFd{{Fd: int32(p.tty), Events: unix.POLLIN}}, 100)
			if err != nil && err != unix.EINTR {
				return
			}
			var got []Key
			if n > 0 {
				m, err := unix.Read(p.tty, buf)
				if err != nil && err != unix.EINTR && err != unix.EAGAIN {
					return
				}
				got = kp.feed(buf[:max(m, 0)])
			} else {
				got = kp.idle() // 0.1 秒没有新输入
			}
			for _, k := range got {
				select {
				case keys <- k:
				default: // 没人来取就算了
				}
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

// keyParser 把终端送来的字节流切成一个个键。方向键、功能键发来的是 Esc 开头的一串（CSI：
// Esc [ 参数… 结尾字符；SS3：Esc O 一个字符），算作一个 KeyOther；Esc 后面跟着别的字符时，
// 是先按了 Esc 再按那个键（网络慢时会一起到，例如 Esc 和回车）。一串字节可能分几次到：
// 没到齐的先留着，等下一次；等了一会儿还是没有新输入时（见 idle），单独的 Esc 就是按了 Esc。
type keyParser struct{ pending []byte }

func (p *keyParser) feed(b []byte) []Key {
	p.pending = append(p.pending, b...)
	var out []Key
	for len(p.pending) > 0 {
		k, n := parseKey(p.pending)
		if n == 0 {
			break // 一串还没到齐
		}
		out = append(out, k)
		p.pending = p.pending[n:]
	}
	return out
}

// idle 在一阵子没有新输入时调用：留着的单独的 Esc 就是按了 Esc，没到齐的一串算作 KeyOther。
func (p *keyParser) idle() []Key {
	if len(p.pending) == 0 {
		return nil
	}
	k := KeyOther
	if len(p.pending) == 1 && p.pending[0] == 0x1b {
		k = KeyEsc
	}
	p.pending = p.pending[:0]
	return []Key{k}
}

// parseKey 从 b 开头切出一个键，返回它和用掉的字节数；还没到齐时返回 0。
func parseKey(b []byte) (Key, int) {
	switch b[0] {
	case '\r', '\n':
		return KeyEnter, 1
	case 0x1b:
	default:
		return Key(b[0]), 1
	}
	if len(b) == 1 {
		return 0, 0
	}
	switch b[1] {
	case '[': // CSI：参数和中间字符（0x20–0x3F），以 0x40–0x7E 结尾
		for i := 2; i < len(b); i++ {
			if b[i] >= 0x40 && b[i] <= 0x7e {
				return KeyOther, i + 1
			}
			if b[i] < 0x20 || b[i] > 0x3f {
				return KeyOther, i // 不合规的一串：到这里为止，后面的照常读
			}
		}
		return 0, 0
	case 'O': // SS3：再跟一个字符
		if len(b) < 3 {
			return 0, 0
		}
		return KeyOther, 3
	}
	return KeyEsc, 1
}
