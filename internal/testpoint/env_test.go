//go:build testpoint

package testpoint

import (
	"reflect"
	"testing"
	"time"
)

// 测试版从 DOCKHOP_TEST_ 开头的环境变量读设置；没设、写错的照常。
func TestFromEnv(t *testing.T) {
	env := map[string]string{
		"DOCKHOP_TEST_FAIL":            "volume:db, commit-cleanup",
		"DOCKHOP_TEST_PAUSE":           "after-stop",
		"DOCKHOP_TEST_KEEP_LOG":        "1",
		"DOCKHOP_TEST_CONNECT_TIMEOUT": "5s",
		"DOCKHOP_TEST_STARTUP_GRACE":   "两秒",
	}
	want := Settings{Fail: []string{"volume:db", "commit-cleanup"}, Pause: "after-stop", KeepLog: true, ConnectTimeout: 5 * time.Second}
	if got := FromEnv(func(k string) string { return env[k] }); !reflect.DeepEqual(got, want) {
		t.Errorf("读到的：%+v，应为 %+v", got, want)
	}
	if got := FromEnv(func(string) string { return "" }); !reflect.DeepEqual(got, Settings{}) {
		t.Errorf("没设时应照常：%+v", got)
	}
}
