package preflight

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/ui"
)

// Problem 是检查没能通过的原因，调用方把它显示成失败原因和建议。
type Problem struct {
	Reason string
	Hint   string
}

func (p *Problem) Error() string { return p.Reason }

// Runner 反复检查、让用户处理问题，直到没有需要处理的问题。
type Runner struct {
	Screen  *ui.Screen
	Prompt  *ui.Prompter
	Auto    bool // --yes：自动采用推荐做法
	Collect func() (*Target, error)
	shown   map[string]bool
}

// errRecheck 表示用户处理好了、要重新检查。
var errRecheck = errors.New("重新检查")

// Run 执行检查。返回 nil 表示可以继续，Decisions 里是用户的选择。
func (r *Runner) Run(plan *bundle.Plan, d *Decisions, o Options) (*Result, error) {
	if r.shown == nil {
		r.shown = map[string]bool{}
	}
	auto := r.Auto
	for round := 1; ; round++ {
		if d.Workdir != "" {
			o.BaseDir = d.Workdir
		}
		t, err := r.Collect()
		if err != nil {
			return nil, &Problem{Reason: "读取新服务器的情况失败：" + err.Error()}
		}
		res := Evaluate(plan, t, d, o)
		var asks, stops []Issue
		for _, i := range res.Issues {
			switch i.Level {
			case Info:
				if !r.shown[i.Title] {
					r.shown[i.Title] = true
					if i.Kind == "docker_version" {
						r.Screen.Info("%s", i.Title) // 一般不影响迁移，不算要留意的事
					} else {
						r.Screen.Warn("%s", i.Title)
					}
				}
			case Ask:
				asks = append(asks, i)
			case Stop:
				stops = append(stops, i)
			}
		}
		if len(stops) > 0 {
			r.Screen.Hold()
			// 只有一个问题时，原因和建议都写在最后的结果里，这里不重复。
			if len(stops) > 1 {
				r.Screen.Println(fmt.Sprintf("发现 %d 个无法继续的问题，还没有下载或改动任何东西：", len(stops)))
				for _, s := range stops {
					r.Screen.Println("  ✗ " + s.Title)
					if s.Hint != "" {
						r.Screen.Println("    建议：" + s.Hint)
					}
				}
			}
			return res, &Problem{Reason: stops[0].Title, Hint: stops[0].Hint}
		}
		if len(asks) == 0 {
			return res, nil
		}
		if !r.Prompt.Interactive() && !auto {
			r.Screen.Hold()
			r.Screen.Println(fmt.Sprintf("发现 %d 个需要处理的问题，还没有下载或改动任何东西：", len(asks)))
			for _, a := range asks {
				r.Screen.Println("  ! " + a.Title)
				if a.Hint != "" {
					r.Screen.Println("    建议：" + a.Hint)
				}
			}
			return res, &Problem{Reason: asks[0].Title,
				Hint: "处理后重新运行；或者加上 --yes，自动采用推荐的做法（例如自动换一个空闲端口）"}
		}
		if !auto {
			r.Screen.Hold()
			r.Screen.Println(fmt.Sprintf("发现 %d 个需要处理的问题（还没有下载或改动任何东西）：", len(asks)))
		}
		for i, a := range asks {
			err := r.resolve(a, d, i+1, len(asks), auto)
			if errors.Is(err, errRecheck) {
				break
			}
			if err != nil {
				hint := "处理后用同一个链接重新迁入即可继续"
				if auto {
					hint = "这个问题没法自动处理，请处理后重新运行"
				}
				var p *Problem
				if errors.As(err, &p) {
					return res, p
				}
				if errors.Is(err, errCancel) {
					return res, &Problem{Reason: "已取消：" + a.Title, Hint: hint}
				}
				return res, err
			}
		}
		// 自动处理只做一轮：再查一次还有问题就交给用户。
		if auto && round >= 2 {
			if !r.Prompt.Interactive() {
				return res, &Problem{Reason: "自动处理之后仍有问题没有解决：" + asks[0].Title, Hint: "查看上面的问题，处理后重新运行"}
			}
			auto = false
		}
		if !auto {
			r.Screen.Println("\n重新检查……")
		}
	}
}

var errCancel = errors.New("取消")

