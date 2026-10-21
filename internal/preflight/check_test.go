package preflight

import (
	"bytes"
	"strings"
	"testing"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/host"
	"github.com/lx969788249/dockhop/internal/ui"
)

func testPlan() *bundle.Plan {
	return &bundle.Plan{
		Source: bundle.Source{Arch: "x86_64", Docker: "29.2.1"},
		Sizes:  bundle.PlanSizes{Bundle: 100 << 20, Images: 200 << 20, Volumes: 50 << 20},
		Containers: []bundle.PlanContainer{
			{Name: "web", State: "running", Image: "nginx", Ports: []bundle.PlanPort{{HostPort: "80", ContainerPort: "80", Proto: "tcp"}},
				Networks: []bundle.PlanNetAddr{{Name: "appnet", IP: "172.20.0.10"}}, Volumes: []string{"data"}},
			{Name: "old", State: "exited", Image: "busybox", Ports: []bundle.PlanPort{{HostPort: "22", ContainerPort: "22", Proto: "tcp"}}},
			{Name: "site-app-1", Project: "site", State: "running", Image: "app", Ports: []bundle.PlanPort{{HostPort: "3000", ContainerPort: "3000", Proto: "tcp"}}},
		},
		Projects: []bundle.PlanProject{{Name: "site", Services: []string{"app"}}},
		Volumes:  []bundle.PlanVolume{{Name: "data", Driver: "local", Bytes: 50 << 20}},
		Networks: []bundle.PlanNetwork{{Name: "appnet", Driver: "bridge", Subnets: []string{"172.20.0.0/16"}}},
	}
}

func testTarget() *Target {
	return &Target{
		Arch: "x86_64", DockerVersion: "29.2.1", DockerRoot: "/var/lib/docker", HasCompose: true, Local: true,
		Runtimes: map[string]bool{"runc": true}, Containers: map[string]TargetContainer{}, Projects: map[string]bool{},
		Volumes: map[string]*docker.Volume{}, Networks: map[string]TargetNetwork{}, DockerPorts: map[string]string{},
		ListenersOK: true, Exists: func(string) bool { return true },
		Disk: func(string) (host.Disk, error) { return host.Disk{Path: "/", Free: 100 << 30, Dev: 1}, nil },
	}
}

func kinds(r *Result, level Level) []string {
	var out []string
	for _, i := range r.Issues {
		if i.Level == level {
			out = append(out, i.Kind)
		}
	}
	return out
}

func find(r *Result, kind string) *Issue {
	for i := range r.Issues {
		if r.Issues[i].Kind == kind {
			return &r.Issues[i]
		}
	}
	return nil
}

func TestEvaluateClean(t *testing.T) {
	r := Evaluate(testPlan(), testTarget(), &Decisions{}, Options{})
	if len(r.Issues) != 0 || r.Containers != 3 {
		t.Fatalf("没有问题时不应报告：%+v", r.Issues)
	}
	if r.NeedBytes != (100+200+50)<<20+200<<20 {
		t.Errorf("同一块磁盘时需要的空间：%d", r.NeedBytes)
	}
}

func TestEvaluatePorts(t *testing.T) {
	tg := testTarget()
	tg.Listeners = []host.Listener{{Proto: "tcp", Addr: "0.0.0.0", Port: 80}, {Proto: "tcp", Addr: "0.0.0.0", Port: 81}, {Proto: "tcp", Addr: "::", Port: 22}}
	tg.DockerPorts["tcp/3000"] = "other"
	r := Evaluate(testPlan(), tg, &Decisions{}, Options{})
	var ports []*Issue
	for i := range r.Issues {
		if r.Issues[i].Kind == "port" {
			ports = append(ports, &r.Issues[i])
		}
	}
	if len(ports) != 2 {
		t.Fatalf("应发现 web 的 80 和 site 的 3000 被占用（old 本来就停着，不检查 22）：%+v", r.Issues)
	}
	if ports[0].Container != "web" || ports[0].Suggest != "82" || !strings.Contains(ports[0].Title, "其他程序") {
		t.Errorf("web 的端口问题：%+v", ports[0])
	}
	if ports[1].Project != "site" || !strings.Contains(ports[1].Title, "容器 other") {
		t.Errorf("site 的端口问题：%+v", ports[1])
	}
	// 选择换端口之后不再报告；占用端口的是将被替换的同名容器时也不算冲突。
	d := &Decisions{Ports: []PortChange{{PortRef: *ports[0].Port, NewPort: "8080"}}}
	tg.DockerPorts["tcp/3000"] = "site-app-1"
	tg.Containers["site-app-1"] = TargetContainer{Project: "site"}
	tg.Projects["site"] = true
	r = Evaluate(testPlan(), tg, d, Options{Existing: "replace"})
	if find(r, "port") != nil {
		t.Errorf("不应再有端口问题：%+v", r.Issues)
	}
	// 迁移包里两个容器要同一个端口。
	p := testPlan()
	p.Containers[1].State = "running"
	p.Containers[1].Ports[0].HostPort = "80"
	r = Evaluate(p, testTarget(), &Decisions{}, Options{})
	if i := find(r, "port"); i == nil || !strings.Contains(i.Title, "同时被迁移包里的 web") {
		t.Errorf("应发现迁移包内部的端口冲突：%+v", r.Issues)
	}
}

