package backup

import (
	"testing"

	"github.com/lx969788249/dockhop/internal/compose"
	"github.com/lx969788249/dockhop/internal/docker"
)

func TestDriftReason(t *testing.T) {
	str := func(s string) *string { return &s }
	var c docker.Container
	c.Config.Image = "nginx:1.27"
	c.Config.Env = []string{"PATH=/usr/bin", "MODE=prod", "FROM_FILE=yes"}
	same := compose.Service{Image: "nginx:1.27", Environment: map[string]*string{"MODE": str("prod"), "FROM_FILE": str("yes"), "PASSTHROUGH": nil}}
	if why := driftReason(same, &c); why != "" {
		t.Errorf("没有改动时不应报告：%s", why)
	}
	changed := compose.Service{Image: "nginx:1.27", Environment: map[string]*string{"MODE": str("dev"), "NEW": str("1")}}
	if why := driftReason(changed, &c); why != "环境变量 MODE、NEW" {
		t.Errorf("环境变量不同应报告：%q", why)
	}
	if why := driftReason(compose.Service{Image: "nginx:1.29"}, &c); why != "镜像 nginx:1.27 → nginx:1.29" {
		t.Errorf("镜像不同应报告：%q", why)
	}
}
