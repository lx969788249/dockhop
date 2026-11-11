package logging

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	r := &Redactor{}
	r.AddSecret("hunter2-very-secret")
	r.AddSecret("abc") // 太短，不登记，避免误伤
	cases := map[string]string{
		"数据库密码是 hunter2-very-secret":                                                           "数据库密码是 ***",
		"-e DB_PASSWORD=p@ss123 -e MODE=prod":                                                  "-e DB_PASSWORD=*** -e MODE=prod",
		"MYSQL_ROOT_PASSWORD=x&y":                                                              "MYSQL_ROOT_PASSWORD=***",
		"API_TOKEN=a,b;c 其他":                                                                   "API_TOKEN=*** 其他",
		"https://x/?token=abc&user=bob":                                                        "https://x/?token=***",
		`{"Env":["DB_PASSWORD=my secret&x","MODE=prod"]}`:                                      `{"Env":["DB_PASSWORD=***","MODE=prod"]}`,
		"docker run -e 'DB_PASSWORD=my secret' app":                                            "docker run -e 'DB_PASSWORD=***' app",
		`{"Env":["API_TOKEN=tok_123"],"Password": "s3cret"}`:                                   `{"Env":["API_TOKEN=***"],"Password": "***"}`,
		"  admin_password: changeme":                                                           "  admin_password: ***",
		"key is AGE-SECRET-KEY-1QQQ7ZZ":                                                        "key is ***",
		"https://1.2.3.4:8080/tok/#a7j8zg6qtzkg8ddwxnzl94th3c4ns7ckdzjxu0w0fypeapduldmqq2q4nn": "https://1.2.3.4:8080/tok/#***",
		"cannot connect to postgres://app:p%40ss!w0rd@db/app":                                  "cannot connect to postgres://app:***@db/app",
		"DSN=mysql://root:x@tcp(db)/app":                                                       "DSN=***",
		"see https://example.com/a@b":                                                          "see https://example.com/a@b",
		"abc 是普通文字":                                                                            "abc 是普通文字",
		"MODE=production":                                                                      "MODE=production",
	}
	// 写法隐藏只在一行里：引号没有配对时遮到行末，不能把后面的日志吞掉（失败时最需要日志）；
	// 值里转义的引号算在值里；没加引号的值遮到空白为止，中文标点也算在值里：分不出值在哪结束时宁可多藏。
	for _, c := range []struct{ in, want string }{
		{"-e \"DB_PASSWORD=abc\n后面这行日志要留着\nMODE=prod\"", "-e \"DB_PASSWORD=***\n后面这行日志要留着\nMODE=prod\""},
		{"'DB_PASSWORD=abc\n后面这行日志要留着'", "'DB_PASSWORD=***\n后面这行日志要留着'"},
		{`{"Env":["DB_PASSWORD=pa\"ss","MODE=prod"]}`, `{"Env":["DB_PASSWORD=***","MODE=prod"]}`},
		{"\"Password\": \"abc\n后面这行日志要留着\"", "\"Password\": \"***\"\n后面这行日志要留着\""},
		{"API_TOKEN=abc，def 后面是普通文字", "API_TOKEN=*** 后面是普通文字"},
		{"PASSWORD=abc，def", "PASSWORD=***"},
	} {
		cases[c.in] = c.want
	}
	for in, want := range cases {
		got := r.Redact(in)
		if got != want {
			t.Errorf("Redact(%q)\n实际 %q\n期望 %q", in, got, want)
		}
		if again := r.Redact(got); again != got {
			t.Errorf("隐藏过的再隐藏一次应不变：%q → %q", got, again)
		}
	}
}

func TestEnvSecrets(t *testing.T) {
	got := EnvSecrets([]string{"MYSQL_ROOT_PASSWORD=hunter2", "MODE=prod", "API_KEY=abc", "EMPTY_TOKEN=", "DATABASE_URL=postgres://u:p@db/x", "SENTRY_DSN=https://k@s.io/1"})
	want := []string{"hunter2", "abc", "postgres://u:p@db/x", "https://k@s.io/1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("EnvSecrets = %v", got)
	}
	var nilRedactor *Redactor
	nilRedactor.AddSecret("hunter2-very-secret")
	if got := nilRedactor.Redact("DB_PASSWORD=x 普通文字"); got != "DB_PASSWORD=*** 普通文字" {
		t.Errorf("nil 时仍按写法隐藏：%q", got)
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
	b, _ := os.ReadFile(l.Path())
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	last := lines[len(lines)-2:]
	if strings.Contains(string(b), "topsecret-value") || !strings.HasSuffix(last[0], "链接密钥 ***") || !strings.HasSuffix(last[1], "第二行") {
		t.Fatalf("密钥应隐藏、多行内容应各占一行：%q", last)
	}
	if !l.Finish(Failed, false) {
		t.Fatal("失败时应保留日志")
	}
	if _, err := os.Stat(l.Path()); err != nil {
		t.Fatalf("失败时日志应还在：%v", err)
	}

	l2, _ := Open(dir, "out", nil)
	if l2.Finish(Succeeded, false) {
		t.Fatal("成功时应删除日志")
	}
	if _, err := os.Stat(l2.Path()); !os.IsNotExist(err) {
		t.Fatal("成功时日志文件应被删除")
	}
	l3, _ := Open(dir, "out", nil)
	if !l3.Finish(Succeeded, true) {
		t.Fatal("要求保留时成功也应保留")
	}

	var nilLogger *Logger
	nilLogger.Printf("不会崩溃")
	if nilLogger.Path() != "" || nilLogger.Finish(Succeeded, false) {
		t.Fatal("nil Logger 应该什么也不做")
	}
}

// 什么都没做就取消：只删这一份。成功：连同一种任务以前失败留下的也删掉，别的任务的、更晚的
// （同时开始的另一个任务）不动；什么都不剩时目录也删掉。
func TestFinishDropsOldLogs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	os.MkdirAll(dir, 0o700)
	for _, name := range []string{"in-20260101-000000-1.log", "out-20260101-000000-2.log", "in-29991231-000000-3.log"} {
		os.WriteFile(filepath.Join(dir, name), []byte("旧日志"), 0o600)
	}
	left := func() []string {
		entries, _ := os.ReadDir(dir)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return names
	}
	l, _ := Open(dir, "in", nil)
	if l.Finish(Cancelled, false) || len(left()) != 3 {
		t.Fatalf("取消时只删这一份：%v", left())
	}
	l, _ = Open(dir, "in", nil)
	l.Finish(Succeeded, false)
	if got := strings.Join(left(), " "); got != "in-29991231-000000-3.log out-20260101-000000-2.log" {
		t.Fatalf("成功时应删掉同一种任务更早的日志：%s", got)
	}
	for _, name := range left() {
		os.Remove(filepath.Join(dir, name))
	}
	l, _ = Open(dir, "out", nil)
	l.Finish(Succeeded, false)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("什么都不剩时目录也应删掉：%v", err)
	}
}