func TestEvaluateHostIP(t *testing.T) {
	p := testPlan()
	p.Containers[0].Ports[0].HostIP = "10.9.9.9"
	tg := testTarget()
	tg.HostIPs = map[string]bool{"127.0.0.1": true, "192.168.1.5": true}
	r := Evaluate(p, tg, &Decisions{}, Options{})
	i := find(r, "host_ip")
	if i == nil || i.Level != Ask {
		t.Fatalf("新服务器没有这个地址时应询问：%+v", r.Issues)
	}
	d := &Decisions{}
	d.setPort(PortChange{PortRef: *i.Port, NewIP: "", NewPort: "80"})
	if find(Evaluate(p, tg, d, Options{}), "host_ip") != nil {
		t.Error("改绑到所有地址之后不应再报告")
	}
}

func TestEvaluateExisting(t *testing.T) {
	tg := testTarget()
	tg.Containers["web"] = TargetContainer{}
	tg.Projects["site"] = true
	if got := kinds(Evaluate(testPlan(), tg, &Decisions{}, Options{}), Ask); strings.Join(got, ",") != "existing_container,existing_project" {
		t.Errorf("不写 --existing 时应询问：%v", got)
	}
	if got := kinds(Evaluate(testPlan(), tg, &Decisions{}, Options{Existing: "fail"}), Stop); len(got) != 2 {
		t.Errorf("--existing fail 时应停下：%v", got)
	}
	r := Evaluate(testPlan(), tg, &Decisions{}, Options{Existing: "replace"})
	if len(kinds(r, Ask)) != 0 || len(kinds(r, Info)) != 2 {
		t.Errorf("--existing replace 时只提示：%+v", r.Issues)
	}
	if find(Evaluate(testPlan(), tg, &Decisions{}, Options{Existing: "skip"}), "skip_with_data") == nil {
		t.Error("--existing skip 且迁移包里有数据时应停下")
	}
	if len(kinds(Evaluate(testPlan(), tg, &Decisions{ReplaceContainers: []string{"web"}, SkipProjects: []string{"site"}}, Options{}), Ask)) != 0 {
		t.Error("做了选择之后不应再询问")
	}
}

func TestEvaluateNetworksAndVolumes(t *testing.T) {
	tg := testTarget()
	tg.Networks["other"] = TargetNetwork{Driver: "bridge", Subnets: []string{"172.20.5.0/24"}}
	r := Evaluate(testPlan(), tg, &Decisions{}, Options{})
	if find(r, "subnet_conflict") == nil {
		t.Errorf("网段重叠：%+v", r.Issues)
	}
	if find(Evaluate(testPlan(), tg, &Decisions{AutoSubnet: []string{"appnet"}}, Options{}), "subnet_conflict") != nil {
		t.Error("选择自动网段后不应再报告")
	}
	tg = testTarget()
	tg.Networks["appnet"] = TargetNetwork{Driver: "bridge", Subnets: []string{"172.30.0.0/16"}, IPs: map[string]string{}}
	if find(Evaluate(testPlan(), tg, &Decisions{}, Options{}), "network_mismatch") == nil {
		t.Error("同名网络网段不同时应询问")
	}
	tg.Networks["appnet"] = TargetNetwork{Driver: "bridge", Subnets: []string{"172.20.0.0/16"}, IPs: map[string]string{"172.20.0.10": "someone"}}
	r = Evaluate(testPlan(), tg, &Decisions{}, Options{})
	if find(r, "network_mismatch") != nil || find(r, "ip_conflict") == nil {
		t.Errorf("同名同网段网络沿用；固定 IP 被占用要询问：%+v", r.Issues)
	}
	tg = testTarget()
	tg.Volumes["data"] = &docker.Volume{Name: "data", Driver: "nfs"}
	if find(Evaluate(testPlan(), tg, &Decisions{}, Options{}), "volume_mismatch") == nil {
		t.Error("同名卷存储方式不同时应询问")
	}
	tg.Volumes["data"] = &docker.Volume{Name: "data", Driver: "local"}
	if i := find(Evaluate(testPlan(), tg, &Decisions{}, Options{}), "volume_replace"); i == nil || i.Level != Info {
		t.Error("同名同存储方式的卷只提示会替换")
	}
}

