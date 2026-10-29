package compose

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// DeclaredFiles 直接读原始的 Compose 文件，找出项目要用、但在项目目录以外的文件和
// 目录：-f 指定的配置文件本身，以及配置里的 env_file、build 上下文、configs、secrets、
// include、extends 写到的文件。include 和 extends 引用的文件也会接着读，里面的相对
// 路径按 Compose 的规则，从那个文件自己的目录算起。
//
// 返回的路径互不包含（目录里的文件不再单独列出），打包时每个文件只出现一次。路径里
// 有变量（$X、${X}）的无法确定是哪个文件，放在 unknown 里，由调用方提醒用户。
//
// 不同版本的 docker compose config 输出差别很大（新版会把 env_file 并进环境变量、
// 不再列出路径，而且有的版本加了 --no-env-resolution 也一样），所以不依赖它的输出。
func DeclaredFiles(p Project) (files, unknown []string) {
	w := &walker{project: p.WorkingDir, read: map[string]bool{}, found: map[string]bool{}, unknown: map[string]bool{}}
	for _, f := range p.Files {
		// -f 的文件里的相对路径都从项目目录算起，不管文件本身在哪。
		w.parse(w.add(f, p.WorkingDir), p.WorkingDir)
	}
	for path := range w.found {
		if !w.covered(path) {
			files = append(files, path)
		}
	}
	for path := range w.unknown {
		unknown = append(unknown, path)
	}
	sort.Strings(files)
	sort.Strings(unknown)
	return files, unknown
}

type walker struct {
	project string          // 项目目录：里面的文件随项目目录一起打包
	read    map[string]bool // 读过的 Compose 文件（include 可能互相引用）
	found   map[string]bool // 项目目录以外、要一起迁移的路径
	unknown map[string]bool
}

// add 记下一个写到的路径，相对路径从 base 算起。返回绝对路径；路径不存在、是网址或者
// 有变量时返回空。
func (w *walker) add(path, base string) string {
	switch {
	case path == "" || strings.Contains(path, "://"):
		return ""
	case strings.Contains(path, "$"):
		w.unknown[path] = true
		return ""
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	path = filepath.Clean(path)
	if _, err := os.Stat(path); err != nil {
		return "" // 旧服务器上也没有（例如 required: false 的 env_file），不用迁移
	}
	if path != w.project && !strings.HasPrefix(path, w.project+string(filepath.Separator)) {
		w.found[path] = true
	}
	return path
}

// covered 表示 path 在另一个要迁移的目录里面，不用单独列出。
func (w *walker) covered(path string) bool {
	for dir := range w.found {
		if strings.HasPrefix(path, dir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// parse 读一个 Compose 文件，记下它写到的文件。base 是它里面的相对路径的起点。
func (w *walker) parse(file, base string) {
	if file == "" || w.read[file] {
		return
	}
	w.read[file] = true
	b, err := os.ReadFile(file)
	if err != nil {
		return
	}
	var doc struct {
		Services map[string]struct {
			EnvFile yaml.Node `yaml:"env_file"`
			Build   yaml.Node `yaml:"build"`
			Extends yaml.Node `yaml:"extends"`
		} `yaml:"services"`
		Configs map[string]struct {
			File string `yaml:"file"`
		} `yaml:"configs"`
		Secrets map[string]struct {
			File string `yaml:"file"`
		} `yaml:"secrets"`
		Include yaml.Node `yaml:"include"`
	}
	if yaml.Unmarshal(b, &doc) != nil {
		return
	}
	for _, s := range doc.Services {
		for _, v := range stringsOf(&s.EnvFile, "path") {
			w.add(v, base)
		}
		for _, v := range stringsOf(&s.Build, "context") {
			w.add(v, base)
		}
		// extends: {file: 另一个文件, service: 名字}；只写名字时是同一个文件里的服务。
		if s.Extends.Kind == yaml.MappingNode {
			for _, v := range stringsOf(&s.Extends, "file") {
				if f := w.add(v, base); f != "" {
					w.parse(f, filepath.Dir(f))
				}
			}
		}
	}
	for _, c := range doc.Configs {
		w.add(c.File, base)
	}
	for _, c := range doc.Secrets {
		w.add(c.File, base)
	}
	entries := doc.Include.Content
	if doc.Include.Kind == yaml.ScalarNode {
		entries = []*yaml.Node{&doc.Include}
	}
	for _, inc := range entries {
		w.include(inc, base)
	}
}

// include 处理 include 的一项：一个路径，或者 {path, project_directory, env_file}。被包含的
// 文件里的相对路径从 project_directory 算起，没写时从（第一个）被包含文件所在的目录算起；
// 没写 env_file 时用 project_directory 里的 .env。
func (w *walker) include(inc *yaml.Node, base string) {
	var files []string
	for _, v := range stringsOf(inc, "path") {
		if f := w.add(v, base); f != "" {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		return
	}
	dir := filepath.Dir(files[0])
	envFiles := []string{".env"}
	if inc.Kind == yaml.MappingNode {
		if pd := stringsOf(inc, "project_directory"); len(pd) == 1 && !strings.Contains(pd[0], "$") {
			dir = filepath.Clean(pd[0])
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(base, dir)
			}
		}
		if ef := stringsOf(inc, "env_file"); len(ef) > 0 {
			for _, v := range ef {
				w.add(v, base)
			}
			envFiles = nil
		}
	}
	for _, v := range envFiles {
		w.add(v, dir)
	}
	for _, f := range files {
		w.parse(f, dir)
	}
}

// stringsOf 取出 Compose 里常见的几种写法里的字符串：单个字符串、字符串列表、
// 或者 {key: 字符串} 的列表/对象。
func stringsOf(n *yaml.Node, key string) []string {
	switch n.Kind {
	case yaml.ScalarNode:
		return []string{n.Value}
	case yaml.SequenceNode:
		var out []string
		for _, c := range n.Content {
			out = append(out, stringsOf(c, key)...)
		}
		return out
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == key {
				return stringsOf(n.Content[i+1], key)
			}
		}
	}
	return nil
}
