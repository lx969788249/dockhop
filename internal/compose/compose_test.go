package compose

import (
	"reflect"
	"testing"
)

func TestReferencedOutside(t *testing.T) {
	cfg := `{
	  "services": {
	    "web": {"env_file": ["/opt/app/.env", "/etc/shared/common.env"], "build": {"context": "/opt/app/web"}},
	    "api": {"env_file": [{"path": "/srv/secrets/api.env", "required": true}], "build": {"context": "https://github.com/x/y.git"}},
	    "db":  {}
	  },
	  "configs": {"nginx": {"file": "/etc/nginx/site.conf"}},
	  "secrets": {"pw": {"file": "./secrets/pw.txt"}}
	}`
	got, err := referencedOutside(cfg, "/opt/app")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/etc/nginx/site.conf", "/etc/shared/common.env", "/srv/secrets/api.env"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("项目目录以外引用的文件：\n实际 %v\n期望 %v", got, want)
	}
	if _, err := referencedOutside("不是 JSON", "/opt/app"); err == nil {
		t.Fatal("输出读不懂时应报错")
	}
}
