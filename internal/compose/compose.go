// Package compose 调用服务器上已安装的 docker compose：算配置摘要、找出配置引用的
// 文件、启动项目。Compose 的配置规则复杂，自己解析容易出错，所以一律交给它本身。
package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Tool 是一个可用的 Compose 命令。
type Tool struct {
	cmd     []string // 例如 ["docker", "compose"] 或 ["docker-compose"]
	Version string
}

// ErrNotFound 表示服务器上没有安装 Compose。
var ErrNotFound = errors.New("没有找到 docker compose")

// Detect 找出服务器上的 Compose：优先用 docker compose 插件，其次是独立的 docker-compose。
func Detect(ctx context.Context) (*Tool, error) {
	for _, cmd := range [][]string{{"docker", "compose"}, {"docker-compose"}} {
		if _, err := exec.LookPath(cmd[0]); err != nil {
			continue
		}
		out, err := run(ctx, "", append(append([]string{}, cmd...), "version", "--short"))
		if err != nil {
			continue
		}
		return &Tool{cmd: cmd, Version: strings.TrimPrefix(strings.TrimSpace(out), "v")}, nil
	}
	return nil, ErrNotFound
}

// Legacy 表示是老的 Python 版 docker-compose（1.x），它不支持部分新功能。
func (t *Tool) Legacy() bool { return strings.HasPrefix(t.Version, "1.") }

// String 返回命令名，用于日志和提示。
func (t *Tool) String() string { return strings.Join(t.cmd, " ") + " " + t.Version }

// Project 是一个 Compose 项目的位置。
type Project struct {
	Name       string
	WorkingDir string
	Files      []string // 启动时用的配置文件（绝对路径）
}

func (t *Tool) args(p Project, extra ...string) []string {
	a := append([]string{}, t.cmd...)
	a = append(a, "-p", p.Name)
	for _, f := range p.Files {
		a = append(a, "-f", f)
	}
	return append(a, extra...)
}

// ConfigHashes 按现在的文件算出每个服务的配置摘要。和容器标签
// com.docker.compose.config-hash 不一致，说明启动后改过文件，或启动时用了
// shell 里临时设置的变量。
func (t *Tool) ConfigHashes(ctx context.Context, p Project) (map[string]string, error) {
	out, err := run(ctx, p.WorkingDir, t.args(p, "config", "--hash", "*"))
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			m[f[0]] = f[1]
		}
	}
	return m, nil
}

// ReferencedFiles 找出配置引用的、在项目目录以外的文件和目录（env_file、configs、
// secrets、build 上下文）。老的 docker-compose 1.x 不支持，返回空。
func (t *Tool) ReferencedFiles(ctx context.Context, p Project) ([]string, error) {
	if t.Legacy() {
		return nil, nil
	}
	// 新版 Compose 默认把 env_file 的内容并进环境变量、不再列出文件路径，
	// 要加 --no-env-resolution 才保留路径；老一点的 v2 没有这个选项，就不加。
	out, err := run(ctx, p.WorkingDir, t.args(p, "config", "--format", "json", "--no-env-resolution"))
	if err != nil && strings.Contains(err.Error(), "no-env-resolution") {
		out, err = run(ctx, p.WorkingDir, t.args(p, "config", "--format", "json"))
	}
	if err != nil {
		return nil, err
	}
	return referencedOutside(out, p.WorkingDir)
}

func referencedOutside(configJSON, workingDir string) ([]string, error) {
	var cfg struct {
		Services map[string]struct {
			EnvFile json.RawMessage `json:"env_file"`
			Build   *struct {
				Context string `json:"context"`
			} `json:"build"`
		} `json:"services"`
		Configs map[string]struct {
			File string `json:"file"`
		} `json:"configs"`
		Secrets map[string]struct {
			File string `json:"file"`
		} `json:"secrets"`
	}
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return nil, fmt.Errorf("读不懂 docker compose config 的输出：%v", err)
	}
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" || strings.Contains(p, "://") {
			return // 远程的 build 上下文（git 地址等）不是本机文件
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(workingDir, p)
		}
		p = filepath.Clean(p)
		if p == workingDir || strings.HasPrefix(p, workingDir+string(filepath.Separator)) {
			return // 在项目目录里，会随目录一起迁移
		}
		seen[p] = true
	}
	for _, s := range cfg.Services {
		// env_file 可能是字符串列表，也可能是 {path, required} 列表。
		var plain []string
		var long []struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(s.EnvFile, &plain) == nil {
			for _, p := range plain {
				add(p)
			}
		} else if json.Unmarshal(s.EnvFile, &long) == nil {
			for _, p := range long {
				add(p.Path)
			}
		}
		if s.Build != nil {
			add(s.Build.Context)
		}
	}
	for _, c := range cfg.Configs {
		add(c.File)
	}
	for _, c := range cfg.Secrets {
		add(c.File)
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

func run(ctx context.Context, dir string, argv []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s：%s", strings.Join(argv[:min(len(argv), 2)], " "), msg)
	}
	return stdout.String(), nil
}
