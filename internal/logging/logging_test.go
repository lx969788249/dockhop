package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	r := &Redactor{}
	r.AddSecret("hunter2-very-secret")
	r.AddSecret("abc") // 太短，不登记，避免误伤
	cases := map[string]string{
		"数据库密码是 hunter2-very-secret":                                                          "数据库密码是 ***",
		"-e DB_PASSWORD=p@ss123 -e MODE=prod":                                                 "-e DB_PASSWORD=*** -e MODE=prod",
		"MYSQL_ROOT_PASSWORD=x&y":                                                             "MYSQL_ROOT_PASSWORD=***&y",
		`{"Env":["API_TOKEN=tok_123"],"Password": "s3cret"}`:                                  `{"Env":["API_TOKEN=***"],"Password": "***"}`,
		"  admin_password: changeme":                                                          "  admin_password: ***",
		"key is AGE-SECRET-KEY-1QQQ7ZZ":                                                       "key is ***",
		"http://1.2.3.4:8080/tok/#a7j8zg6qtzkg8ddwxnzl94th3c4ns7ckdzjxu0w0fypeapduldmqq2q4nn": "http://1.2.3.4:8080/tok/#***",
		"abc 是普通文字":                                                                           "abc 是普通文字",
		"MODE=production":                                                                     "MODE=production",
	}
	for in, want := range cases {
		if got := r.Redact(in); got != want {
			t.Errorf("Redact(%q)\n实际 %q\n期望 %q", in, got, want)
		}
	}
}

func TestLoggerLifecycle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	r := &Redactor{}
	r.AddSecret("topsecret-value")
	l, err := Open(dir, "in", r)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(l.Path())
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("日志文件权限应为 600，实际 %v", st.Mode().Perm())
	}
	for i := 1; i <= 30; i++ {
		l.Printf("第 %d 行", i)
	}
	l.Printf("链接密钥 topsecret-value\n第二行")
	tail := l.Tail(3)
	if len(tail) != 3 || !strings.HasSuffix(tail[1], "链接密钥 ***") || !strings.HasSuffix(tail[2], "第二行") {
		t.Fatalf("Tail：%q", tail)
	}
	if !l.Finish(false, false) {
		t.Fatal("失败时应保留日志")
	}
	if _, err := os.Stat(l.Path()); err != nil {
		t.Fatalf("失败时日志应还在：%v", err)
	}

	l2, _ := Open(dir, "out", nil)
	if l2.Finish(true, false) {
		t.Fatal("成功时应删除日志")
	}
	if _, err := os.Stat(l2.Path()); !os.IsNotExist(err) {
		t.Fatal("成功时日志文件应被删除")
	}
	l3, _ := Open(dir, "out", nil)
	if !l3.Finish(true, true) {
		t.Fatal("要求保留时成功也应保留")
	}

	var nilLogger *Logger
	nilLogger.Printf("不会崩溃")
	if nilLogger.Path() != "" || nilLogger.Finish(true, false) {
		t.Fatal("nil Logger 应该什么也不做")
	}
}
