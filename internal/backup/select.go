package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/spec"
	"github.com/lx969788249/dockhop/internal/ui"
)

// Compose 写在容器上的标签。
const (
	labelProject     = "com.docker.compose.project"
	labelWorkingDir  = "com.docker.compose.project.working_dir"
	labelConfigFiles = "com.docker.compose.project.config_files"
	labelService     = "com.docker.compose.service"
	labelOneoff      = "com.docker.compose.oneoff"
)

// item 是一个容器：解析后的常用字段，加上完整的原始配置。
type item struct {
	c   *docker.Container
	raw json.RawMessage
}

func (it *item) name() string { return it.c.ShortName() }

// unit 是菜单里的一项：一个独立容器，或者一整个 Compose 项目。
type unit struct {
	project    string // 为空表示独立容器
	workingDir string
	files      []string // Compose 配置文件（绝对路径）
	fileArgs   []string // 启动时 -f 的原样写法（老的 docker-compose 会把相对路径原样记在标签里）
	items      []*item
	panel      bool
	note       string
}

func (u *unit) label() string {
	if u.project == "" {
		return u.items[0].name()
	}
	return u.project
}

// tags 是菜单里写在名字后面 [ ] 里的补充说明：没在运行的写状态，项目写容器数。
func (u *unit) tags() []string {
	if u.project != "" {
		if len(u.items) > 1 {
			return []string{fmt.Sprintf("%d 个容器", len(u.items))}
		}
		return nil
	}
	var tags []string
	switch c := u.items[0].c; {
	case c.State.Paused:
		tags = append(tags, "暂停")
	case !c.State.Running:
		tags = append(tags, "已停止")
	}
	if u.panel {
		tags = append(tags, "面板自身，回车时不包含")
	}
	if u.note != "" {
		tags = append(tags, u.note)
	}
	return tags
}

type inventory struct {
	units     []*unit
	unitOf    map[*item]*unit
	byName    map[string]*item
	byID      map[string]*item
	notMoving []string // 不能迁移的容器及原因
}

// buildInventory 读出本机全部容器，按“独立容器 / Compose 项目”分组。
func buildInventory(ctx context.Context, dc *docker.Client, screen *ui.Screen) (*inventory, error) {
	list, err := dc.Containers(ctx, true)
	if err != nil {
		return nil, fmt.Errorf("读取容器列表失败：%v", err)
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("这台服务器上没有任何容器")
	}
	inv := &inventory{unitOf: map[*item]*unit{}, byName: map[string]*item{}, byID: map[string]*item{}}
	projects := map[string]*unit{}
	var order []*unit
	for i, s := range list {
		screen.Count("识别容器", i+1, len(list), s.Name())
		c, raw, err := dc.InspectContainer(ctx, s.ID)
		if err != nil {
			if docker.IsNotFound(err) {
				continue // 刚好被删掉了
			}
			return nil, err
		}
		it := &item{c: c, raw: raw}
		inv.byName[it.name()] = it
		inv.byID[c.ID] = it
		labels := c.Config.Labels
		if spec.IsSwarmTask(labels) {
			inv.notMoving = append(inv.notMoving, it.name()+"（Swarm 服务创建的容器，DockHop 不支持）")
			continue
		}
		if labels[labelOneoff] == "True" {
			inv.notMoving = append(inv.notMoving, it.name()+"（docker compose run 创建的临时容器）")
			continue
		}
		proj := labels[labelProject]
		if proj != "" {
			wdir, files, problem := composeLocation(labels)
			if problem == "" {
				key := proj + "|" + wdir
				u := projects[key]
				if u == nil {
					u = &unit{project: proj, workingDir: wdir, files: files, fileArgs: fileArgs(labels[labelConfigFiles], files)}
					projects[key] = u
					order = append(order, u)
				}
				u.items = append(u.items, it)
				inv.unitOf[it] = u
				continue
			}
			screen.Detail("容器 %s 属于 Compose 项目 %s，但%s，按独立容器迁移", it.name(), proj, problem)
			u := &unit{items: []*item{it}, note: "找不到 Compose 配置文件，按单独的容器迁移"}
			order = append(order, u)
			inv.unitOf[it] = u
			continue
		}
		u := &unit{items: []*item{it}, panel: spec.IsPanelContainer(c)}
		order = append(order, u)
		inv.unitOf[it] = u
	}
	// 独立容器在前，Compose 项目在后，各自按名字排序，菜单才稳定好找。
	sort.SliceStable(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if (a.project == "") != (b.project == "") {
			return a.project == ""
		}
		return a.label() < b.label()
	})
	for _, u := range order {
		sort.Slice(u.items, func(i, j int) bool { return u.items[i].name() < u.items[j].name() })
	}
	inv.units = order
	return inv, nil
}

