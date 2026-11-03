// Package testpoint 是给集成测试用的埋点：DOCKHOP_TEST_FAIL=名字,名字 在这些地方模拟失败；
// DOCKHOP_TEST_PAUSE=名字 在这个地方停住，等测试删掉标记文件再继续，用来模拟迁移到一半被 Ctrl+C
// 或 kill -9。平时不设置这两个环境变量，什么也不做。
package testpoint

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Fail 表示要在 name 这个地方模拟失败。getenv 为空时什么都不模拟。
func Fail(getenv func(string) string, name string) bool {
	if getenv == nil {
		return false
	}
	for _, p := range strings.Split(getenv("DOCKHOP_TEST_FAIL"), ",") {
		if strings.TrimSpace(p) == name {
			return true
		}
	}
	return false
}

// Pause 在 name 这个地方停住：在 dir 下写一个标记文件 test-paused-名字，等它被删掉（或者 ctx 取消）
// 再继续。
func Pause(ctx context.Context, getenv func(string) string, dir, name string) {
	if getenv == nil || getenv("DOCKHOP_TEST_PAUSE") != name {
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
