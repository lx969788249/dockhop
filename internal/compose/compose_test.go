package compose

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestDeclaredFiles(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "proj")
	for _, d := range []string{proj, filepath.Join(proj, "web"), filepath.Join(root, "ctx"), filepath.Join(root, "etc")} {
		os.MkdirAll(d, 0o755)
	}
	for _, f := range []string{"shared.env", "api.env", "etc/site.conf", "extra.yml"} {
		os.WriteFile(filepath.Join(root, f), []byte("x"), 0o644)
	}
	os.WriteFile(filepath.Join(proj, "local.env"), []byte("x"), 0o644)
	main := filepath.Join(proj, "compose.yml")
	os.WriteFile(main, []byte(`
include: ["../extra.yml"]
services:
  web:
    env_file: ../shared.env
    build: ./web
  api:
    env_file:
      - path: `+filepath.Join(root, "api.env")+`
        required: true
      - ./local.env
      - ${MISSING}/x.env
    build: {context: ../ctx}
  db:
    env_file: [../does-not-exist.env]
configs:
  nginx: {file: ../etc/site.conf}
secrets:
  pw: {file: ./local.env}
`), 0o644)
	got, unknown := DeclaredFiles(Project{Name: "p", WorkingDir: proj, Files: []string{main}})
	want := []string{
		filepath.Join(root, "api.env"), filepath.Join(root, "ctx"), filepath.Join(root, "etc", "site.conf"),
		filepath.Join(root, "extra.yml"), filepath.Join(root, "shared.env"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("项目目录以外写到的文件：\n实际 %v\n期望 %v", got, want)
	}
	if !reflect.DeepEqual(unknown, []string{"${MISSING}/x.env"}) {
		t.Errorf("带变量的路径要单独列出来提醒用户：%v", unknown)
	}
}

// -f 指定的、项目目录以外的配置文件本身也要迁移；include 要一层层读下去，被包含的文件里
// 的相对路径从它自己的目录算起；extends 引用的文件同样。
func TestDeclaredFilesFollowsReferences(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) string {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
		return p
	}
	main := write("app/compose.yml", "include:\n  - ../common/compose.yml\n  - path: ./sub/compose.yml\n    project_directory: ../lib\nservices:\n  web: {image: alpine}\n")
	prod := write("config/prod.yml", "services:\n  web: {env_file: ../app/prod.env}\n")
	write("app/prod.env", "MODE=prod\n")
	common := write("common/compose.yml", "include: [../nested/compose.yml]\nservices:\n  db: {image: alpine, env_file: ./service.env}\n")
	env := write("common/service.env", "X=1\n")
	commonDotEnv := write("common/.env", "TAG=1\n")
	nested := write("nested/compose.yml", "services:\n  worker:\n    extends: {file: ../base/svc.yml, service: base}\n")
	base := write("base/svc.yml", "services:\n  base: {image: alpine, env_file: ./base.env}\n")
	baseEnv := write("base/base.env", "Y=1\n")
	write("app/sub/compose.yml", "services:\n  lib: {image: alpine, env_file: ./lib.env}\n")
	libEnv := write("lib/lib.env", "Z=1\n")
	got, _ := DeclaredFiles(Project{Name: "app", WorkingDir: filepath.Join(root, "app"), Files: []string{main, prod}})
	want := []string{baseEnv, base, common, commonDotEnv, env, prod, libEnv, nested}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("要一起迁移的文件：\n实际 %v\n期望 %v", got, want)
	}
}

// 同一个目录既是 build 上下文、里面的文件又被 env_file 写到时，只列出目录：
// 否则打包时同一个文件出现两次，迁入时整个包会被拒绝。
func TestDeclaredFilesNoOverlap(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "app"), 0o755)
	os.MkdirAll(filepath.Join(root, "shared"), 0o755)
	os.WriteFile(filepath.Join(root, "shared", "app.env"), []byte("x"), 0o644)
	main := filepath.Join(root, "app", "compose.yml")
	os.WriteFile(main, []byte("services:\n  web: {build: ../shared, env_file: ../shared/app.env}\n"), 0o644)
	got, _ := DeclaredFiles(Project{Name: "app", WorkingDir: filepath.Join(root, "app"), Files: []string{main}})
	if want := []string{filepath.Join(root, "shared")}; !reflect.DeepEqual(got, want) {
		t.Fatalf("实际 %v，期望 %v", got, want)
	}
}

func TestKeyLines(t *testing.T) {
	// 老的 docker-compose 报错后面跟着整段配置内容，要留住说明错误的那一行。
	text := "Creating network\nInvalid interpolation format for \"environment\" option in service \"waline\": \"{%- set x = r/^[0-9]+$/g -%}\n{%- if a -%}\n  b\n{%- endif -%}\""
	got := keyLines(text)
	if !strings.HasPrefix(got, "Invalid interpolation format") || strings.Count(got, "\n") != 2 {
		t.Errorf("应从报错那一行开始、最多 3 行：%q", got)
	}
	if got := keyLines("a\nb\nc\nd"); got != "b\nc\nd" {
		t.Errorf("没有报错字样时取最后 3 行：%q", got)
	}
}

// 副本个数从 docker compose config 的输出里读：deploy.replicas、scale，都没写时是 1。
// 依赖的服务也从这里读。
func TestServiceReplicas(t *testing.T) {
	var cfg struct {
		Services map[string]Service `json:"services"`
	}
	raw := `{"services":{"a":{"image":"x"},"b":{"deploy":{"replicas":3},"depends_on":{"c":{"condition":"service_healthy"},"a":{}}},"c":{"scale":2,"depends_on":["a"]},"d":{"deploy":{"resources":{}}}}}`
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	for svc, want := range map[string]int{"a": 1, "b": 3, "c": 2, "d": 1} {
		if got := cfg.Services[svc].Replicas(); got != want {
			t.Errorf("%s：%d，应为 %d", svc, got, want)
		}
	}
	// depends_on 两种写法都要认得。
	if got := cfg.Services["b"].Needs(); !reflect.DeepEqual(got, []string{"a", "c"}) {
		t.Errorf("b 依赖：%v", got)
	}
	if got := cfg.Services["c"].Needs(); !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("c 依赖：%v", got)
	}
}
