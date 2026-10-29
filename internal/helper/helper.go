// Package helper 管理 DockHop 创建的临时容器（读写数据卷用）：标上是哪个任务创建的，
// 清理时只删自己的、或者创建它的任务已经结束的。
//
// 同一台服务器上可能同时有迁出和迁入，或者 DOCKHOP_HOME 不同的两次任务在运行，它们的
// 临时容器正在读写数据，不能当作遗留的删掉。
package helper

import (
	"context"
	"path/filepath"

	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/host"
)

// Label 标记 DockHop 的临时容器，值是创建它的任务拿着的锁文件。老版本的值是 "1"。
const Label = "dockhop.helper"

// Labels 是拿着 lock 的任务创建临时容器时要带的标签。
func Labels(lock string) map[string]string { return map[string]string{Label: lock} }

// Clean 删除临时容器：拿着 lock 的这个任务的（包括这个任务上次被中断时留下的），以及
// 创建它的任务已经结束的（它的锁没人拿着）。只认标签，不看名字：用户自己的容器名字碰巧
// 以 dockhop-helper- 开头时不能删。
func Clean(ctx context.Context, dc *docker.Client, lock string) {
	list, err := dc.Containers(ctx, true)
	if err != nil {
		return
	}
	for _, c := range list {
		owner, ok := c.Labels[Label]
		switch {
		case !ok:
			continue
		case owner == lock:
		case owner == "1":
			// 老版本没有记是谁的：同一个目录里没有别的任务在运行时才删。
			if othersRunning(lock) {
				continue
			}
		case host.Held(owner):
			continue
		}
		_ = dc.RemoveContainerAndVolumes(ctx, c.ID)
	}
}

func othersRunning(lock string) bool {
	locks, _ := filepath.Glob(filepath.Join(filepath.Dir(lock), "*.lock"))
	for _, l := range locks {
		if l != lock && host.Held(l) {
			return true
		}
	}
	return false
}
