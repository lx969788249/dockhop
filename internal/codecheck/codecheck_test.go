// Package codecheck 检查能用程序检查的结构规矩，go test ./... 会跑。规矩是什么、为什么这样定，见仓库根目录的
// CLAUDE.md。违反了先看规矩背后的原因；确实需要例外的，改这里的表，并写明为什么。
package codecheck

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// pkg 是一个包的源文件：正式代码和测试分开。
type pkg struct {
	path         string // 相对于 internal 的路径，例如 restore、docker/dockertest；程序入口是 cmd/dockhop
	files, tests []*ast.File
}

var fset = token.NewFileSet()

// load 解析 internal 和 cmd 下的全部 Go 文件。测试文件解析时认出局部变量（见 TestTestsKeepPackageVars）。
func load(t *testing.T) []*pkg {
	t.Helper()
	byDir := map[string]*pkg{}
	for _, root := range []string{"..", "../../cmd"} {
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.Contains(p, "testdata") {
				return err
			}
			mode := parser.SkipObjectResolution
			if strings.HasSuffix(p, "_test.go") {
				mode = 0
			}
			f, err := parser.ParseFile(fset, p, nil, mode)
			if err != nil {
				return err
			}
			dir, _ := filepath.Rel("..", filepath.Dir(p))
			dir = filepath.ToSlash(strings.TrimPrefix(dir, "../"))
			k := byDir[dir]
			if k == nil {
				k = &pkg{path: dir}
				byDir[dir] = k
			}
			if strings.HasSuffix(p, "_test.go") {
				k.tests = append(k.tests, f)
			} else {
				k.files = append(k.files, f)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	var out []*pkg
	for _, k := range byDir {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// funcName 是函数在报告和例外表里的名字，例如“restore migrate.units”“backup buildPlan”。
func funcName(k *pkg, fd *ast.FuncDecl) string {
	name := fd.Name.Name
	if fd.Recv != nil && len(fd.Recv.List) > 0 {
		t := fd.Recv.List[0].Type
		if s, ok := t.(*ast.StarExpr); ok {
			t = s.X
		}
		if g, ok := t.(*ast.IndexExpr); ok {
			t = g.X
		}
		if id, ok := t.(*ast.Ident); ok {
			name = id.Name + "." + name
		}
	}
	return k.path + " " + name
}

// ---------- 函数有多难读 ----------

// measure 量一个函数有多难读：
//   - cc 是认知复杂度（SonarSource 的算法）：if、else if、else、for、switch、select 各 +1，嵌在别的
//     判断、循环、函数字面量里的 if、for、switch、select 再加上嵌套的层数；一串相同的 && 或 || 算 +1；
//     带标签的 break、continue、goto 算 +1。按 Go 的写法有一处不同：出错就返回的判断
//     （if err != nil { return … }，没有 else）不算，它是平铺的，读的时候不用记着它；
//   - depth 是最深的嵌套：if、for、switch、select 的里面和函数字面量各算一层。
type measure struct{ cc, depth int }

func measureFunc(body *ast.BlockStmt) measure {
	var m measure
	m.block(body.List, 0)
	return m
}

func (m *measure) block(list []ast.Stmt, nest int) {
	for _, s := range list {
		m.stmt(s, nest)
	}
}

// inside 进入一层嵌套：里面的语句在 nest 这一层。
func (m *measure) inside(list []ast.Stmt, nest int) {
	m.depth = max(m.depth, nest)
	m.block(list, nest)
}

func (m *measure) stmt(s ast.Stmt, nest int) {
	switch s := s.(type) {
	case *ast.IfStmt:
		if errGuard(s) {
			m.expr(s.Init, nest)
			return
		}
		m.cc += 1 + nest
		m.ifChain(s, nest)
	case *ast.ForStmt:
		m.cc += 1 + nest
		m.expr(s.Init, nest)
		m.expr(s.Cond, nest)
		m.expr(s.Post, nest)
		m.inside(s.Body.List, nest+1)
	case *ast.RangeStmt:
		m.cc += 1 + nest
		m.expr(s.X, nest)
		m.inside(s.Body.List, nest+1)
	case *ast.SwitchStmt:
		m.cc += 1 + nest
		m.expr(s.Init, nest)
		m.expr(s.Tag, nest)
		m.clauses(s.Body, nest)
	case *ast.TypeSwitchStmt:
		m.cc += 1 + nest
		m.expr(s.Init, nest)
		m.expr(s.Assign, nest)
		m.clauses(s.Body, nest)
	case *ast.SelectStmt:
		m.cc += 1 + nest
		m.clauses(s.Body, nest)
	case *ast.BlockStmt:
		m.block(s.List, nest)
	case *ast.LabeledStmt:
		m.stmt(s.Stmt, nest)
	case *ast.BranchStmt:
		if s.Label != nil {
			m.cc++
		}
	default:
		m.expr(s, nest)
	}
}

// ifChain 量 if 的条件和里面，以及后面的 else if、else（各 +1，不加嵌套）。
func (m *measure) ifChain(s *ast.IfStmt, nest int) {
	m.expr(s.Init, nest)
	m.expr(s.Cond, nest)
	m.inside(s.Body.List, nest+1)
	switch e := s.Else.(type) {
	case *ast.IfStmt:
		m.cc++
		m.ifChain(e, nest)
	case *ast.BlockStmt:
		m.cc++
		m.inside(e.List, nest+1)
	}
}

func (m *measure) clauses(body *ast.BlockStmt, nest int) {
	for _, c := range body.List {
		switch c := c.(type) {
		case *ast.CaseClause:
			for _, e := range c.List {
				m.expr(e, nest)
			}
			m.inside(c.Body, nest+1)
		case *ast.CommClause:
			m.expr(c.Comm, nest)
			m.inside(c.Body, nest+1)
		}
	}
}

// expr 量表达式和简单语句：一串相同的 && 或 || 算一次；函数字面量的里面多一层嵌套。
func (m *measure) expr(n ast.Node, nest int) {
	if n == nil {
		return
	}
	ast.Inspect(n, func(c ast.Node) bool {
		switch c := c.(type) {
		case *ast.FuncLit:
			m.inside(c.Body.List, nest+1)
			return false
		case *ast.BinaryExpr:
			if c.Op == token.LAND || c.Op == token.LOR {
				if l, ok := c.X.(*ast.BinaryExpr); !ok || l.Op != c.Op {
					m.cc++
				}
			}
		}
		return true
	})
}

// errGuard 是出错就返回（或者跳过这一个）的判断：if err != nil { return … }，没有 else。
func errGuard(s *ast.IfStmt) bool {
	if s.Else != nil || len(s.Body.List) != 1 {
		return false
	}
	switch b := s.Body.List[0].(type) {
	case *ast.ReturnStmt:
	case *ast.BranchStmt:
		if b.Tok != token.CONTINUE || b.Label != nil {
			return false
		}
	default:
		return false
	}
	c, ok := s.Cond.(*ast.BinaryExpr)
	if !ok || c.Op != token.NEQ {
		return false
	}
	x, ok1 := c.X.(*ast.Ident)
	y, ok2 := c.Y.(*ast.Ident)
	return ok1 && ok2 && y.Name == "nil" && (strings.HasSuffix(x.Name, "err") || strings.HasSuffix(x.Name, "Err"))
}

// 量法和 SonarSource 的说明一致（前两个是它说明里的例子），加上 Go 的出错就返回不算。
func TestMeasure(t *testing.T) {
	for _, c := range []struct {
		src       string
		cc, depth int
	}{
		{`func sumOfPrimes(max int) int {
			total := 0
		OUT:
			for i := 1; i <= max; i++ {
				for j := 2; j < i; j++ {
					if i%j == 0 {
						continue OUT
					}
				}
				total += i
			}
			return total
		}`, 7, 3},
		{`func getWords(n int) string {
			switch n {
			case 1:
				return "one"
			case 2:
				return "a couple"
			default:
				return "lots"
			}
		}`, 1, 1},
		{`func f(a, b, c bool) error {
			if err := g(); err != nil {
				return err
			}
			for range 3 {
				if a && b || c {
				} else if b {
				} else {
				}
			}
			go func() {
				if a {
				}
			}()
			return nil
		}`, 1 + (2 + 2) + 1 + 1 + 2, 2},
	} {
		f, err := parser.ParseFile(token.NewFileSet(), "", "package p\n"+c.src, 0)
		if err != nil {
			t.Fatal(err)
		}
		fd := f.Decls[0].(*ast.FuncDecl)
		if m := measureFunc(fd.Body); m.cc != c.cc || m.depth != c.depth {
			t.Errorf("%s：复杂度 %d、嵌套 %d，应为 %d、%d", fd.Name.Name, m.cc, m.depth, c.cc, c.depth)
		}
	}
}

// fn 是一个函数量出来的结果。
type fn struct {
	name  string
	lines int
	measure
}

func functions(pkgs []*pkg) []fn {
	var out []fn
	for _, k := range pkgs {
		for _, f := range k.files {
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				out = append(out, fn{name: funcName(k, fd), lines: fset.Position(fd.End()).Line - fset.Position(fd.Pos()).Line + 1,
					measure: measureFunc(fd.Body)})
			}
		}
	}
	return out
}

// TestReport 是结构体检的报告：发布前用 go test ./internal/codecheck -run Report -v 看，数字记进改动记录。
func TestReport(t *testing.T) {
	all := functions(load(t))
	pct := func(get func(fn) int) string {
		v := make([]int, len(all))
		for i, f := range all {
			v[i] = get(f)
		}
		sort.Ints(v)
		p := func(q float64) int { return v[int(q*float64(len(v)-1))] }
		return fmt.Sprintf("中位 %d，95%% %d，99%% %d，最大 %d", p(.5), p(.95), p(.99), v[len(v)-1])
	}
	t.Logf("函数 %d 个", len(all))
	t.Logf("认知复杂度：%s", pct(func(f fn) int { return f.cc }))
	t.Logf("最深嵌套：%s", pct(func(f fn) int { return f.depth }))
	t.Logf("行数：%s", pct(func(f fn) int { return f.lines }))
	sort.Slice(all, func(i, j int) bool { return all[i].cc > all[j].cc })
	for _, f := range all[:15] {
		t.Logf("复杂度 %3d  嵌套 %d  %3d 行  %s", f.cc, f.depth, f.lines, f.name)
	}
}

// ---------- 函数 ----------

// 一个函数的认知复杂度不超过 maxComplexity、嵌套不超过 maxDepth（量法见 measure）。线按本项目自己的分布定：
// 99% 的函数复杂度不超过 44、嵌套不超过 4 层，超过的是最难读的那 1%。行数不设线：长短和难读只是中等相关，
// 平铺的长函数（例如逐个结局写结果摘要）不用拆。
const (
	maxComplexity = 45
	maxDepth      = 4
)

// exception 是超了线、暂时不改的函数：记下现在的数字（只许变小），写明为什么。“待整理”的有现成的拆分点，
// 按说明拆开后删掉这一条；“不拆”的拆开反而更乱。
type exception struct {
	cc, depth int
	why       string
}

var exceptions = map[string]exception{
	"spec Rebuild.Build": {64, 3, "不拆：平铺的一条条配置规则，各条互不相干；拆开要来回传六七个变量，反而难对照"},

	"backup buildPlan":     {59, 5, "待整理：每个容器要的东西（显卡、设备、端口、网络、卷）单独成一个函数"},
	"backup chooseUnits":   {54, 4, "待整理：按 --include 选和在菜单里选分成两个函数"},
	"backup changesStream": {46, 5, "待整理：把一个路径的内容抄进 tar 单独成一个函数"},
}

func TestFunctions(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range functions(load(t)) {
		ex, listed := exceptions[f.name]
		seen[f.name] = listed
		over := f.cc > maxComplexity || f.depth > maxDepth
		switch {
		case over && !listed:
			t.Errorf("%s：认知复杂度 %d、嵌套 %d 层，超过了 %d、%d 层。先找自然的拆分点（一件事一个函数）；拆开反而要来回传一堆变量的，"+
				"在 exceptions 里写明为什么", f.name, f.cc, f.depth, maxComplexity, maxDepth)
		case listed && (f.cc > ex.cc || f.depth > ex.depth):
			t.Errorf("%s 变得更复杂了（复杂度 %d → %d，嵌套 %d → %d）：例外只许变小，先整理（%s）", f.name, ex.cc, f.cc, ex.depth, f.depth, ex.why)
		case listed && !over:
			t.Errorf("%s 已经不超线了（复杂度 %d、嵌套 %d），从 exceptions 里删掉", f.name, f.cc, f.depth)
		case listed && (f.cc < ex.cc || f.depth < ex.depth):
			t.Errorf("%s 变简单了（复杂度 %d、嵌套 %d），把 exceptions 里的数字改小", f.name, f.cc, f.depth)
		}
	}
	for name := range exceptions {
		if _, ok := seen[name]; !ok {
			t.Errorf("exceptions 里的 %s 找不到了，删掉这一条", name)
		}
	}
}

// ---------- 包之间 ----------

// layers 是每个包在第几层：只能用比自己低的层里的包（Go 本来就不许循环引用，这里再管住方向）。第 0 层不用
// 项目里的别的包；restore 和 backup 在同一层，互相不用；cli 是唯一把它们接起来的地方。新加的包先在这里定好层。
var layers = map[string]int{
	"host": 0, "logging": 0, "crypto": 0, "docker": 0, "compose": 0, "testpoint": 0, "ui": 0,
	"spec": 1, "bundle": 1, "transfer": 1, "helper": 1, "docker/dockertest": 1,
	"preflight": 2,
	"restore":   3, "backup": 3,
	"cli":         4,
	"cmd/dockhop": 5,
}

// testOnly 是只给测试用的包，正式代码不能用：不编进发布的程序。
var testOnly = map[string]bool{"docker/dockertest": true}

const module = "github.com/lx969788249/dockhop/"

func TestLayers(t *testing.T) {
	for _, k := range load(t) {
		if len(k.files) == 0 {
			continue // 只有测试的包（例如这里）
		}
		mine, ok := layers[k.path]
		if !ok {
			t.Errorf("新的包 %s 还没定在第几层：在 layers 里加上", k.path)
			continue
		}
		for _, f := range k.files {
			for _, im := range f.Imports {
				dep, ok := strings.CutPrefix(strings.Trim(im.Path.Value, `"`), module+"internal/")
				if !ok {
					continue
				}
				switch l, known := layers[dep]; {
				case testOnly[dep] && k.path != dep:
					t.Errorf("%s 用了只给测试用的 %s（%s）", k.path, dep, fset.Position(im.Pos()))
				case !known:
					t.Errorf("%s 用的 %s 还没定在第几层", k.path, dep)
				case l >= mine:
					t.Errorf("%s（第 %d 层）不能用 %s（第 %d 层）：只能用更低的层（%s）", k.path, mine, dep, l, fset.Position(im.Pos()))
				}
			}
		}
	}
}

// ---------- 这两轮整理定下的规矩 ----------

// calls 返回正式代码里 sel 这些地方出现在哪些函数里（调用或者当作值用），例如 os.Getenv、x.StepFailed。
func calls(pkgs []*pkg, match func(*ast.SelectorExpr) bool) map[string]bool {
	out := map[string]bool{}
	for _, k := range pkgs {
		for _, f := range k.files {
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					if s, ok := n.(*ast.SelectorExpr); ok && match(s) {
						out[funcName(k, fd)] = true
					}
					return true
				})
			}
		}
	}
	return out
}

