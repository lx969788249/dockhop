//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// 保真测试：用一组常见和不常见的写法创建容器，迁出 → 删除一切 → 迁入，然后把新容器的
// 完整配置（docker inspect 的 Config、HostConfig、网络、挂载、状态）和原来的逐字段比较，
// 除了下面明确列出的修改，必须完全一致。
func TestInFidelity(t *testing.T) {
	p := fmt.Sprintf("dmtest_fid%d", os.Getpid())
	dir := t.TempDir()
	octet := 100 + os.Getpid()%100
	run := filepath.Join("/run", p) // 系统路径：只重建挂载，不迁移内容
	t.Cleanup(func() {
		removePrefixed(p)
		removeProject(p + "_proj")
		removeProject(p + "solo")
		try("docker", "rmi", "-f", p+"_health:1")
		os.RemoveAll(run)
	})
	sh(t, "docker", "pull", "-q", "alpine:3.20")

	// ---- 准备旧服务器上的东西 ----
	sh(t, "docker", "network", "create", "--subnet", fmt.Sprintf("172.29.%d.0/24", octet), p+"_neta")
	sh(t, "docker", "network", "create", p+"_netb")
	sh(t, "docker", "volume", "create", "--label", "app=fid", p+"_vol")
	sh(t, "docker", "run", "--rm", "-v", p+"_vol:/v", "alpine:3.20", "sh", "-c", `
		echo volume-data >/v/data.txt && chown 1234:5678 /v/data.txt && chmod 640 /v/data.txt
		mkdir -p /v/sub && chown 999:999 /v/sub
		ln -s /etc/passwd /v/external && ln -s /nowhere /v/dangling && ln -s loop /v/loop
		mkfifo /v/fifo`)
	ro, rw := filepath.Join(dir, "ro"), filepath.Join(dir, "rw")
	os.MkdirAll(ro, 0o755)
	os.MkdirAll(filepath.Join(rw, "deep", "er"), 0o750)
	os.WriteFile(filepath.Join(ro, "config.txt"), []byte("ro-data"), 0o644)
	os.WriteFile(filepath.Join(rw, "deep", "er", "state.txt"), []byte("rw-data"), 0o600)
	os.Symlink("deep/er/state.txt", filepath.Join(rw, "rel-link"))
	single := filepath.Join(dir, "app.conf") // 单个文件挂载
	os.WriteFile(single, []byte("single-file"), 0o640)
	real := filepath.Join(dir, "real")
	os.MkdirAll(real, 0o755)
	os.WriteFile(filepath.Join(real, "r.txt"), []byte("through-symlink"), 0o644)
	os.Symlink(real, filepath.Join(dir, "linked"))
	os.MkdirAll(run, 0o755)
	localtime, _ := os.ReadFile("/etc/localtime")

	names := map[string]string{
		"full": p + "_full", "multi": p + "_multi", "health": p + "_health", "stopped": p + "_stopped",
		"paused": p + "_paused", "provider": p + "_provider", "sidecar": p + "_sidecar", "symlink": p + "_symlink", "changed": p + "_changed",
	}
	sh(t, "docker", "run", "-d", "-it", "--name", names["full"], "--label", noWatchtower,
		"--hostname", "fidelity-host", "--user", "1000:1000", "-w", "/tmp", "-e", "FOO=bar", "-e", "WITH_SPACE=a b c",
		"--label", "app.tier=web", "--label", "app.note=hello world",
		"-p", "127.0.0.1::80", "-p", fmt.Sprintf("127.0.0.1:%d:81/udp", nextPort()),
		"--restart", "unless-stopped", "--memory", "64m", "--cpus", "0.5", "--cpu-shares", "512", "--pids-limit", "100",
		"--cap-add", "NET_ADMIN", "--cap-drop", "MKNOD", "--ulimit", "nofile=1024:2048", "--init", "--stop-timeout", "7",
		"--stop-signal", "SIGINT", "--oom-score-adj", "100", "--read-only", "--tmpfs", "/scratch:rw,size=8m",
		"--add-host", "db.test:10.1.2.3", "--dns", "1.1.1.1", "--dns-search", "example.test",
		"--health-cmd", "true", "--health-interval", "2s",
		"-v", "/etc/localtime:/etc/localtime:ro", "-v", p+"_vol:/data", "-v", ro+":/cfg:ro", "-v", rw+":/rw",
		"-v", single+":/etc/app.conf:ro", "-v", run+":/run/host",
		"--entrypoint", "sh", "alpine:3.20", "-c", "sleep 600")
	sh(t, "docker", "run", "-d", "--name", names["multi"], "--label", noWatchtower, "--network", p+"_neta",
		"--ip", fmt.Sprintf("172.29.%d.10", octet), "--network-alias", "svc-a", "alpine:3.20", "sleep", "600")
	sh(t, "docker", "network", "connect", "--alias", "svc-b", p+"_netb", names["multi"])
	// exec 形式的健康检查只能写在镜像里，迁移后不能被改写成 shell 形式。
	build := exec.Command("docker", "build", "-q", "-t", p+"_health:1", "-")
	build.Stdin = strings.NewReader("FROM alpine:3.20\nHEALTHCHECK --interval=2s --timeout=2s --retries=3 CMD [\"/bin/true\"]\n")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	sh(t, "docker", "run", "-d", "--name", names["health"], "--label", noWatchtower, p+"_health:1", "sleep", "600")
	sh(t, "docker", "create", "--name", names["stopped"], "--label", noWatchtower, "-e", "STOPPED=1", "alpine:3.20", "sleep", "600")
	sh(t, "docker", "run", "-d", "--name", names["paused"], "--label", noWatchtower, "alpine:3.20", "sleep", "600")
	sh(t, "docker", "pause", names["paused"])
	sh(t, "docker", "run", "-d", "--name", names["provider"], "--label", noWatchtower, "alpine:3.20", "sleep", "600")
	sh(t, "docker", "run", "-d", "--name", names["sidecar"], "--label", noWatchtower, "--network", "container:"+names["provider"], "alpine:3.20", "sleep", "600")
	sh(t, "docker", "run", "-d", "--name", names["symlink"], "--label", noWatchtower, "-v", filepath.Join(dir, "linked")+":/data", "alpine:3.20", "sleep", "600")
	sh(t, "docker", "run", "-d", "--name", names["changed"], "--label", noWatchtower, "alpine:3.20", "sleep", "600")
	exec1(t, names["changed"], "echo added >/etc/added.conf && mkdir -p /opt/tool/bin && echo t >/opt/tool/bin/x && echo changed >/etc/motd && rm /etc/issue")

	// Compose：用 -p 指定和目录名不同的项目名；命名卷和匿名卷。
	proj := filepath.Join(dir, "compose-dir")
	os.MkdirAll(proj, 0o755)
	os.WriteFile(filepath.Join(proj, "compose.yml"), []byte(`services:
  app:
    image: alpine:3.20
    command: ["sleep", "600"]
    labels: {`+noWatchtower[:strings.Index(noWatchtower, "=")]+`: "false"}
    volumes: ["appdata:/data", "/anon"]
    healthcheck: {test: ["CMD", "true"], interval: 2s}
  side:
    image: alpine:3.20
    command: ["sleep", "600"]
    labels: {`+noWatchtower[:strings.Index(noWatchtower, "=")]+`: "false"}
volumes:
  appdata: {}
`), 0o644)
	c := composeCmd(t)
	sh(t, c[0], append(c[1:], "-p", p+"_proj", "--project-directory", proj, "-f", filepath.Join(proj, "compose.yml"), "up", "-d")...)
	app := containerByLabel(t, p+"_proj", "app")
	side := containerByLabel(t, p+"_proj", "side")
	exec1(t, app, "echo anon-data >/anon/a.txt && echo named-data >/data/n.txt")
	// 单服务 Compose 项目，目录名就是项目名：迁移后在原目录直接 docker compose up。
	solo := filepath.Join(dir, p+"solo")
	os.MkdirAll(solo, 0o755)
	os.WriteFile(filepath.Join(solo, "compose.yml"), []byte(`services:
  web:
    image: alpine:3.20
    command: ["sleep", "600"]
    labels: {`+noWatchtower[:strings.Index(noWatchtower, "=")]+`: "false"}
    env_file: [web.env]
`), 0o644)
	os.WriteFile(filepath.Join(solo, "web.env"), []byte("SOLO=1\n"), 0o644)
	upSolo := exec.Command(c[0], append(c[1:], "up", "-d")...)
	upSolo.Dir = solo
	if b, err := upSolo.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	soloName := containerByLabel(t, p+"solo", "web")

	all := []string{}
	for _, n := range names {
		all = append(all, n)
	}
	all = append(all, app, side, soloName)
	waitHealthy(t, names["health"], names["full"], app)
	before := map[string]map[string]any{}
	for _, n := range all {
		before[n] = normalize(t, inspect(t, n))
	}

	// ---- 迁出，然后删掉一切，相当于一台新服务器 ----
	m := startOut(t, "--include", strings.Join(all, ","))
	removeAll(all...)
	removeProject(p + "_proj")
	removeProject(p + "solo")
	for _, v := range []string{p + "_vol"} {
		try("docker", "volume", "rm", v)
	}
	try("docker", "network", "rm", p+"_neta", p+"_netb")
	try("docker", "rmi", "-f", p+"_health:1")
	os.RemoveAll(dir)
	os.MkdirAll(dir, 0o700)

	home := t.TempDir()
	out, code := m.in(t, home, nil)
	if code != 0 {
		t.Fatalf("迁入应成功，退出码 %d", code)
	}
	mustContain(t, out, "[8/8] 完成迁入 ✓", "✅ 迁入完成", "里没挂载出来的改动已一起迁入")
	m.waitOutSuccess(t)
	noLeftovers(t, home)
	if os.Getenv("DOCKHOP_TEST_TIMING") != "" {
		for _, l := range strings.Split(readLogs(home), "\n") {
			if strings.Contains(l, "POST") || strings.Contains(l, "=====") || strings.Contains(l, "$ ") || strings.Contains(l, "健康检查通过") {
				t.Log(l)
			}
		}
	}

	// ---- 完整配置比较 ----
	waitHealthy(t, names["health"], names["full"], containerByLabel(t, p+"_proj", "app"))
	for _, n := range all {
		after := normalize(t, inspect(t, n))
		want := before[n]
		if n == names["changed"] {
			// 容器里删除过文件：在新服务器上做出带改动的镜像，镜像名是 dockhop-image:容器名。
			if img := after["Config"].(map[string]any)["Image"]; img != "dockhop-image:"+n {
				t.Errorf("%s 的镜像应改为带改动的镜像：%v", n, img)
			}
			after["Config"].(map[string]any)["Image"] = want["Config"].(map[string]any)["Image"]
		}
		if diff := diffMaps(want, after, ""); len(diff) > 0 {
			t.Errorf("容器 %s 迁移前后不一致：\n%s", n, strings.Join(diff, "\n"))
		}
	}

	// ---- 数据 ----
	full := names["full"]
	got := exec1(t, full, "cat /data/data.txt /cfg/config.txt /rw/deep/er/state.txt /etc/app.conf; echo; readlink /rw/rel-link")
	if got != "volume-data\nro-datarw-datasingle-file\ndeep/er/state.txt" {
		t.Errorf("full 的数据不对：%q", got)
	}
	vol := volumeFiles(t, p+"_vol")
	for _, want := range []string{"./data.txt 1234:5678 640 volume-data", "./sub/ 999:999", "./external -> /etc/passwd", "./dangling -> /nowhere", "./loop -> loop"} {
		if !strings.Contains(vol, want) {
			t.Errorf("卷里缺少「%s」：\n%s", want, vol)
		}
	}
	if fi, err := os.Stat(filepath.Join(rw, "deep", "er")); err != nil || fi.Mode().Perm() != 0o750 {
		t.Errorf("目录权限没有保留：%v %v", fi, err)
	}
	if now, _ := os.ReadFile("/etc/localtime"); string(now) != string(localtime) {
		t.Error("宿主机的 /etc/localtime 不应被改动")
	}
	if l, _ := os.Readlink(filepath.Join(dir, "linked")); l != real {
		t.Errorf("软链接目录应在原路径重建：%q", l)
	}
	if got := exec1(t, names["symlink"], "cat /data/r.txt"); got != "through-symlink" {
		t.Errorf("软链接目录的数据：%q", got)
	}
	if got := exec1(t, names["changed"], "cat /etc/added.conf /opt/tool/bin/x /etc/motd; test -e /etc/issue && echo issue-still-there || true"); got != "added\nt\nchanged" {
		t.Errorf("容器里的改动：%q", got)
	}
	app = containerByLabel(t, p+"_proj", "app")
	if got := exec1(t, app, "cat /anon/a.txt /data/n.txt"); got != "anon-data\nnamed-data" {
		t.Errorf("Compose 的匿名卷和命名卷：%q", got)
	}
	if s := state(t, names["stopped"]); s != "created" && s != "exited" {
		t.Errorf("原来停着的容器只创建不启动：%s", s)
	}
	if s := state(t, names["paused"]); s != "paused" {
		t.Errorf("原来暂停的容器：%s", s)
	}

	// ---- 迁移后在原目录 docker compose up：不应重建容器 ----
	soloID := sh(t, "docker", "inspect", "-f", "{{.Id}}", soloName)
	up := exec.Command(c[0], append(c[1:], "up", "-d")...)
	up.Dir = solo
	if b, err := up.CombinedOutput(); err != nil {
		t.Fatalf("迁移后在原目录 docker compose up 失败：%v\n%s", err, b)
	}
	if id := sh(t, "docker", "inspect", "-f", "{{.Id}}", soloName); id != soloID {
		t.Error("迁移后 docker compose up 不应重建容器（配置应和文件一致）")
	}
	if n := len(strings.Fields(try("docker", "ps", "-aq", "--filter", "label=com.docker.compose.project="+p+"solo"))); n != 1 {
		t.Errorf("不应多出别的容器：%d 个", n)
	}
}

