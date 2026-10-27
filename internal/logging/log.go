// Package logging 负责运行日志和诊断文件。
//
// 屏幕只显示步骤、进度、警告和结果；每一步的细节、执行的操作和返回的错误都
// 写进日志。成功后日志自动删除，失败时保留并在结果里给出位置，方便用户反馈。
// 写入前统一隐藏密钥和密码。
package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Logger 写一份运行日志。零值和 nil 都可以安全使用（什么也不写）。
type Logger struct {
	mu       sync.Mutex
	f        *os.File
	path     string
	redactor *Redactor
	now      func() time.Time
}

// Open 在 dir 下创建一份日志，文件名形如 in-20261021-120000-1234.log。
// 目录和文件只有当前用户能读。
func Open(dir, kind string, redactor *Redactor) (*Logger, error) {
	if redactor == nil {
		redactor = &Redactor{}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	_ = os.Chmod(dir, 0o700)
	name := fmt.Sprintf("%s-%s-%d.log", kind, time.Now().Format("20060102-150405"), os.Getpid())
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	return &Logger{f: f, path: path, redactor: redactor, now: time.Now}, nil
}

// Path 是日志文件的位置；没有日志时为空。
func (l *Logger) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Printf 追加一行带时间的日志。
func (l *Logger) Printf(format string, args ...any) {
	if l == nil || l.f == nil {
		return
	}
	msg := l.redactor.Redact(fmt.Sprintf(format, args...))
	l.mu.Lock()
	defer l.mu.Unlock()
	ts := l.now().Format("15:04:05")
	for _, line := range strings.Split(strings.TrimRight(msg, "\n"), "\n") {
		fmt.Fprintf(l.f, "[%s] %s\n", ts, line)
	}
}

// Block 原样写入一段多行内容（如环境信息），同样会隐藏秘密。
func (l *Logger) Block(text string) {
	if l == nil || l.f == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprint(l.f, l.redactor.Redact(text))
	if !strings.HasSuffix(text, "\n") {
		fmt.Fprintln(l.f)
	}
}

// Finish 结束日志：成功且没有要求保留时删除日志文件，返回日志是否还在。
func (l *Logger) Finish(success, keep bool) bool {
	if l == nil || l.f == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.f.Close()
	l.f = nil
	if success && !keep {
		_ = os.Remove(l.path)
		_ = os.Remove(filepath.Dir(l.path)) // 目录空了就顺手删掉
		return false
	}
	return true
}