func sameSet(got map[string]bool, want []string) bool {
	var g []string
	for k := range got {
		g = append(g, k)
	}
	sort.Strings(g)
	w := slices.Clone(want)
	sort.Strings(w)
	return slices.Equal(g, w)
}

// 报错和界面分开：逻辑出错时返回 ui.Failure，不自己显示；Screen.StepFailed 只在迁入、迁出的 Run 里各一处，
// 加上一组服务没能启动时当场显示（groupFailed，这一步接着启动别的组）。
func TestStepFailedOnlyAtTop(t *testing.T) {
	want := []string{"restore Run", "backup Run", "restore migrate.groupFailed"}
	got := calls(load(t), func(s *ast.SelectorExpr) bool { return s.Sel.Name == "StepFailed" })
	if !sameSet(got, want) {
		t.Errorf("StepFailed 只能在 %v 里用，现在在 %v：逻辑里出错返回 *ui.Failure", want, keys(got))
	}
}

// 迁入、迁出不读环境变量：cli 读一次（测试用的设置见 testpoint.FromEnv），经 Deps 传下去；docker 客户端
// 读 DOCKER_HOST，和 docker 命令一样。
func TestEnvOnlyAtTop(t *testing.T) {
	want := []string{"cli Env.fill", "docker New"}
	got := calls(load(t), func(s *ast.SelectorExpr) bool {
		x, ok := s.X.(*ast.Ident)
		return ok && (x.Name == "os" && slices.Contains([]string{"Getenv", "LookupEnv", "Environ", "ExpandEnv"}, s.Sel.Name) ||
			x.Name == "syscall" && s.Sel.Name == "Getenv")
	})
	if !sameSet(got, want) {
		t.Errorf("读环境变量只能在 %v 里，现在在 %v：要用的设置经 Deps 传进去", want, keys(got))
	}
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// 测试不改包级变量：要改短的时限、要模拟的失败经 Deps 传进去（testpoint.Settings），别的测试不会被它影响。
func TestTestsKeepPackageVars(t *testing.T) {
	pkgs := load(t)
	vars := map[string]map[string]bool{} // 包 → 包级变量
	for _, k := range pkgs {
		vars[k.path] = map[string]bool{}
		for _, f := range k.files {
			for _, d := range f.Decls {
				if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.VAR {
					for _, s := range g.Specs {
						for _, n := range s.(*ast.ValueSpec).Names {
							vars[k.path][n.Name] = true
						}
					}
				}
			}
		}
	}
	for _, k := range pkgs {
		for _, f := range k.tests {
			imported := map[string]string{} // 包名 → 包
			for _, im := range f.Imports {
				if dep, ok := strings.CutPrefix(strings.Trim(im.Path.Value, `"`), module+"internal/"); ok {
					name := dep[strings.LastIndex(dep, "/")+1:]
					if im.Name != nil {
						name = im.Name.Name
					}
					imported[name] = dep
				}
			}
			ast.Inspect(f, func(n ast.Node) bool {
				a, ok := n.(*ast.AssignStmt)
				if !ok || a.Tok != token.ASSIGN {
					return true
				}
				for _, l := range a.Lhs {
					switch x := l.(type) {
					case *ast.Ident: // 没认出是局部变量，就是包级的
						if x.Obj == nil && vars[k.path][x.Name] {
							t.Errorf("%s：测试改了包级变量 %s，改成经 Deps 传进去", fset.Position(a.Pos()), x.Name)
						}
					case *ast.SelectorExpr:
						if p, ok := x.X.(*ast.Ident); ok && p.Obj == nil && vars[imported[p.Name]][x.Sel.Name] {
							t.Errorf("%s：测试改了 %s 的包级变量 %s", fset.Position(a.Pos()), imported[p.Name], x.Sel.Name)
						}
					}
				}
				return true
			})
		}
	}
}

// 测试里迁入、迁出的 run（和迁入的 migrate、undo）只用 testRun、testMigrate、testUndo 造（在 helpers_test.go）：
// 自己拼的话，没给的依赖是空的（以前回滚资料写进过测试所在的目录）。
func TestTestsBuildRunsThroughHelpers(t *testing.T) {
	for _, k := range load(t) {
		if k.path != "restore" && k.path != "backup" {
			continue
		}
		for _, f := range k.tests {
			if filepath.Base(fset.Position(f.Pos()).Filename) == "helpers_test.go" {
				continue
			}
			ast.Inspect(f, func(n ast.Node) bool {
				if c, ok := n.(*ast.CompositeLit); ok {
					if id, ok := c.Type.(*ast.Ident); ok && (id.Name == "run" || id.Name == "migrate" || id.Name == "undo") {
						t.Errorf("%s：测试里直接拼 %s{}，改用 helpers_test.go 里的 test%s", fset.Position(c.Pos()), id.Name,
							strings.ToUpper(id.Name[:1])+id.Name[1:])
					}
				}
				return true
			})
		}
	}
}
