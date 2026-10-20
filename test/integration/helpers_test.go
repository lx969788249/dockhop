//go:build integration

// Package integration 在真实的 Docker 上测试 DockHop。会创建、停止、删除以
// 测试前缀命名的容器、卷和网络，只在测试机或 CI 上运行：
//
//	go test -tags integration ./test/integration -v
//
// DOCKHOP_BIN 指定要测试的程序；不指定时自动编译。
package integration

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var bin string

func TestMain(m *testing.M) {
	if _, err := exec.LookPath("docker"); err != nil {
		fmt.Println("跳过：没有 docker")
		os.Exit(0)
	}
	bin = os.Getenv("DOCKHOP_BIN")
	if bin == "" {
		dir, _ := os.MkdirTemp("", "dockhop-bin")
		bin = filepath.Join(dir, "dockhop")
		cmd := exec.Command("go", "build", "-o", bin, "../../cmd/dockhop")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Println("编译失败：", err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

// sh 运行一条命令，失败时让测试失败。
func sh(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s 失败：%v\n%s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// try 运行一条命令，忽略错误（用于清理）。
func try(name string, args ...string) string {
	out, _ := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(out))
}

// composeCmd 返回服务器上的 Compose 命令。
func composeCmd(t *testing.T) []string {
	if exec.Command("docker", "compose", "version").Run() == nil {
		return []string{"docker", "compose"}
	}
	if _, err := exec.LookPath("docker-compose"); err == nil {
		return []string{"docker-compose"}
	}
	t.Skip("没有 docker compose")
	return nil
}

func composeLegacy() bool {
	return exec.Command("docker", "compose", "version").Run() != nil
}

// state 返回容器状态（running、exited……）。
func state(t *testing.T, name string) string {
	t.Helper()
	return try("docker", "inspect", "-f", "{{.State.Status}}", name)
}

// containerByLabel 按 Compose 项目和服务找容器名（老 docker-compose 用下划线命名）。
func containerByLabel(t *testing.T, project, service string) string {
	t.Helper()
	return sh(t, "docker", "ps", "-a", "--filter", "label=com.docker.compose.project="+project,
		"--filter", "label=com.docker.compose.service="+service, "--format", "{{.Names}}")
}

// proc 是在后台运行的 dockhop。
type proc struct {
	cmd    *exec.Cmd
	mu     sync.Mutex
	output bytes.Buffer
	done   chan error
}

func start(t *testing.T, env []string, args ...string) *proc {
	t.Helper()
	p := &proc{done: make(chan error, 1)}
	p.cmd = exec.Command(bin, args...)
	p.cmd.Env = append(os.Environ(), env...)
	pr, pw := io.Pipe()
	p.cmd.Stdout, p.cmd.Stderr = pw, pw
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			p.mu.Lock()
			p.output.WriteString(sc.Text() + "\n")
			p.mu.Unlock()
		}
	}()
	go func() { err := p.cmd.Wait(); pw.Close(); p.done <- err }()
	t.Cleanup(func() { _ = p.cmd.Process.Kill() })
	return p
}

func (p *proc) out() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.output.String()
}

// waitFor 等输出里出现 re，返回匹配的内容。
func (p *proc) waitFor(t *testing.T, re *regexp.Regexp, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if m := re.FindString(p.out()); m != "" {
			return m
		}
		select {
		case err := <-p.done:
			p.done <- err
			t.Fatalf("dockhop 提前退出（%v）：\n%s", err, p.out())
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Fatalf("等待 %s 超时：\n%s", re, p.out())
	return ""
}

// wait 等进程结束，返回退出码。
func (p *proc) wait(t *testing.T, timeout time.Duration) int {
	t.Helper()
	select {
	case err := <-p.done:
		p.done <- err
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		if err != nil {
			t.Fatal(err)
		}
		return 0
	case <-time.After(timeout):
		t.Fatalf("dockhop 没有在 %s 内结束：\n%s", timeout, p.out())
		return -1
	}
}

func waitFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("等待 %s 超时", path)
}