// resolve 处理一个问题。返回 nil 表示已经记下选择，errRecheck 表示要重新检查，
// errCancel 表示取消迁入。auto 时自动采用推荐做法，没法自动处理时返回 errCancel。
func (r *Runner) resolve(i Issue, d *Decisions, n, total int, auto bool) error {
	choose := func(options ...string) (int, error) {
		if auto {
			r.Screen.Warn("%s → 按 --yes 自动选择：%s", i.Title, strings.TrimSuffix(options[0], "（推荐）"))
			return 1, nil
		}
		r.Screen.Println(fmt.Sprintf("\n问题 %d/%d：%s", n, total, i.Title))
		return r.Prompt.Choose(options, 1)
	}
	skipProject := func(reason string) {
		d.SkipProjects = add(d.SkipProjects, i.Project)
		d.Skipped = append(d.Skipped, Skip{Project: i.Project, Reason: reason})
	}
	switch i.Kind {
	case "port", "host_ip":
		if i.Project != "" {
			// Compose 项目的端口写在用户自己的文件里，不替用户改，只能处理后重查或跳过。
			if auto {
				return errCancel
			}
			r.Screen.Println(fmt.Sprintf("\n问题 %d/%d：%s", n, total, i.Title))
			r.Screen.Println("  Compose 项目的端口写在它的配置文件里。请停掉占用端口的程序，或在新服务器上修改 " + i.Project + " 的配置文件中的端口。")
			c, err := r.Prompt.Choose([]string{"我已处理好，重新检查（推荐）", "暂时跳过 " + i.Project + " 项目，继续迁移其他容器", "取消迁入"}, 1)
			if err != nil {
				return err
			}
			switch c {
			case 1:
				return errRecheck
			case 2:
				skipProject("端口 " + i.Port.HostPort + " 有冲突")
				return nil
			}
			return errCancel
		}
		if i.Kind == "host_ip" {
			c, err := choose("改为绑定到所有地址（推荐）", "不对外开放这个端口", "取消迁入")
			if err != nil {
				return err
			}
			switch c {
			case 1:
				d.setPort(PortChange{PortRef: *i.Port, NewIP: "", NewPort: i.Port.HostPort})
				return nil
			case 2:
				d.setPort(PortChange{PortRef: *i.Port, Drop: true})
				return nil
			}
			return errCancel
		}
		if auto {
			if i.Suggest == "" {
				return errCancel
			}
			r.Screen.Warn("%s → 按 --yes 自动改用空闲端口 %s", i.Title, i.Suggest)
			d.setPort(PortChange{PortRef: *i.Port, NewIP: i.Port.HostIP, NewPort: i.Suggest})
			return nil
		}
		first := "改用空闲端口 " + i.Suggest + "（推荐）"
		if i.Suggest == "" {
			first = "改用其他端口（附近没有找到空闲端口，需要手动输入）"
		}
		c, err := choose(first, "输入其他端口", "我先停掉占用端口的程序，然后重新检查", "不对外开放这个端口", "取消迁入")
		if err != nil {
			return err
		}
		switch c {
		case 1, 2:
			port := i.Suggest
			if c == 2 || port == "" {
				port, err = r.Prompt.Ask("新端口：", func(s string) error {
					if n, err := strconv.Atoi(s); err != nil || n < 1 || n > 65535 {
						return errors.New("请输入 1 到 65535 之间的端口号")
					}
					return nil
				})
				if err != nil {
					return err
				}
			}
			d.setPort(PortChange{PortRef: *i.Port, NewIP: i.Port.HostIP, NewPort: port})
		case 3:
			return errRecheck
		case 4:
			d.setPort(PortChange{PortRef: *i.Port, Drop: true})
		default:
			return errCancel
		}
		return nil

	case "existing_container", "existing_project":
		what, keep := "容器", "保留已有的，跳过这个容器"
		if i.Kind == "existing_project" {
			what, keep = "项目", "保留已有的，跳过这个项目"
		}
		c, err := choose("替换它（推荐，迁入失败时会放回原来的）", keep, "取消迁入")
		if err != nil {
			return err
		}
		switch {
		case c == 1 && what == "容器":
			d.ReplaceContainers = add(d.ReplaceContainers, i.Container)
		case c == 1:
			d.ReplaceProjects = add(d.ReplaceProjects, i.Project)
		case c == 2 && what == "容器":
			d.SkipContainers = add(d.SkipContainers, i.Container)
			d.Skipped = append(d.Skipped, Skip{Container: i.Container, Reason: "保留新服务器上已有的容器"})
		case c == 2:
			skipProject("保留新服务器上已有的项目")
		default:
			return errCancel
		}
		return nil

	case "ip_conflict", "subnet_conflict", "network_mismatch":
		if i.Kind == "ip_conflict" && i.Project != "" {
			if auto {
				return errCancel
			}
			r.Screen.Println(fmt.Sprintf("\n问题 %d/%d：%s", n, total, i.Title))
			c, err := r.Prompt.Choose([]string{"我已处理好，重新检查（推荐）", "暂时跳过 " + i.Project + " 项目，继续迁移其他容器", "取消迁入"}, 1)
			if err != nil {
				return err
			}
			switch c {
			case 1:
				return errRecheck
			case 2:
				skipProject("固定 IP 有冲突")
				return nil
			}
			return errCancel
		}
		first := map[string]string{
			"ip_conflict":      "改为由 Docker 自动分配 IP（推荐）",
			"subnet_conflict":  "让 Docker 自动选择一个不冲突的网段，网络里的容器改为自动分配 IP（推荐）",
			"network_mismatch": "继续使用新服务器上已有的这个网络，容器改为自动分配 IP（推荐）",
		}[i.Kind]
		c, err := choose(first, "取消迁入")
		if err != nil {
			return err
		}
		if c != 1 {
			return errCancel
		}
		switch i.Kind {
		case "ip_conflict":
			d.AutoIP = append(d.AutoIP, NetRef{Container: i.Container, Network: i.Network})
		case "subnet_conflict":
			d.AutoSubnet = add(d.AutoSubnet, i.Network)
		default:
			d.ReuseNetwork = add(d.ReuseNetwork, i.Network)
		}
		return nil

	case "volume_mismatch":
		if auto {
			return errCancel
		}
		r.Screen.Println(fmt.Sprintf("\n问题 %d/%d：%s", n, total, i.Title))
		c, err := r.Prompt.Choose([]string{"取消迁入，我先处理这个数据卷（推荐）", "保留已有的数据卷，不写入迁移过来的数据"}, 1)
		if err != nil {
			return err
		}
		if c != 2 {
			return errCancel
		}
		d.KeepVolumes = add(d.KeepVolumes, i.Volume)
		return nil

	case "digest":
		c, err := choose("改用迁移包里的镜像（推荐：内容和旧服务器完全相同，只是配置里的镜像名去掉 @sha256 部分）", "取消迁入")
		if err != nil {
			return err
		}
		if c != 1 {
			return errCancel
		}
		d.BundleImages = add(d.BundleImages, i.Image)
		return nil

	case "disk_base":
		if auto {
			return errCancel
		}
		r.Screen.Println(fmt.Sprintf("\n问题 %d/%d：%s", n, total, i.Title))
		c, err := r.Prompt.Choose([]string{"我先清理磁盘，然后重新检查（推荐）", "换一个目录存放下载文件", "取消迁入"}, 1)
		if err != nil {
			return err
		}
		switch c {
		case 1:
			return errRecheck
		case 2:
			dir, err := r.Prompt.Ask("目录（例如 /data/dockhop）：", func(s string) error {
				if !filepath.IsAbs(strings.TrimSpace(s)) {
					return errors.New("请输入以 / 开头的完整路径")
				}
				return nil
			})
			if err != nil {
				return err
			}
			d.Workdir = filepath.Clean(strings.TrimSpace(dir))
			return errRecheck
		}
		return errCancel

	default: // disk_docker、disk_bind 等：只能清理后重查
		if auto {
			return errCancel
		}
		r.Screen.Println(fmt.Sprintf("\n问题 %d/%d：%s", n, total, i.Title))
		c, err := r.Prompt.Choose([]string{"我先处理，然后重新检查（推荐）", "取消迁入"}, 1)
		if err != nil {
			return err
		}
		if c == 1 {
			return errRecheck
		}
		return errCancel
	}
}

