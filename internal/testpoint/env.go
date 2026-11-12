//go:build testpoint

package testpoint

import (
	"strings"
	"time"
)

// FromEnv 读验收测试设的环境变量（DOCKHOP_TEST_ 开头，见 Settings）；没设、写错的照常。只有测试版这样读，
// 发布的程序见 release.go。
func FromEnv(getenv func(string) string) Settings {
	s := Settings{Pause: getenv("DOCKHOP_TEST_PAUSE"), KeepLog: getenv("DOCKHOP_TEST_KEEP_LOG") == "1"}
	for _, p := range strings.Split(getenv("DOCKHOP_TEST_FAIL"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			s.Fail = append(s.Fail, p)
		}
	}
	s.ConnectTimeout, _ = time.ParseDuration(getenv("DOCKHOP_TEST_CONNECT_TIMEOUT"))
	s.StartupGrace, _ = time.ParseDuration(getenv("DOCKHOP_TEST_STARTUP_GRACE"))
	return s
}
