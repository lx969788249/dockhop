package compose

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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
	got := DeclaredFiles(Project{Name: "p", WorkingDir: proj, Files: []string{main}})
	want := []string{
		filepath.Join(root, "api.env"), filepath.Join(root, "ctx"), filepath.Join(root, "etc", "site.conf"),
		filepath.Join(root, "extra.yml"), filepath.Join(root, "shared.env"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("项目目录以外写到的文件：\n实际 %v\n期望 %v", got, want)
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
func TestServiceReplicas(t *testing.T) {
	var cfg struct {
		Services map[string]Service `json:"services"`
	}
	raw := `{"services":{"a":{"image":"x"},"b":{"deploy":{"replicas":3}},"c":{"scale":2},"d":{"deploy":{"resources":{}}}}}`
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	for svc, want := range map[string]int{"a": 1, "b": 3, "c": 2, "d": 1} {
		if got := cfg.Services[svc].Replicas(); got != want {
			t.Errorf("%s：%d，应为 %d", svc, got, want)
		}
	}
}
