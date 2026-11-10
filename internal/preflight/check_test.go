package preflight

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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
		Runtimes: map[string]bool{"runc": true}, Containers: map[string]TargetContainer{},
		Volumes: map[string]*docker.Volume{}, Networks: map[string]TargetNetwork{},
		ListenersOK: true, Exists: func(string) bool { return true }, HasContent: func(string) bool { return false },
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

// Compose 项目的文件和容器里的改动也会解开、写入，要算进需要的磁盘空间。
func TestDiskCountsProjectsAndChanges(t *testing.T) {
	p := testPlan()
	p.Sizes.Projects, p.Sizes.Changes = 30<<20, 40<<20
	r := Evaluate(p, testTarget(), &Decisions{}, Options{})
	if r.NeedBytes != (100+200+50)<<20+200<<20+(30+40)<<20 {
		t.Errorf("需要的空间：%d", r.NeedBytes)
	}
}

func TestEvaluatePorts(t *testing.T) {
	tg := testTarget()
	tg.Listeners = []host.Listener{{Proto: "tcp", Addr: "0.0.0.0", Port: 80}, {Proto: "tcp", Addr: "0.0.0.0", Port: 81}, {Proto: "tcp", Addr: "::", Port: 22}}
	tg.DockerPorts = append(tg.DockerPorts, DockerPort{Proto: "tcp", Port: "3000", Container: "other"})
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
	tg.DockerPorts = []DockerPort{{Proto: "tcp", Port: "3000", Container: "site-app-1"}}
	tg.Containers["site-app-1"] = TargetContainer{Project: "site"}
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
	tg.Containers["site-app-1"] = TargetContainer{Project: "site"}
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
	if find(Evaluate(testPlan(), tg, &Decisions{}, Options{Existing: "skip"}), "skip_with_data") != nil {
		t.Error("保留的服务的数据没有别的容器要写入，不用处理")
	}
	if len(kinds(Evaluate(testPlan(), tg, &Decisions{ReplaceContainers: []string{"web"}, SkipProjects: []string{"site"}}, Options{}), Ask)) != 0 {
		t.Error("做了选择之后不应再询问")
	}
}

