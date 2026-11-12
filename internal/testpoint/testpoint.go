// Package testpoint 是测试用的设置：验收测试在指定的地方模拟失败、停住（模拟迁移到一半被 Ctrl+C 或
// kill -9），把等待的时间改短；单元测试把等待的时间改短。都写在 Settings 里，经 Deps 传给迁入、迁出。
//
// 只有带 testpoint 标签编译的测试版（go build -tags testpoint）才从 DOCKHOP_TEST_ 开头的环境变量读这些
// 设置（见 FromEnv）。发布的程序里没有这些环境变量，Settings 永远是零值，照常运行。
package testpoint

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// Settings 是测试用的设置，零值表示照常运行。
type Settings struct {
	// 验收测试用环境变量设：
	Fail           []string      // 在这些地方模拟失败（DOCKHOP_TEST_FAIL=名字,名字）
	Pause          string        // 在这个地方停住，等测试删掉标记文件再继续（DOCKHOP_TEST_PAUSE=名字）
	KeepLog        bool          // 成功、取消时也留下日志（DOCKHOP_TEST_KEEP_LOG=1）
	ConnectTimeout time.Duration // 连旧服务器最多等多久（DOCKHOP_TEST_CONNECT_TIMEOUT=5s）
	StartupGrace   time.Duration // 容器启动后至少看多久，再判断它稳不稳（DOCKHOP_TEST_STARTUP_GRACE=2s）

	// 单元测试直接设：
	HealthWait   time.Duration // 等容器就绪至少等多久
	PullWindow   time.Duration // 从镜像仓库拉镜像，在这么长的时间里
	PullMinBytes int64         // 进展不到这么多就放弃
}

// Fails 表示要在 name 这个地方模拟失败。
func (s Settings) Fails(name string) bool { return slices.Contains(s.Fail, name) }

// PauseAt 在 name 这个地方停住：在 dir 下写一个标记文件 test-paused-名字，等它被删掉（或者 ctx 取消）
// 再继续。
func (s Settings) PauseAt(ctx context.Context, dir, name string) {
	if s.Pause != name {
		return
	}
	marker := filepath.Join(dir, "test-paused-"+name)
	_ = os.WriteFile(marker, nil, 0o600)
	for {
		if _, err := os.Stat(marker); err != nil || ctx.Err() != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
