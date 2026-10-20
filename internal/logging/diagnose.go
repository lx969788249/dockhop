package logging

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/host"
	"github.com/lx969788249/dockhop/internal/ui"
)

// DiagnoseInput 是生成诊断文件需要的信息。
type DiagnoseInput struct {
	Version   string
	Platform  string
	Home      string // DOCKHOP_HOME，最近的日志在 Home/logs 下
	Docker    *docker.Client
	DockerErr error // 连不上 Docker 时的原因
	Now       time.Time
}

// WriteDiagnose 收集排查问题需要的信息写到 w。只读取信息，不改动任何容器或数据；
// 所有内容写入前都会隐藏密钥和密码。个别项目读不到不影响其他项目。
func WriteDiagnose(ctx context.Context, w io.Writer, in DiagnoseInput, r *Redactor) error {
	if r == nil {
		r = &Redactor{}
	}
	bw := bufio.NewWriter(w)
	section := func(title string) { fmt.Fprintf(bw, "\n===== %s =====\n", title) }
	line := func(format string, args ...any) { fmt.Fprintln(bw, r.Redact(fmt.Sprintf(format, args...))) }

	fmt.Fprintf(bw, "DockHop 诊断信息 · %s\n", in.Now.Format("2006-01-02 15:04:05 -0700"))
	line("DockHop：%s（%s）", in.Version, in.Platform)
	line("系统：%s", host.OSName())
	line("内核：%s", host.Kernel())
	line("用户：uid=%d", os.Geteuid())

	section("Docker")
	if in.Docker == nil {
		line("连不上 Docker：%v", in.DockerErr)
	} else {
		v := in.Docker.Server()
		line("版本：%s · 接口 %s（使用 %s）· 本机：%v", v.Version, v.APIVersion, in.Docker.APIVersion(), in.Docker.Local())
		if info, err := in.Docker.Info(ctx); err != nil {
			line("读取 Docker 概况失败：%v", err)
		} else {
			runtimes := make([]string, 0, len(info.Runtimes))
			for k := range info.Runtimes {
				runtimes = append(runtimes, k)
			}
			sort.Strings(runtimes)
			line("系统：%s · 架构 %s · %d 核 · 内存 %s", info.OperatingSystem, info.Architecture, info.NCPU, ui.HumanBytes(info.MemTotal))
			line("存储：%s · %s · cgroup v%s · 默认日志 %s", info.Driver, info.DockerRootDir, info.CgroupVersion, info.LoggingDriver)
			line("运行环境：%s（默认 %s）· Swarm：%s", strings.Join(runtimes, "、"), info.DefaultRuntime, orDash(info.Swarm.LocalNodeState))
			line("容器 %d 个 · 镜像 %d 个", info.Containers, info.Images)
			if d, err := host.DiskOf(info.DockerRootDir); err == nil {
				line("Docker 目录所在磁盘：剩余 %s / 共 %s", ui.HumanBytes(int64(d.Free)), ui.HumanBytes(int64(d.Total)))
			}
		}

		section("容器")
		if list, err := in.Docker.Containers(ctx, true); err != nil {
			line("读取失败：%v", err)
		} else {
			for _, c := range list {
				var ports []string
				for _, p := range c.Ports {
					if p.PublicPort > 0 {
						ports = append(ports, fmt.Sprintf("%s:%d->%d/%s", p.IP, p.PublicPort, p.PrivatePort, p.Type))
					}
				}
				project := c.Labels["com.docker.compose.project"]
				line("%s · %s · %s · %s%s%s", c.Name(), c.Image, c.State, c.Status,
					prefixIf(" · 端口 ", strings.Join(ports, " ")), prefixIf(" · Compose 项目 ", project))
			}
		}

		section("网络")
		if nets, err := in.Docker.Networks(ctx); err != nil {
			line("读取失败：%v", err)
		} else {
			for _, n := range nets {
				var subnets []string
				for _, c := range n.IPAM.Config {
					subnets = append(subnets, c.Subnet)
				}
				line("%s · %s · %s", n.Name, n.Driver, orDash(strings.Join(subnets, " ")))
			}
		}

		section("数据卷")
		if vols, err := in.Docker.Volumes(ctx); err != nil {
			line("读取失败：%v", err)
		} else {
			for _, v := range vols {
				line("%s · %s%s", v.Name, v.Driver, prefixIf(" · Compose 项目 ", v.Labels["com.docker.compose.project"]))
			}
		}
	}

	section("磁盘")
	for _, p := range []string{"/", in.Home} {
		if d, err := host.DiskOf(p); err == nil {
			line("%s（统计 %s）：剩余 %s / 共 %s", p, d.Path, ui.HumanBytes(int64(d.Free)), ui.HumanBytes(int64(d.Total)))
		}
	}

	section("监听端口")
	if ls, err := host.Listeners(); err != nil {
		line("读取失败：%v", err)
	} else {
		for _, l := range ls {
			line("%s %s:%d", l.Proto, l.Addr, l.Port)
		}
	}

	section("防火墙")
	found := false
	for _, c := range [][]string{{"ufw", "status"}, {"firewall-cmd", "--state"}, {"firewall-cmd", "--list-ports"}} {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		found = true
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		out, _ := exec.CommandContext(cctx, c[0], c[1:]...).CombinedOutput()
		cancel()
		line("$ %s\n%s", strings.Join(c, " "), strings.TrimSpace(string(out)))
	}
	if !found {
		line("没有找到 ufw 或 firewalld（云厂商控制台里的安全组需要另外检查）")
	}

	section("最近的运行日志（最多 3 份，每份最后 300 行）")
	logs, _ := filepath.Glob(filepath.Join(in.Home, "logs", "*.log"))
	sort.Slice(logs, func(i, j int) bool { return modTime(logs[i]).After(modTime(logs[j])) })
	if len(logs) == 0 {
		line("没有日志")
	}
	for i, p := range logs {
		if i == 3 {
			break
		}
		line("--- %s", p)
		for _, l := range tailFile(p, 300) {
			line("%s", l)
		}
	}
	return bw.Flush()
}

func modTime(p string) time.Time {
	st, err := os.Stat(p)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

func tailFile(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	return lines
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func prefixIf(prefix, s string) string {
	if s == "" {
		return ""
	}
	return prefix + s
}