func (d *Decisions) setPort(p PortChange) {
	for i := range d.Ports {
		if d.Ports[i].PortRef == p.PortRef {
			d.Ports[i] = p
			return
		}
	}
	d.Ports = append(d.Ports, p)
}

// Summary 列出检查时做的调整，显示在结果里，让用户知道哪些地方和旧服务器不同。
func (d *Decisions) Summary() []string {
	var out []string
	for _, p := range d.Ports {
		switch {
		case p.Drop:
			out = append(out, fmt.Sprintf("不再对外开放：%s 的端口 %s", p.Container, p.HostPort))
		case p.NewPort != p.HostPort:
			out = append(out, fmt.Sprintf("端口变更：%s %s → %s", p.Container, p.HostPort, p.NewPort))
		case p.NewIP != p.HostIP:
			out = append(out, fmt.Sprintf("改绑到所有地址：%s 的端口 %s（原来绑定 %s）", p.Container, p.HostPort, p.HostIP))
		}
	}
	for _, s := range d.Skipped {
		if s.Project != "" {
			out = append(out, fmt.Sprintf("⚠️ 没有迁入：Compose 项目 %s（%s）", s.Project, s.Reason))
		} else {
			out = append(out, fmt.Sprintf("⚠️ 没有迁入：容器 %s（%s）", s.Container, s.Reason))
		}
	}
	for _, a := range d.AutoIP {
		out = append(out, fmt.Sprintf("自动分配 IP：%s 在网络 %s 中", a.Container, a.Network))
	}
	for _, n := range d.AutoSubnet {
		out = append(out, "自动选择网段：网络 "+n)
	}
	for _, n := range d.ReuseNetwork {
		out = append(out, "沿用新服务器已有的网络："+n)
	}
	for _, v := range d.KeepVolumes {
		out = append(out, "保留已有数据卷、没有写入迁移的数据："+v)
	}
	for _, img := range d.BundleImages {
		out = append(out, "改用迁移包里的镜像："+shortRef(img)+" → "+StripDigest(img))
	}
	return out
}

// StripDigest 去掉镜像名里的 @sha256 部分；没有标签时用摘要前 12 位做标签。
func StripDigest(ref string) string {
	name, digest, ok := strings.Cut(ref, "@")
	if !ok {
		return ref
	}
	last := name[strings.LastIndex(name, "/")+1:]
	if strings.Contains(last, ":") {
		return name
	}
	digest = strings.TrimPrefix(digest, "sha256:")
	if len(digest) > 12 {
		digest = digest[:12]
	}
	return name + ":pinned-" + digest
}