func waitHealthy(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		for i := 0; i < 60; i++ {
			if try("docker", "inspect", "-f", "{{.State.Health.Status}}", n) == "healthy" {
				break
			}
			time.Sleep(time.Second)
		}
	}
}

// normalize 去掉每次创建都会变的字段（ID、时间、动态分配的地址等），其余原样保留用于比较。
func normalize(t *testing.T, c map[string]any) map[string]any {
	t.Helper()
	id, _ := c["Id"].(string)
	name := strings.TrimPrefix(c["Name"].(string), "/")
	st := c["State"].(map[string]any)
	state := st["Status"]
	if state == "exited" || state == "created" {
		state = "stopped"
	}
	health := ""
	if h, ok := st["Health"].(map[string]any); ok {
		health, _ = h["Status"].(string)
	}
	cfg := c["Config"].(map[string]any)
	hc := c["HostConfig"].(map[string]any)
	// 自动起的主机名；借用别的容器网络时，主机名就是被借用容器的（它也有新 ID）。
	if mode, _ := hc["NetworkMode"].(string); cfg["Hostname"] == id[:12] || strings.HasPrefix(mode, "container:") {
		delete(cfg, "Hostname")
	}
	for _, k := range []string{"NetworkMode", "PidMode", "IpcMode"} {
		if v, _ := hc[k].(string); strings.HasPrefix(v, "container:") {
			hc[k] = "container:" + strings.TrimPrefix(try("docker", "inspect", "-f", "{{.Name}}", strings.TrimPrefix(v, "container:")), "/")
		}
	}
	// 端口：随机分配的宿主机端口每次不同。
	nets := map[string]any{}
	if ns, ok := c["NetworkSettings"].(map[string]any)["Networks"].(map[string]any); ok {
		for n, v := range ns {
			ep := v.(map[string]any)
			var aliases []string
			if list, ok := ep["Aliases"].([]any); ok {
				for _, a := range list {
					if s := a.(string); s != id[:12] && s != name {
						aliases = append(aliases, s)
					}
				}
			}
			sort.Strings(aliases)
			nets[n] = map[string]any{"IPAMConfig": ep["IPAMConfig"], "Aliases": aliases, "DriverOpts": ep["DriverOpts"], "Links": ep["Links"]}
		}
	}
	var mounts []string
	if list, ok := c["Mounts"].([]any); ok {
		for _, m := range list {
			mm := m.(map[string]any)
			src := fmt.Sprint(mm["Source"])
			if mm["Type"] == "volume" {
				src = fmt.Sprint(mm["Name"])
				if len(src) == 64 {
					src = "<匿名卷>"
				}
			}
			mounts = append(mounts, fmt.Sprintf("%v %s→%v %v %v %v", mm["Type"], src, mm["Destination"], mm["Mode"], mm["RW"], mm["Propagation"]))
		}
	}
	sort.Strings(mounts)
	return map[string]any{
		"State": map[string]any{"Status": state, "Health": health, "Paused": st["Paused"]},
		"Name":  name, "Config": cfg, "HostConfig": hc, "Networks": nets, "Mounts": mounts,
		"Path": c["Path"], "Args": c["Args"],
	}
}

