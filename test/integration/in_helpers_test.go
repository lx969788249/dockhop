//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 迁入的验收测试在同一台机器上进行：先迁出，再把原来的容器、卷、网络、目录删掉
// （相当于一台全新的服务器），然后迁入，比较前后是否一致。

// noWatchtower 让服务器上的 watchtower 不去更新测试容器。
const noWatchtower = "com.centurylinklabs.watchtower.enable=false"

var portSeq atomic.Int32

func nextPort() int { return freePort() + 20 + int(portSeq.Add(1)) }

// migration 是一次迁出：迁出在后台运行、提供下载，直到迁入成功或测试结束。
type migration struct {
	out  *proc
	link string
}

func startOut(t *testing.T, args ...string) *migration {
	t.Helper()
	home := t.TempDir()
	all := append([]string{"out", "--stop-shared", "--host", "127.0.0.1", "--port", fmt.Sprint(nextPort())}, args...)
	p := start(t, []string{"DOCKHOP_HOME=" + home, "DOCKHOP_KEEP_LOG=1"}, all...)
	m := &migration{out: p, link: p.waitFor(t, linkPattern, 10*time.Minute)}
	t.Cleanup(func() {
		_ = p.cmd.Process.Signal(os.Interrupt)
		select {
		case <-p.done:
		case <-time.After(time.Minute):
		}
	})
	return m
}

// in 运行一次迁入，返回输出和退出码。
func (m *migration) in(t *testing.T, home string, env []string, args ...string) (string, int) {
	t.Helper()
	return runIn(t, home, env, append([]string{"in", "--yes", m.link}, args...)...)
}

func runIn(t *testing.T, home string, env []string, args ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(append(os.Environ(), "DOCKHOP_HOME="+home, "DOCKHOP_KEEP_LOG=1", "DOCKHOP_TEST_STARTUP_GRACE=2s"), env...)
	b, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("迁入没能运行：%v", err)
	}
	out := strings.ReplaceAll(string(b), m58(string(b)), "<密钥>")
	t.Logf("迁入输出（退出码 %d）：\n%s", code, out)
	return out, code
}

// m58 找出输出里的链接密钥（测试日志里不显示）。
func m58(s string) string {
	if m := linkPattern.FindString(s); m != "" {
		return m[strings.Index(m, "#")+1:]
	}
	return "\x00"
}

// waitOutSuccess 等迁出收到“迁入成功”后自己结束。
func (m *migration) waitOutSuccess(t *testing.T) {
	t.Helper()
	if code := m.out.wait(t, 2*time.Minute); code != 0 {
		t.Fatalf("迁出应在新服务器迁入成功后自动结束，退出码 %d：\n%s", code, m.out.out())
	}
	if !strings.Contains(m.out.out(), "新服务器已迁入成功") {
		t.Errorf("迁出应显示新服务器已迁入成功：\n%s", m.out.out())
	}
}

func mustContain(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("输出里缺少「%s」", w)
		}
	}
}

func inspect(t *testing.T, name string) map[string]any {
	t.Helper()
	b, err := exec.Command("docker", "inspect", "--type", "container", name).Output()
	if err != nil {
		t.Fatalf("读不到容器 %s：%v", name, err)
	}
	var list []map[string]any
	json.Unmarshal(b, &list)
	return list[0]
}

func exists(kind, name string) bool {
	return exec.Command("docker", kind, "inspect", name).Run() == nil
}

// exec1 以 root 身份在容器里执行命令，返回输出。
func exec1(t *testing.T, container string, script string) string {
	t.Helper()
	return sh(t, "docker", "exec", "-u", "0", container, "sh", "-c", script)
}

// volumeFiles 列出卷里的文件（路径、属主、权限、内容或链接目标），用来比较是否完全一样。
func volumeFiles(t *testing.T, volume string) string {
	t.Helper()
	return sh(t, "docker", "run", "--rm", "--label", noWatchtower, "-v", volume+":/v:ro", "alpine:3.20", "sh", "-c",
		`cd /v && find . | sort | while read f; do
		  if [ -L "$f" ]; then echo "$f -> $(readlink "$f")";
		  elif [ -f "$f" ]; then echo "$f $(stat -c '%u:%g %a' "$f") $(cat "$f")";
		  else echo "$f/ $(stat -c '%u:%g %a' "$f")"; fi; done`)
}

// dirFiles 列出宿主机目录里的文件。
func dirFiles(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		line := rel + " " + info.Mode().String()
		if info.Mode()&os.ModeSymlink != 0 {
			l, _ := os.Readlink(p)
			line += " -> " + l
		} else if info.Mode().IsRegular() {
			b, _ := os.ReadFile(p)
			line += " " + string(b)
		}
		lines = append(lines, line)
		return nil
	})
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// noLeftovers 确认迁入结束后没有留下临时容器和回滚资料。
func noLeftovers(t *testing.T, home string) {
	t.Helper()
	if h := try("docker", "ps", "-aq", "--filter", "label=dockhop.helper"); h != "" {
		t.Errorf("临时容器没有删除：%s", h)
	}
	// 做带删除改动的镜像用的临时容器没有标签（标签会进到镜像里），按名字查。
	if h := try("docker", "ps", "-a", "--filter", "name=dockhop-helper-[0-9]", "--format", "{{.Names}}"); h != "" {
		t.Errorf("临时容器没有删除：%s", h)
	}
	if left, _ := filepath.Glob(filepath.Join(home, "rollback", "*")); len(left) > 0 {
		t.Errorf("回滚资料没有删除：%v", left)
	}
	if backups := try("docker", "ps", "-a", "--filter", "name=dockhop-backup", "--format", "{{.Names}}"); backups != "" {
		t.Errorf("改名备份的旧容器没有删除：%s", backups)
	}
}

func removeAll(names ...string) {
	for _, n := range names {
		try("docker", "rm", "-fv", n)
	}
}

func removeProject(project string) {
	for _, id := range strings.Fields(try("docker", "ps", "-aq", "--filter", "label=com.docker.compose.project="+project)) {
		try("docker", "rm", "-fv", id)
	}
	for _, n := range strings.Fields(try("docker", "network", "ls", "-q", "--filter", "label=com.docker.compose.project="+project)) {
		try("docker", "network", "rm", n)
	}
	for _, v := range strings.Fields(try("docker", "volume", "ls", "-q", "--filter", "label=com.docker.compose.project="+project)) {
		try("docker", "volume", "rm", v)
	}
}

func removePrefixed(prefix string) {
	for _, id := range strings.Fields(try("docker", "ps", "-aq", "--filter", "name="+prefix)) {
		try("docker", "rm", "-fv", id)
	}
	for _, v := range strings.Fields(try("docker", "volume", "ls", "-q", "--filter", "name="+prefix)) {
		try("docker", "volume", "rm", v)
	}
	for _, n := range strings.Fields(try("docker", "network", "ls", "-q", "--filter", "name="+prefix)) {
		try("docker", "network", "rm", n)
	}
	for _, img := range strings.Fields(try("docker", "images", "-q", "--filter", "reference=dockhop-image")) {
		try("docker", "rmi", "-f", img)
	}
}