// composeLocation 从标签里找出 Compose 项目的目录和配置文件。找不到时（例如
// Portainer 等面板创建的项目，配置文件在面板自己的容器里）返回原因。
func composeLocation(labels map[string]string) (string, []string, string) {
	wdir := labels[labelWorkingDir]
	var files []string
	for _, f := range splitConfigFiles(labels[labelConfigFiles]) {
		if !filepath.IsAbs(f) && wdir != "" {
			f = filepath.Join(wdir, f)
		}
		files = append(files, filepath.Clean(f))
	}
	if wdir == "" && len(files) > 0 {
		wdir = filepath.Dir(files[0]) // 老的 docker-compose 没有记录工作目录
	}
	if wdir == "" {
		return "", nil, "没有记录项目目录"
	}
	if st, err := os.Stat(wdir); err != nil || !st.IsDir() {
		return "", nil, "项目目录 " + wdir + " 不在这台服务器上"
	}
	var kept []string
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			// 迁入时为快照镜像临时加的覆盖文件，用完就删了，不算缺失。
			if strings.HasPrefix(filepath.Base(f), "dockhop-override-") {
				continue
			}
			return "", nil, "配置文件 " + f + " 不在这台服务器上"
		}
		kept = append(kept, f)
	}
	return wdir, kept, ""
}

// fileArgs 是启动时 -f 的原样写法：去掉迁入时临时加的覆盖文件，其余和标签里一样。
func fileArgs(label string, kept []string) []string {
	keep := map[string]bool{}
	for _, f := range kept {
		keep[filepath.Base(f)] = true
	}
	var out []string
	for _, f := range splitConfigFiles(label) {
		if keep[filepath.Base(f)] {
			out = append(out, f)
		}
	}
	return out
}

