package host

import (
	"time"

	"golang.org/x/sys/unix"
)

// SetModTime 尽量把 path 本身（是软链接时不跟过去）的修改时间设成 t。设不上不算失败：有的网络盘、FUSE
// 不让改，有的文件系统不能给软链接设；修改时间不影响数据，也不影响程序能不能用，不值得为它让整次迁入失败。
func SetModTime(path string, t time.Time) {
	ts := unix.NsecToTimespec(t.UnixNano())
	_ = unix.UtimesNanoAt(unix.AT_FDCWD, path, []unix.Timespec{ts, ts}, unix.AT_SYMLINK_NOFOLLOW)
}
