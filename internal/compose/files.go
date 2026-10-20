package compose

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// DeclaredFiles 直接读原始的 Compose 文件，找出里面写到的、在项目目录以外的文件和
// 目录：env_file、build 上下文、configs、secrets、include。
//
// 不同版本的 docker compose config 输出差别很大（新版会把 env_file 并进环境变量、
// 不再列出路径，而且有的版本加了 --no-env-resolution 也一样），所以不依赖它的输出。
// 路径里有 ${变量} 的无法确定，跳过。
func DeclaredFiles(p Project) []string {
	seen := map[string]bool{}
	add := func(path string) {
		if path == "" || strings.Contains(path, "${") || strings.Contains(path, "://") {
			return
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(p.WorkingDir, path)
		}
		path = filepath.Clean(path)
		if path == p.WorkingDir || strings.HasPrefix(path, p.WorkingDir+string(filepath.Separator)) {
			return
		}
		if _, err := os.Stat(path); err == nil {
			seen[path] = true
		}
	}
	for _, f := range p.Files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var doc struct {
			Services map[string]struct {
				EnvFile yaml.Node `yaml:"env_file"`
				Build   yaml.Node `yaml:"build"`
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
			continue
		}
		for _, s := range doc.Services {
			for _, v := range stringsOf(&s.EnvFile, "path") {
				add(v)
			}
			for _, v := range stringsOf(&s.Build, "context") {
				add(v)
			}
		}
		for _, c := range doc.Configs {
			add(c.File)
		}
		for _, c := range doc.Secrets {
			add(c.File)
		}
		for _, v := range stringsOf(&doc.Include, "path") {
			add(v)
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
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
