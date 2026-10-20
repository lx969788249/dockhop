package compose

import (
	"os"
	"path/filepath"
	"reflect"
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