// diffMaps 逐字段比较，返回不一样的地方。随机端口的宿主机端口不比较。
func diffMaps(a, b any, path string) []string {
	if strings.HasSuffix(path, ".HostPort") {
		return nil
	}
	ma, okA := a.(map[string]any)
	mb, okB := b.(map[string]any)
	if okA && okB {
		keys := map[string]bool{}
		for k := range ma {
			keys[k] = true
		}
		for k := range mb {
			keys[k] = true
		}
		var out []string
		var sorted []string
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			out = append(out, diffMaps(ma[k], mb[k], path+"."+k)...)
		}
		return out
	}
	sa, okA := a.([]any)
	sb, okB := b.([]any)
	if okA && okB && len(sa) == len(sb) {
		var out []string
		for i := range sa {
			out = append(out, diffMaps(sa[i], sb[i], fmt.Sprintf("%s[%d]", path, i))...)
		}
		return out
	}
	if reflect.DeepEqual(a, b) || (isEmpty(a) && isEmpty(b)) {
		return nil
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return []string{fmt.Sprintf("  %s：原来 %s，现在 %s", path, ja, jb)}
}

// isEmpty：null、空列表、空对象、false、0、"" 视为相同（Docker 不同写法表示“没有设置”）。
func isEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case []any:
		return len(x) == 0
	case []string:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	case bool:
		return !x
	case float64:
		return x == 0
	case string:
		return x == ""
	}
	return false
}
