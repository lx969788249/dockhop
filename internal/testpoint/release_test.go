//go:build !testpoint

package testpoint

import (
	"reflect"
	"testing"
)

// 发布的程序（不带 testpoint 标签编译）不认测试用的环境变量：设了也照常运行。
func TestReleaseIgnoresEnv(t *testing.T) {
	if got := FromEnv(func(string) string { return "volume:db" }); !reflect.DeepEqual(got, Settings{}) {
		t.Errorf("发布的程序不应读测试用的设置：%+v", got)
	}
}
