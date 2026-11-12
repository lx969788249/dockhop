package backup

import (
	"bytes"
	"strings"
	"testing"

	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/ui"
)

// menuInventory 是菜单测试用的服务：单独的容器 web（在运行）、cache（已停止），Compose 项目 site（两个容器），
// 面板自身 panel；不能迁移的 dockhop-helper。
func menuInventory() *inventory {
	mk := func(name string, running bool) *item {
		c := &docker.Container{Name: "/" + name}
		c.State.Running = running
		return &item{c: c}
	}
	web, cache, app, db, panel := mk("web", true), mk("cache", false), mk("site-app-1", true), mk("site-db-1", true), mk("panel", true)
	inv := &inventory{
		units: []*unit{{items: []*item{web}}, {items: []*item{cache}}, {project: "site", items: []*item{app, db}},
			{items: []*item{panel}, panel: true}},
		unitOf: map[*item]*unit{}, byName: map[string]*item{}, notMoving: []string{"dockhop-helper（DockHop 自己的临时容器）"},
	}
	for _, u := range inv.units {
		for _, it := range u.items {
			inv.unitOf[it] = u
			inv.byName[it.name()] = it
		}
	}
	return inv
}

func labelsOf(units []*unit) string {
	var out []string
	for _, u := range units {
		out = append(out, u.label())
	}
	return strings.Join(out, ",")
}

// --include 按容器名选：Compose 项目里的容器整个项目一起迁移（说一声，只说一次）；找不到的、不能迁移的停下。
func TestChooseUnitsByName(t *testing.T) {
	inv := menuInventory()
	var out bytes.Buffer
	chosen, err := chooseUnits(inv, []string{"site-db-1", "web", "site-app-1"}, ui.New(&out, false, nil), ui.NewPrompter(nil, &out))
	if err != nil || labelsOf(chosen) != "site,web" || strings.Count(out.String(), "一起迁移") != 1 {
		t.Errorf("选到 %s（%v）\n%s", labelsOf(chosen), err, out.String())
	}
	inv.byName["orphan"] = &item{c: &docker.Container{Name: "/orphan"}}
	for _, name := range []string{"nope", "orphan"} {
		if _, err := chooseUnits(inv, []string{name}, ui.New(&out, false, nil), nil); err == nil {
			t.Errorf("%s 应停下", name)
		}
	}
}

// 菜单：单独的容器和 Compose 项目分开列，后面 [ ] 里写补充说明，不能迁移的单独列出；序号可以用逗号、中文逗号、
// 顿号、空格分开，重复的只算一次，写错了说明后重新问；直接回车是除了面板自身的全部。
func TestChooseUnitsFromMenu(t *testing.T) {
	for _, c := range []struct{ input, want string }{
		{"\n", "web,cache,site"},
		{"3，1、3 2\n", "site,web,cache"},
		{"0\nx\n,\n4\n", "panel"},
	} {
		var out bytes.Buffer
		chosen, err := chooseUnits(menuInventory(), nil, ui.New(&out, false, nil), ui.NewPrompter(strings.NewReader(c.input), &out))
		if err != nil || labelsOf(chosen) != c.want {
			t.Errorf("输入 %q：选到 %s（%v），应为 %s\n%s", c.input, labelsOf(chosen), err, c.want, out.String())
		}
	}
	var out bytes.Buffer
	chooseUnits(menuInventory(), nil, ui.New(&out, false, nil), ui.NewPrompter(strings.NewReader("0\nx\n,\n4\n"), &out))
	want := "  单独的容器：\n    1. web\n    2. cache  [已停止]\n    4. panel  [面板自身，回车时不包含]\n" +
		"  Compose 项目（项目里的容器一起迁移）：\n    3. site  [2 个容器]\n" +
		"  不能迁移：\n    · dockhop-helper（DockHop 自己的临时容器）\n" +
		"\n请输入要迁移的序号，多个用逗号隔开（例如 1,3），直接回车迁移全部（不含面板自身）：序号 0 不对，请输入 1 到 4 之间的数字\n"
	if !strings.HasPrefix(out.String(), want) || strings.Count(out.String(), "请输入要迁移的序号") != 4 ||
		!strings.Contains(out.String(), "序号 x 不对") || !strings.Contains(out.String(), "请至少选择一项") {
		t.Errorf("菜单：\n%s", out.String())
	}
	inv := menuInventory()
	inv.units = inv.units[3:] // 只剩面板自身
	if chosen, err := chooseUnits(inv, nil, ui.New(&out, false, nil), ui.NewPrompter(strings.NewReader("\n1\n"), &out)); err != nil ||
		labelsOf(chosen) != "panel" || !strings.Contains(out.String(), "除了面板自身没有别的容器了") {
		t.Errorf("只有面板自身时直接回车应重新问：%s（%v）", labelsOf(chosen), err)
	}
	if _, err := chooseUnits(menuInventory(), nil, ui.New(&out, false, nil), ui.NewPrompter(nil, &out)); err == nil {
		t.Error("不能提问时应停下")
	}
}
