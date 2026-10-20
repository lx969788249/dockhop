package logging

import (
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Redactor 在内容写进日志或诊断文件之前隐藏密钥和密码。
//
// 两种方式一起用：
//   - 按值隐藏：程序知道的秘密（链接里的密钥、容器里名字像密码的环境变量的值），
//     不管以什么形式出现都会被替换；
//   - 按写法隐藏：NAME=值、"name": "值" 这类写法里，名字像密码的都隐藏。
type Redactor struct {
	mu      sync.RWMutex
	secrets []string
}

const mask = "***"

// 名字里带这些词的变量当作秘密。宁可多藏，不能漏藏。
const secretName = `[A-Za-z0-9_.-]*(?i:pass|pwd|secret|token|key|credential|auth|salt|cookie|session)[A-Za-z0-9_.-]*`

var patterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	// NAME=值（环境变量、命令行参数）
	{regexp.MustCompile(`(` + secretName + `)=([^\s"'&,;]+)`), "${1}=" + mask},
	// "name": "值"（JSON）
	{regexp.MustCompile(`("` + secretName + `"\s*:\s*)"[^"]*"`), `${1}"` + mask + `"`},
	// name: 值（YAML）
	{regexp.MustCompile(`(?m)^(\s*` + secretName + `\s*:\s+)(\S.*)$`), "${1}" + mask},
	// age 私钥，以及迁移链接 # 后面的密钥
	{regexp.MustCompile(`(?i)age-secret-key-1[0-9a-z]+`), mask},
	{regexp.MustCompile(`(#)[0-9a-z]{58}\b`), "${1}" + mask},
}

// AddSecret 登记一个必须隐藏的值。太短的值（少于 4 个字符）容易误伤正常文字，不登记。
func (r *Redactor) AddSecret(v string) {
	v = strings.TrimSpace(v)
	if len(v) < 4 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.secrets {
		if s == v {
			return
		}
	}
	r.secrets = append(r.secrets, v)
	// 先替换长的，避免短值是长值一部分时只替换掉一半。
	sort.Slice(r.secrets, func(i, j int) bool { return len(r.secrets[i]) > len(r.secrets[j]) })
}

// Redact 返回隐藏了秘密的文字。
func (r *Redactor) Redact(s string) string {
	r.mu.RLock()
	for _, v := range r.secrets {
		s = strings.ReplaceAll(s, v, mask)
	}
	r.mu.RUnlock()
	for _, p := range patterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}
