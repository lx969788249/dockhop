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

// Service 是按现在的文件算出来的一个服务的配置（只取用来核对的部分）。
type Service struct {
	Image       string             `json:"image"`
	Environment map[string]*string `json:"environment"`
}

// Services 按现在的文件（含 .env、env_file 和当前的环境变量）算出每个服务最终的
// 镜像和环境变量，用来和正在运行的容器核对。老的 docker-compose 1.x 输出的不是
// JSON，返回空。
//
// 不用 config --hash：服务用了 env_file 时，它算出的摘要和启动时记在容器上的不一样，
// 即使文件完全没改过（第 0 阶段之后在 Compose v5 上实测）。
func (t *Tool) Services(ctx context.Context, p Project) (map[string]Service, error) {
	if t.Legacy() {
		return nil, nil
	}
	out, err := run(ctx, p.WorkingDir, t.args(p, "config", "--format", "json"))
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Services map[string]Service `json:"services"`
	}
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		return nil, fmt.Errorf("读不懂 docker compose config 的输出：%v", err)
	}
	return cfg.Services, nil
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