// 保留新服务器上已有的服务时，和它共用数据的其他容器迁入会覆盖它的数据：选择保留和
// --existing skip 一样要处理。保留的服务用到的数据按新、旧服务器上的挂载一起算。
func TestEvaluateKeptData(t *testing.T) {
	p := testPlan()
	p.Containers[1].Volumes = []string{"data"} // old 和 web 共用卷 data
	p.Containers[2].Binds = []string{"/srv/shared/site"}
	tg := testTarget()
	tg.Containers["web"] = TargetContainer{Binds: []string{"/srv/shared"}}
	for name, d := range map[string]*Decisions{
		"选择保留":            {SkipContainers: []string{"web"}},
		"--existing skip": {},
	} {
		o := Options{}
		if d.SkipContainers == nil {
			o.Existing = "skip"
		}
		r := Evaluate(p, tg, d, o)
		var got []string
		for _, i := range r.Issues {
			if i.Kind == "skip_with_data" {
				got = append(got, i.Container)
			}
		}
		// old 和旧服务器上的 web 共用卷；site 的目录在新服务器上的 web 挂载的目录里面。
		if strings.Join(got, ",") != "old,site-app-1" {
			t.Errorf("%s：%v", name, r.Issues)
		}
	}
	if find(Evaluate(p, tg, &Decisions{ReplaceContainers: []string{"web"}}, Options{}), "skip_with_data") != nil {
		t.Error("替换已有的服务时不用处理")
	}
	d := &Decisions{SkipContainers: []string{"web"}}
	r := &Runner{Screen: ui.New(io.Discard, false, nil), Prompt: ui.NewPrompter(strings.NewReader("1\n1\n"), io.Discard),
		Collect: func() (*Target, error) { return tg, nil }}
	if _, err := r.Run(p, d, Options{}); err != nil || !d.SkippedContainer(p.Containers[1]) || !slices.Contains(d.SkipProjects, "site") {
		t.Fatalf("选择一起跳过之后应能继续：%v %+v", err, d)
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
	// 选项条数一样、名字不同：值都是空的也是不同的存储方式（以前会当成一样）。
	tg.Volumes["data"] = &docker.Volume{Name: "data", Driver: "local", Options: map[string]string{"device": ""}}
	p := testPlan()
	p.Volumes[0].Opts = map[string]string{"type": ""}
	if find(Evaluate(p, tg, &Decisions{}, Options{}), "volume_mismatch") == nil {
		t.Error("同名卷的选项名字不同时应询问")
	}
	// 新服务器上的同一个目录里已经有东西时，和数据卷一样提示会被替换；没有这个目录、或者是空目录时不提。
	p = testPlan()
	p.Containers[0].Binds = []string{"/srv/web"}
	p.Binds = []bundle.PlanBind{{Path: "/srv/web", Bytes: 1 << 20}}
	tg = testTarget()
	tg.HasContent = func(path string) bool { return path == "/srv/web" }
	if i := find(Evaluate(p, tg, &Decisions{}, Options{}), "bind_replace"); i == nil || i.Level != Info || !strings.Contains(i.Title, "/srv/web") {
		t.Errorf("已有的目录会被替换时应提示：%+v", i)
	}
	tg.HasContent = func(string) bool { return false }
	if i := find(Evaluate(p, tg, &Decisions{}, Options{}), "bind_replace"); i != nil {
		t.Errorf("目录不存在或是空的，不用提示：%+v", i)
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

// Docker 不在这台服务器上时，替换已有的数据卷（包括写进匿名卷：新建的匿名卷里有镜像带的文件）要先
// 清空，清空要把本机的程序放进临时容器，做不到：没有目录和项目也要在下载之前停下。只写进新建的
// 有名字的卷不用清空，不停。
func TestEvaluateRemoteDockerVolumes(t *testing.T) {
	const anon = "3977206abb71f8aa4677d89b605e71e99ae3db6773688b6f62e61fbbadf9c34c"
	for _, c := range []struct {
		name     string
		existing bool   // 新服务器上已经有 data 这个卷
		volume   string // web 用的卷
		stop     bool
	}{
		{"替换已有的卷", true, "data", true},
		{"写进匿名卷", false, anon, true},
		{"新建的卷", false, "data", false},
	} {
		p := testPlan()
		p.Containers = p.Containers[:1] // 只有 web：没有 Compose 项目
		p.Containers[0].Volumes = []string{c.volume}
		p.Volumes = []bundle.PlanVolume{{Name: c.volume, Driver: "local", Bytes: 1 << 20}}
		tg := testTarget()
		tg.Local = false
		if c.existing {
			tg.Volumes["data"] = &docker.Volume{Name: "data", Driver: "local"}
		}
		i := find(Evaluate(p, tg, &Decisions{}, Options{}), "remote_docker")
		if got := i != nil && i.Level == Stop; got != c.stop {
			t.Errorf("%s：应停下 %v，实际 %+v", c.name, c.stop, i)
		}
	}
}

func TestEvaluateDiskAndDigest(t *testing.T) {
	tg := testTarget()
	tg.Disk = func(string) (host.Disk, error) { return host.Disk{Path: "/", Free: 100 << 20, Dev: 1}, nil }
	if i := find(Evaluate(testPlan(), tg, &Decisions{}, Options{}), "disk_base"); i == nil || !strings.Contains(i.Title, "550MB") {
		t.Errorf("磁盘不够：%+v", i)
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
	if len(d.Ports) != 1 || d.Ports[0].NewPort != "81" || !slices.Contains(d.ReplaceContainers, "web") {
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

// 只是说一声的（新服务器的 Docker 旧一点）不算要留意的问题：显示成说明，不算警告。
func TestRunnerRaised(t *testing.T) {
	tg := testTarget()
	tg.DockerVersion = "27.5.1"
	var out bytes.Buffer
	s := ui.New(&out, false, nil)
	r := &Runner{Screen: s, Prompt: ui.NewPrompter(nil, &out), Collect: func() (*Target, error) { return tg, nil }}
	if _, err := r.Run(testPlan(), &Decisions{}, Options{}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if r.Raised() || len(s.Warnings()) != 0 || !strings.Contains(out.String(), "· 新服务器的 Docker（27.5.1）比旧服务器（29.2.1）旧") {
		t.Errorf("只是说一声：Raised=%v 警告=%v\n%s", r.Raised(), s.Warnings(), out.String())
	}

	tg.Containers["web"] = TargetContainer{}
	r = &Runner{Screen: s, Prompt: ui.NewPrompter(nil, &out), Collect: func() (*Target, error) { return tg, nil }}
	if _, err := r.Run(testPlan(), &Decisions{}, Options{Existing: "replace"}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !r.Raised() || len(s.Warnings()) == 0 {
		t.Errorf("要替换同名容器时应提醒：Raised=%v 警告=%v", r.Raised(), s.Warnings())
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

func TestEvaluateOldCompose(t *testing.T) {
	p := testPlan()
	p.Source.Compose = "5.0.2"
	tg := testTarget()
	tg.Compose = "1.29.2"
	if i := find(Evaluate(p, tg, &Decisions{}, Options{}), "old_compose"); i == nil || i.Level != Info {
		t.Error("新服务器只有老的 docker-compose 时应提前提醒")
	}
	tg.Compose = "2.30.0"
	if find(Evaluate(p, tg, &Decisions{}, Options{}), "old_compose") != nil {
		t.Error("新版 Compose 不用提醒")
	}
}

// 端口绑定在新服务器没有的地址上、端口又被占用：先改绑所有地址、再换端口，两次选择都要保留，
// 不能后一次把前一次冲掉（以前会来回问个没完）。交互和 --yes 都要能处理完。
func TestRunnerIPThenPort(t *testing.T) {
	plan := func() *bundle.Plan {
		p := testPlan()
		p.Containers = p.Containers[:1]
		p.Containers[0].Ports[0].HostIP = "10.9.9.9"
		return p
	}
	tg := testTarget()
	tg.HostIPs = map[string]bool{"192.168.1.5": true}
	tg.Listeners = []host.Listener{{Proto: "tcp", Addr: "0.0.0.0", Port: 80}}
	for _, auto := range []bool{false, true} {
		var out bytes.Buffer
		in := io.Reader(strings.NewReader("1\n1\n"))
		if auto {
			in = nil
		}
		r := &Runner{Screen: ui.New(&out, false, nil), Prompt: ui.NewPrompter(in, &out), Auto: auto,
			Collect: func() (*Target, error) { return tg, nil }}
		d := &Decisions{}
		if _, err := r.Run(plan(), d, Options{}); err != nil {
			t.Fatalf("--yes=%v：应能处理完：%v\n%s", auto, err, out.String())
		}
		if len(d.Ports) != 1 || d.Ports[0].NewIP != "" || d.Ports[0].NewPort != "81" {
			t.Fatalf("--yes=%v：应改绑所有地址并换到 81：%+v", auto, d.Ports)
		}
		sum := strings.Join(d.Summary(), "\n")
		if !strings.Contains(sum, "端口变更：web 80 → 81") || !strings.Contains(sum, "改绑到所有地址") {
			t.Errorf("--yes=%v：结果里两项调整都要写：%s", auto, sum)
		}
	}
}

// 端口冲突按地址判断：绑定在不同的具体地址上可以用同一个端口；同一个地址、或者其中一个是
// 所有地址，才冲突。迁移包里的容器、新服务器上已有的容器、其他程序都一样。
func TestPortConflictsByAddress(t *testing.T) {
	bind := func(name, ip string) bundle.PlanContainer {
		return bundle.PlanContainer{Name: name, State: "running", Ports: []bundle.PlanPort{{HostIP: ip, HostPort: "8080", ContainerPort: "80", Proto: "tcp"}}}
	}
	cases := []struct {
		name       string
		containers []bundle.PlanContainer
		docker     []DockerPort
		listeners  []host.Listener
		conflict   bool
	}{
		{"迁移包里不同的地址", []bundle.PlanContainer{bind("a", "127.0.0.1"), bind("b", "127.0.0.2")}, nil, nil, false},
		{"迁移包里同一个地址", []bundle.PlanContainer{bind("a", "127.0.0.1"), bind("b", "127.0.0.1")}, nil, nil, true},
		{"迁移包里所有地址和具体地址", []bundle.PlanContainer{bind("a", "0.0.0.0"), bind("b", "127.0.0.1")}, nil, nil, true},
		{"已有容器在别的地址", []bundle.PlanContainer{bind("a", "127.0.0.2")}, []DockerPort{{Proto: "tcp", IP: "127.0.0.1", Port: "8080", Container: "x"}}, nil, false},
		{"已有容器在所有地址", []bundle.PlanContainer{bind("a", "127.0.0.2")}, []DockerPort{{Proto: "tcp", IP: "0.0.0.0", Port: "8080", Container: "x"}}, nil, true},
		{"迁移包要所有地址、已有容器在具体地址", []bundle.PlanContainer{bind("a", "")}, []DockerPort{{Proto: "tcp", IP: "127.0.0.1", Port: "8080", Container: "x"}}, nil, true},
		{"其他程序在别的地址", []bundle.PlanContainer{bind("a", "127.0.0.2")}, nil, []host.Listener{{Proto: "tcp", Addr: "127.0.0.1", Port: 8080}}, false},
		{"其他程序在同一个地址", []bundle.PlanContainer{bind("a", "127.0.0.1")}, nil, []host.Listener{{Proto: "tcp", Addr: "127.0.0.1", Port: 8080}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := testPlan()
			p.Containers = c.containers
			tg := testTarget()
			tg.DockerPorts, tg.Listeners = c.docker, c.listeners
			if got := find(Evaluate(p, tg, &Decisions{}, Options{}), "port") != nil; got != c.conflict {
				t.Errorf("冲突：%v，应该：%v", got, c.conflict)
			}
		})
	}
}

// 同一个端口被几个已有容器绑定在不同的地址上：替换掉其中一个，另一个还占着，仍然冲突，
// 不管 Docker 先列出哪个。全都会被替换时不算冲突，Docker 为它们开的监听（docker-proxy）也不算。
func TestPortHeldByOthers(t *testing.T) {
	p := testPlan()
	p.Containers = []bundle.PlanContainer{{Name: "web", State: "running", Ports: []bundle.PlanPort{{HostPort: "8080", ContainerPort: "80", Proto: "tcp"}}}}
	web := DockerPort{Proto: "tcp", IP: "127.0.0.1", Port: "8080", Container: "web"}
	other := DockerPort{Proto: "tcp", IP: "127.0.0.2", Port: "8080", Container: "other"}
	for _, ports := range [][]DockerPort{{web, other}, {other, web}} {
		tg := testTarget()
		tg.Containers["web"], tg.Containers["other"] = TargetContainer{State: "running"}, TargetContainer{State: "running"}
		tg.DockerPorts = ports
		i := find(Evaluate(p, tg, &Decisions{}, Options{Existing: "replace"}), "port")
		if i == nil || !strings.Contains(i.Title, "other") {
			t.Errorf("%v：%+v", ports, i)
		}
	}
	tg := testTarget()
	tg.Containers["web"] = TargetContainer{State: "running"}
	tg.DockerPorts = []DockerPort{web}
	tg.Listeners = []host.Listener{{Proto: "tcp", Addr: "127.0.0.1", Port: 8080}}
	if i := find(Evaluate(p, tg, &Decisions{}, Options{Existing: "replace"}), "port"); i != nil {
		t.Errorf("占用者都会被替换时不算冲突：%+v", i)
	}
}

// 几个容器的端口都被占用时，一轮里推荐的空闲端口互不相同，也不会是迁移包里别的容器要用的，
// --yes 一次全部接受就能通过。
func TestSuggestedPortsDistinct(t *testing.T) {
	p := testPlan()
	p.Containers = nil
	tg := testTarget()
	for i := 0; i < 4; i++ {
		port := 8080 + i
		p.Containers = append(p.Containers, bundle.PlanContainer{Name: fmt.Sprintf("web%d", i+1), State: "running",
			Ports: []bundle.PlanPort{{HostPort: fmt.Sprint(port), ContainerPort: "80", Proto: "tcp"}}})
		tg.Listeners = append(tg.Listeners, host.Listener{Proto: "tcp", Addr: "0.0.0.0", Port: port})
	}
	// 8084 是迁移包里另一个（停着的）容器的端口，不能推荐。
	p.Containers = append(p.Containers, bundle.PlanContainer{Name: "spare", State: "exited",
		Ports: []bundle.PlanPort{{HostPort: "8084", ContainerPort: "80", Proto: "tcp"}}})
	var got []string
	for _, i := range Evaluate(p, tg, &Decisions{}, Options{}).Issues {
		got = append(got, i.Suggest)
	}
	if strings.Join(got, ",") != "8085,8086,8087,8088" {
		t.Errorf("推荐的端口：%v", got)
	}
	r := &Runner{Screen: ui.New(io.Discard, false, nil), Prompt: ui.NewPrompter(nil, io.Discard), Auto: true,
		Collect: func() (*Target, error) { return tg, nil }}
	if _, err := r.Run(p, &Decisions{}, Options{}); err != nil {
		t.Fatal(err)
	}
}

// Compose 项目在 external 网络里用固定 IP，新服务器上同名网络的网段不同：迁入时不会改用户的
// 配置文件，所以不能提供“自动分配 IP”，--yes 也不能自动处理；跳过这个项目之后，网络里剩下
// 的独立容器照常可以改为自动分配。
func TestComposeFixedIPNotAuto(t *testing.T) {
	p := testPlan()
	p.Containers = []bundle.PlanContainer{
		{Name: "app-web-1", Project: "app", State: "running", Networks: []bundle.PlanNetAddr{{Name: "ext", IP: "172.20.0.2"}}},
		{Name: "solo", State: "running", Networks: []bundle.PlanNetAddr{{Name: "ext", IP: "172.20.0.3"}}},
	}
	p.Networks = []bundle.PlanNetwork{{Name: "ext", Driver: "bridge", Subnets: []string{"172.20.0.0/16"}}}
	tg := testTarget()
	tg.Networks["ext"] = TargetNetwork{Driver: "bridge", Subnets: []string{"172.30.0.0/16"}}
	i := find(Evaluate(p, tg, &Decisions{}, Options{}), "network_mismatch")
	if i == nil || i.Project != "app" {
		t.Fatalf("应该按 Compose 项目报告：%+v", i)
	}
	auto := &Runner{Screen: ui.New(io.Discard, false, nil), Prompt: ui.NewPrompter(nil, io.Discard), Auto: true,
		Collect: func() (*Target, error) { return tg, nil }}
	d := &Decisions{}
	if _, err := auto.Run(p, d, Options{}); err == nil || d.AutoIPFor("app-web-1", "ext") {
		t.Fatalf("--yes 不能替 Compose 项目改为自动分配 IP：%v %+v", err, d)
	}
	d = &Decisions{}
	ask := &Runner{Screen: ui.New(io.Discard, false, nil), Prompt: ui.NewPrompter(strings.NewReader("2\n1\n"), io.Discard),
		Collect: func() (*Target, error) { return tg, nil }}
	if _, err := ask.Run(p, d, Options{}); err != nil || !slices.Contains(d.SkipProjects, "app") || !d.AutoIPFor("solo", "ext") {
		t.Fatalf("跳过项目后，独立容器改为自动分配：%v %+v", err, d)
	}
}

// 单独迁入的容器 web 会替换新服务器上同名的 web，不管那个 web 属不属于某个 Compose 项目（迁入时
// 就是这样做的，见 Counterparts）：它占着的端口会让出来，不算冲突；选择保留它时，和它共用数据卷
// 的 worker 也要处理，不然迁入会写进它正在用的数据。
func TestCounterpartsIgnoreTargetProject(t *testing.T) {
	plan := &bundle.Plan{Containers: []bundle.PlanContainer{
		{Name: "web", State: "running", Ports: []bundle.PlanPort{{HostPort: "8080", ContainerPort: "80", Proto: "tcp"}}},
		{Name: "worker", State: "running", Volumes: []string{"shared"}},
	}, Volumes: []bundle.PlanVolume{{Name: "shared", Driver: "local"}}}
	tg := testTarget()
	tg.Containers["web"] = TargetContainer{State: "running", Project: "existing-project", Volumes: []string{"shared"}}
	tg.DockerPorts = []DockerPort{{Proto: "tcp", Port: "8080", Container: "web"}}
	if i := find(Evaluate(plan, tg, &Decisions{ReplaceContainers: []string{"web"}}, Options{}), "port"); i != nil {
		t.Errorf("被替换的 web 让出端口，不算冲突：%+v", i)
	}
	r := Evaluate(plan, tg, &Decisions{SkipContainers: []string{"web"}}, Options{})
	if i := find(r, "skip_with_data"); i == nil || i.Container != "worker" {
		t.Errorf("保留 web 时，和它共用数据卷的 worker 也要处理：%+v", r.Issues)
	}
}

// 单独迁入的容器 web 和新服务器上 Compose 项目 site 里的容器同名，迁移包里也有 site：已有的 web
// 跟着 site 走，不单独问（以前两个都问，选了“替换 web、保留 site”时，检查按保留算、平白给 web
// 换了端口，迁入却把它改名备份了）。保留 site 时 web 的名字被占着，不迁入，也不检查它的端口；
// 替换 site 时 web 随项目替换，让出端口和固定 IP。
func TestCounterpartsFollowProject(t *testing.T) {
	tg := testTarget()
	tg.Containers["web"] = TargetContainer{State: "running", Project: "site"}
	tg.Containers["site-old-1"] = TargetContainer{State: "running", Project: "site"}
	tg.DockerPorts = []DockerPort{{Proto: "tcp", Port: "80", Container: "web"}}
	tg.Networks["appnet"] = TargetNetwork{Driver: "bridge", Subnets: []string{"172.20.0.0/16"}, IPs: map[string]string{"172.20.0.10": "site-old-1"}}
	if got := kinds(Evaluate(testPlan(), tg, &Decisions{}, Options{}), Ask); strings.Join(got, ",") != "existing_project" {
		t.Errorf("已有的 web 跟着 site 走，只问 site：%v", got)
	}
	r := Evaluate(testPlan(), tg, &Decisions{SkipProjects: []string{"site"}}, Options{})
	i := find(r, "existing_container")
	if len(kinds(r, Ask)) != 0 || r.Containers != 1 || i == nil || i.Level != Info || !strings.Contains(i.Title, "属于保留的 Compose 项目 site") {
		t.Errorf("保留 site 时 web 不迁入、不检查它（迁入 %d 个）：%+v", r.Containers, r.Issues)
	}
	r = Evaluate(testPlan(), tg, &Decisions{ReplaceProjects: []string{"site"}}, Options{})
	if len(r.Issues) != 0 || r.Containers != 3 {
		t.Errorf("替换 site 时 web 随项目替换，端口和固定 IP 都让出来：%+v", r.Issues)
	}
}

// --existing skip 保留下来的已有服务，迁移包里和它对应的部分不迁入：不检查它们的端口等（以前照样
// 检查，同名的服务占着自己的端口就报冲突，项目的端口 --yes 也处理不了，迁入就停下了）。
func TestSkipPolicyChecksOnlyMigrated(t *testing.T) {
	tg := testTarget()
	tg.Containers["web"] = TargetContainer{State: "running"}
	tg.Containers["site-app-1"] = TargetContainer{State: "running", Project: "site"}
	tg.DockerPorts = []DockerPort{{Proto: "tcp", Port: "80", Container: "web"}, {Proto: "tcp", Port: "3000", Container: "site-app-1"}}
	r := Evaluate(testPlan(), tg, &Decisions{}, Options{Existing: "skip"})
	if len(kinds(r, Ask)) != 0 || len(kinds(r, Info)) != 2 || r.Containers != 1 {
		t.Errorf("只迁入、只检查 old（迁入 %d 个）：%+v", r.Containers, r.Issues)
	}
}

// Compose 项目 site 里写了 container_name: nginx 的容器，和新服务器上无关的已有容器 nginx 同名（独立容器，
// 或者属于迁移包里没有的项目）：Compose 创建容器时名字冲突，迁入失败、整体回滚。已有的 nginx 跟着 site 走
// （见 Counterparts）：问 site；替换 site 时它一起替换，让出端口；保留 site、--existing skip 时 site 不迁入，
// 不检查它；--existing fail 时停下（以前这些都发现不了，还把它占着的端口当成冲突）。
func TestCounterpartsFollowNamedProject(t *testing.T) {
	p := testPlan()
	p.Containers = append(p.Containers, bundle.PlanContainer{Name: "nginx", Project: "site", State: "running", Image: "nginx",
		Ports: []bundle.PlanPort{{HostPort: "443", ContainerPort: "443", Proto: "tcp"}}})
	for _, owner := range []string{"", "proxy"} {
		tg := testTarget()
		tg.Containers["nginx"] = TargetContainer{State: "running", Project: owner}
		tg.DockerPorts = []DockerPort{{Proto: "tcp", Port: "443", Container: "nginx"}}
		r := Evaluate(p, tg, &Decisions{}, Options{})
		if i := find(r, "existing_project"); strings.Join(kinds(r, Ask), ",") != "existing_project" || i.Project != "site" || !strings.Contains(i.Title, "容器 nginx") {
			t.Errorf("已有的 nginx（所属项目 %q）跟着 site 走，只问 site：%+v", owner, r.Issues)
		}
		if r := Evaluate(p, tg, &Decisions{ReplaceProjects: []string{"site"}}, Options{}); len(r.Issues) != 0 || r.Containers != 4 {
			t.Errorf("替换 site 时已有的 nginx 一起替换，让出端口（所属项目 %q）：%+v", owner, r.Issues)
		}
		for name, c := range map[string]struct {
			d *Decisions
			o Options
		}{"保留 site": {&Decisions{SkipProjects: []string{"site"}}, Options{}}, "--existing skip": {&Decisions{}, Options{Existing: "skip"}}} {
			if r := Evaluate(p, tg, c.d, c.o); len(kinds(r, Ask)) != 0 || r.Containers != 2 {
				t.Errorf("%s：site 不迁入，不检查它（所属项目 %q，迁入 %d 个）：%+v", name, owner, r.Containers, r.Issues)
			}
		}
		if got := kinds(Evaluate(p, tg, &Decisions{}, Options{Existing: "fail"}), Stop); strings.Join(got, ",") != "existing_project" {
			t.Errorf("--existing fail 时应停下（所属项目 %q）：%v", owner, got)
		}
	}
}

// 已有的 nginx 属于新服务器上的 Compose 项目 proxy，迁移包里也有 proxy：它跟着 proxy 走（见 Counterparts）。
// 替换 proxy 时名字空出来，site 照常迁入；保留 proxy 时名字被占着，site 也不迁入（以前照样迁入，Compose
// 创建容器时名字冲突，整体回滚）。不迁入的 site 在新服务器上的 db 跟着保留下来，单独迁入的 db 名字被占着，
// 也不迁入。
func TestCounterpartsNameHeldByKeptProject(t *testing.T) {
	p := testPlan()
	p.Containers = append(p.Containers, bundle.PlanContainer{Name: "nginx", Project: "site", State: "running"},
		bundle.PlanContainer{Name: "proxy-app-1", Project: "proxy", State: "running"}, bundle.PlanContainer{Name: "db", State: "running"})
	tg := testTarget()
	tg.Containers["nginx"] = TargetContainer{State: "running", Project: "proxy"}
	tg.Containers["db"] = TargetContainer{State: "running", Project: "site"}
	r := Evaluate(p, tg, &Decisions{SkipProjects: []string{"proxy"}}, Options{})
	var infos []string
	for _, i := range r.Issues {
		if i.Kind == "existing_container" && i.Level == Info {
			infos = append(infos, i.Title)
		}
	}
	want := "新服务器上已有同名容器 nginx，它属于保留的 Compose 项目 proxy，不迁入 Compose 项目 site|" +
		"新服务器上已有同名容器 db，它属于保留的 Compose 项目 site，不迁入这个容器"
	if len(kinds(r, Ask)) != 0 || r.Containers != 2 || strings.Join(infos, "|") != want {
		t.Errorf("保留 proxy 时 site、db 都不迁入（迁入 %d 个）：%+v", r.Containers, r.Issues)
	}
	r = Evaluate(p, tg, &Decisions{ReplaceProjects: []string{"proxy", "site"}}, Options{})
	if len(r.Issues) != 0 || r.Containers != 6 {
		t.Errorf("替换 proxy、site 时名字都空出来（迁入 %d 个）：%+v", r.Containers, r.Issues)
	}
}

func TestHasContent(t *testing.T) {
	dir := t.TempDir()
	empty, full, file, link := filepath.Join(dir, "empty"), filepath.Join(dir, "full"), filepath.Join(dir, "file"), filepath.Join(dir, "link")
	os.Mkdir(empty, 0o755)
	os.Mkdir(full, 0o755)
	os.WriteFile(filepath.Join(full, "a"), nil, 0o644)
	os.WriteFile(file, nil, 0o644)
	os.Symlink(empty, link)
	for path, want := range map[string]bool{empty: false, full: true, file: true, link: true, filepath.Join(dir, "none"): false} {
		if got := hasContent(path); got != want {
			t.Errorf("hasContent(%s) = %v", filepath.Base(path), got)
		}
	}
}
