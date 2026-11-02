package ui

import "golang.org/x/sys/unix"

// 读写终端设置的 ioctl（见 keys.go）。程序只在 Linux 上运行；termios_darwin.go 让开发时
// 能在 macOS 上编译和测试。
const (
	ioctlGetTermios      = unix.TCGETS
	ioctlSetTermios      = unix.TCSETS
	ioctlSetTermiosFlush = unix.TCSETSF // 改设置的同时丢掉还没读走的输入
)