func TestEvaluateStops(t *testing.T) {
	tg := testTarget()
	tg.Arch = "aarch64"
	tg.HasCompose = false
	tg.Local = false
	p := testPlan()
	p.Binds = []bundle.PlanBind{{Path: "/root/.dockhop/x"}}
	p.Containers[0].GPU = true
	p.Containers[0].Devices = []string{"/dev/ttyUSB0"}
	tg.Exists = func(string) bool { return false }
	r := Evaluate(p, tg, &Decisions{}, Options{Home: "/root/.dockhop"})
	got := strings.Join(kinds(r, Stop), ",")
	for _, want := range []string{"arch", "compose", "remote_docker", "gpu", "device", "bind_home"} {
		if !strings.Contains(got, want) {
			t.Errorf("应停下：%s（实际 %s）", want, got)
		}
	}
}

func TestEvaluateDiskAndDigest(t *testing.T) {
	tg := testTarget()
	tg.Disk = func(string) (host.Disk, error) { return host.Disk{Path: "/", Free: 100 << 20, Dev: 1}, nil }
	if i := find(Evaluate(testPlan(), tg, &Decisions{}, Options{}), "disk_base"); i == nil || !strings.Contains(i.Title, "550MB") {
		t.Errorf("磁盘不够：%+v", i)
	}
	if find(Evaluate(testPlan(), tg, &Decisions{}, Options{BundleOnDisk: true}), "disk_base") == nil {
		t.Error("迁移包已在本机时仍要检查解开需要的空间")
	}
	p := testPlan()
	p.Containers[0].DigestPinned = true
	p.Containers[0].Image = "nginx:1.25@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	tg = testTarget()
	tg.Pullable = func(string) bool { return false }
	if find(Evaluate(p, tg, &Decisions{}, Options{}), "digest") == nil {
		t.Error("拉不到 digest 镜像时应询问")
	}
	if find(Evaluate(p, tg, &Decisions{BundleImages: []string{p.Containers[0].Image}}, Options{}), "digest") != nil {
		t.Error("选择改用迁移包里的镜像后不应再询问")
	}
}

func TestRunnerAuto(t *testing.T) {
	tg := testTarget()
	tg.Listeners = []host.Listener{{Proto: "tcp", Addr: "0.0.0.0", Port: 80}}
	tg.Containers["web"] = TargetContainer{}
	var out bytes.Buffer
	r := &Runner{Screen: ui.New(&out, false, nil), Prompt: ui.NewPrompter(nil, &out), Auto: true,
		Collect: func() (*Target, error) { return tg, nil }}
	d := &Decisions{}
	if _, err := r.Run(testPlan(), d, Options{}); err != nil {
		t.Fatalf("--yes 应自动处理：%v\n%s", err, out.String())
	}
	if len(d.Ports) != 1 || d.Ports[0].NewPort != "81" || !has(d.ReplaceContainers, "web") {
		t.Errorf("自动处理的结果：%+v", d)
	}
	sum := strings.Join(d.Summary(), "\n")
	if !strings.Contains(sum, "端口变更：web 80 → 81") {
		t.Errorf("结果里应列出调整：%s", sum)
	}
	// 不能提问、也没有 --yes：列出问题后停下。
	r = &Runner{Screen: ui.New(&out, false, nil), Prompt: ui.NewPrompter(nil, &out), Collect: func() (*Target, error) { return tg, nil }}
	_, err := r.Run(testPlan(), &Decisions{}, Options{})
	if p, ok := err.(*Problem); !ok || !strings.Contains(p.Hint, "--yes") {
		t.Errorf("应提示 --yes：%v", err)
	}
}

func TestRunnerInteractive(t *testing.T) {
	tg := testTarget()
	tg.Listeners = []host.Listener{{Proto: "tcp", Addr: "0.0.0.0", Port: 80}}
	var out bytes.Buffer
	// 选“输入其他端口”，先输入错的，再输入 8081。
	in := strings.NewReader("2\nabc\n8081\n")
	r := &Runner{Screen: ui.New(&out, false, nil), Prompt: ui.NewPrompter(in, &out), Collect: func() (*Target, error) { return tg, nil }}
	d := &Decisions{}
	if _, err := r.Run(testPlan(), d, Options{}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(d.Ports) != 1 || d.Ports[0].NewPort != "8081" || !strings.Contains(out.String(), "请输入 1 到 65535") {
		t.Errorf("交互处理：%+v\n%s", d, out.String())
	}
}

func TestStripDigest(t *testing.T) {
	d := "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := map[string]string{
		"nginx:1.25@" + d:            "nginx:1.25",
		"ghcr.io/a/b@" + d:           "ghcr.io/a/b:pinned-0123456789ab",
		"localhost:5000/app@" + d:    "localhost:5000/app:pinned-0123456789ab",
		"localhost:5000/app:v1@" + d: "localhost:5000/app:v1",
		"nginx:1.25":                 "nginx:1.25",
	}
	for in, want := range cases {
		if got := StripDigest(in); got != want {
			t.Errorf("%s → %s，期望 %s", in, got, want)
		}
	}
}
