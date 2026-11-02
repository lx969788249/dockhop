package ui

import "golang.org/x/sys/unix"

// 和 termios_linux.go 一样，只为了开发时能在 macOS 上编译和测试。
const (
	ioctlGetTermios      = unix.TIOCGETA
	ioctlSetTermios      = unix.TIOCSETA
	ioctlSetTermiosFlush = unix.TIOCSETAF
)