// splitConfigFiles 拆分配置文件列表：Compose 用逗号连接多个 -f 文件；没有逗号时
// 兼容按冒号分隔的写法（COMPOSE_FILE）。
func splitConfigFiles(raw string) []string {
	if raw == "" {
		return nil
	}
	sep := ","
	if !strings.Contains(raw, ",") {
		sep = ":"
	}
	var out []string
	for _, f := range strings.Split(raw, sep) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// selection 是最终要迁移的内容。
type selection struct {
	units []*unit
	items []*item
}

func (s *selection) projectCount() int {
	n := 0
	for _, u := range s.units {
		if u.project != "" {
			n++
		}
	}
	return n
}

// chooseUnits 决定迁移哪些：给了 --include 就按名字找，否则显示菜单让用户选。
func chooseUnits(inv *inventory, include []string, screen *ui.Screen, prompt *ui.Prompter) ([]*unit, error) {
	if len(include) > 0 {
		var chosen []*unit
		seen := map[*unit]bool{}
		for _, name := range include {
			it := inv.byName[name]
			if it == nil {
				return nil, fmt.Errorf("没有找到容器 %s（执行 docker ps -a 查看容器名）", name)
			}
			u := inv.unitOf[it]
			if u == nil {
				return nil, fmt.Errorf("容器 %s 不能迁移", name)
			}
			if u.project != "" && len(u.items) > 1 && !seen[u] {
				screen.Info("%s 属于 Compose 项目 %s，项目里的 %d 个容器一起迁移", name, u.project, len(u.items))
			}
			if !seen[u] {
				seen[u] = true
				chosen = append(chosen, u)
			}
		}
		return chosen, nil
	}
	if !prompt.Interactive() {
		return nil, fmt.Errorf("没有可以输入的终端，无法显示菜单。请用 --include 容器1,容器2 指定要迁移的容器")
	}
	screen.Hold()
	// 菜单只写名字和 [ ] 里的补充说明；项目目录不显示，一长串路径只会让名字更难找。
	var standalone, projects []string
	panels := false
	width := len(strconv.Itoa(len(inv.units)))
	for i, u := range inv.units {
		line := fmt.Sprintf("    %*d) %s", width, i+1, u.label())
		for _, t := range u.tags() {
			line += "  [" + t + "]"
		}
		if u.project == "" {
			standalone = append(standalone, line)
		} else {
			projects = append(projects, line)
		}
		panels = panels || u.panel
	}
	if len(standalone) > 0 {
		screen.Println("  单独的容器：")
		screen.Println(strings.Join(standalone, "\n"))
	}
	if len(projects) > 0 {
		screen.Println("  Compose 项目（项目里的容器一起迁移）：")
		screen.Println(strings.Join(projects, "\n"))
	}
	if len(inv.notMoving) > 0 {
		screen.Println("  不能迁移：")
		for _, s := range inv.notMoving {
			screen.Println("    · " + s)
		}
	}
	question := "\n请输入要迁移的序号，多个用逗号隔开（例如 1,3），直接回车迁移全部"
	if panels {
		question += "（不含面板自身）"
	}
	question += "："
	var chosen []*unit
	_, err := prompt.Ask(question, func(s string) error {
		chosen = nil
		if strings.TrimSpace(s) == "" {
			for _, u := range inv.units {
				if !u.panel {
					chosen = append(chosen, u)
				}
			}
			if len(chosen) == 0 {
				return fmt.Errorf("除了面板自身没有别的容器了，请输入序号选择")
			}
			return nil
		}
		seen := map[int]bool{}
		// 逗号、中文逗号、顿号、空格都可以用来分开。
		separator := func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(",，、", r) }
		for _, part := range strings.FieldsFunc(s, separator) {
			n, err := strconv.Atoi(part)
			if err != nil || n < 1 || n > len(inv.units) {
				return fmt.Errorf("序号 %s 不对，请输入 1 到 %d 之间的数字", part, len(inv.units))
			}
			if !seen[n] {
				seen[n] = true
				chosen = append(chosen, inv.units[n-1])
			}
		}
		if len(chosen) == 0 {
			return fmt.Errorf("请至少选择一项")
		}
		return nil
	})
	return chosen, err
}

// withDependencies 把选中容器依赖的容器（借用网络、进程、IPC，--link、
// --volumes-from）一起纳入；被依赖的是 Compose 容器时纳入整个项目。
func withDependencies(inv *inventory, chosen []*unit, screen *ui.Screen) (*selection, error) {
	in := map[*unit]bool{}
	for _, u := range chosen {
		in[u] = true
	}
	queue := append([]*unit(nil), chosen...)
	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]
		for _, it := range u.items {
			for _, ref := range spec.Dependencies(it.c) {
				dep := inv.lookup(ref)
				if dep == nil {
					return nil, fmt.Errorf("容器 %s 依赖的容器 %s 不存在，无法迁移", it.name(), shortRef(ref))
				}
				du := inv.unitOf[dep]
				if du == nil {
					return nil, fmt.Errorf("容器 %s 依赖的容器 %s 不能迁移", it.name(), dep.name())
				}
				if !in[du] {
					in[du] = true
					queue = append(queue, du)
					screen.Info("自动加入 %s：%s 依赖它，必须一起迁移", du.label(), it.name())
				}
			}
		}
	}
	sel := &selection{}
	for _, u := range inv.units { // 按菜单顺序
		if in[u] {
			sel.units = append(sel.units, u)
			sel.items = append(sel.items, u.items...)
		}
	}
	return sel, nil
}

// lookup 按名字或 ID（完整或前 12 位以上）找容器。
func (inv *inventory) lookup(ref string) *item {
	if it := inv.byName[strings.TrimPrefix(ref, "/")]; it != nil {
		return it
	}
	if it := inv.byID[ref]; it != nil {
		return it
	}
	if len(ref) >= 12 {
		for id, it := range inv.byID {
			if strings.HasPrefix(id, ref) {
				return it
			}
		}
	}
	return nil
}

func shortRef(ref string) string {
	if len(ref) == 64 {
		return ref[:12]
	}
	return ref
}
