package spec

import (
	"encoding/json"
	"reflect"
	"testing"
)

const sourceRaw = `{
  "Id": "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
  "Name": "/web",
  "Config": {
    "Hostname": "abcdef012345", "Image": "nginx:1.25", "Tty": true, "OpenStdin": true, "StopTimeout": 30,
    "Env": ["PATH=/usr/bin", "NGINX_VERSION=1.25", "MODE=prod"],
    "Entrypoint": ["/docker-entrypoint.sh"], "Cmd": ["nginx", "-g", "daemon off;"],
    "Labels": {"maintainer": "nginx", "app": "web"},
    "ExposedPorts": {"80/tcp": {}, "9000/tcp": {}},
    "FutureField": {"keep": true}
  },
  "HostConfig": {
    "NetworkMode": "appnet", "IpcMode": "private", "CgroupnsMode": "private", "Runtime": "runc",
    "MaskedPaths": ["/proc/kcore"], "ReadonlyPaths": ["/proc/sys"], "ConsoleSize": [0, 0],
    "LogConfig": {"Type": "json-file", "Config": {}},
    "PortBindings": {"80/tcp": [{"HostIp": "10.0.0.5", "HostPort": "8080"}], "9000/tcp": [{"HostIp": "", "HostPort": ""}]},
    "Init": true, "PidsLimit": 100, "Sysctls": {"net.core.somaxconn": "1024"}
  },
  "NetworkSettings": {"Networks": {
    "appnet": {"IPAMConfig": {"IPv4Address": "172.20.0.10"}, "Aliases": ["abcdef012345", "web-alias"], "IPAddress": "172.20.0.10", "MacAddress": "02:42:ac:14:00:0a"},
    "backend": {"IPAMConfig": null, "Aliases": ["db-client"], "IPAddress": "172.21.0.3"}
  }}
}`

const imageRaw = `{"Config": {"Env": ["PATH=/usr/bin", "NGINX_VERSION=1.25"], "Entrypoint": ["/docker-entrypoint.sh"],
  "Cmd": ["nginx", "-g", "daemon off;"], "Labels": {"maintainer": "nginx"}, "ExposedPorts": {"80/tcp": {}}}}`

func TestRebuild(t *testing.T) {
	b, err := Rebuild{
		Raw: json.RawMessage(sourceRaw), ImageConfig: json.RawMessage(imageRaw),
		Source: SourceDefaults{CgroupVersion: "2", LoggingDriver: "json-file", DefaultRuntime: "runc"},
		Port: func(ip, port, cport, proto string) (string, string, bool) {
			if port == "8080" {
				return "", "18080", true // 改绑到所有地址并换端口
			}
			return ip, port, true
		},
	}.Build()
	if err != nil {
		t.Fatal(err)
	}
	if b.Name != "web" {
		t.Errorf("名字：%s", b.Name)
	}
	body := b.Body
	for _, k := range []string{"Hostname", "Entrypoint", "Cmd"} {
		if _, ok := body[k]; ok {
			t.Errorf("%s 应去掉（自动主机名 / 和镜像相同）", k)
		}
	}
	if !reflect.DeepEqual(body["Env"], []any{"MODE=prod"}) {
		t.Errorf("环境变量只应保留容器自己设置的：%v", body["Env"])
	}
	if !reflect.DeepEqual(body["Labels"], map[string]any{"app": "web"}) {
		t.Errorf("标签：%v", body["Labels"])
	}
	if !reflect.DeepEqual(body["ExposedPorts"], map[string]any{"9000/tcp": map[string]any{}}) {
		t.Errorf("端口声明：%v", body["ExposedPorts"])
	}
	if body["Tty"] != true || body["StopTimeout"] != float64(30) || body["FutureField"] == nil {
		t.Errorf("其他字段应原样保留：%v", body)
	}
	hc := body["HostConfig"].(map[string]any)
	for _, k := range []string{"MaskedPaths", "ReadonlyPaths", "ConsoleSize", "IpcMode", "CgroupnsMode", "Runtime", "LogConfig"} {
		if _, ok := hc[k]; ok {
			t.Errorf("Docker 自动填的默认值 %s 应去掉", k)
		}
	}
	if hc["Init"] != true || hc["PidsLimit"] != float64(100) {
		t.Errorf("用户设置应保留：%v", hc)
	}
	pb := hc["PortBindings"].(map[string]any)
	if got := pb["80/tcp"].([]any)[0].(map[string]any); got["HostIp"] != "" || got["HostPort"] != "18080" {
		t.Errorf("端口调整：%v", got)
	}
	if got := pb["9000/tcp"].([]any)[0].(map[string]any); got["HostPort"] != "" {
		t.Errorf("随机端口应原样保留：%v", got)
	}
	// 老版本 Docker：主网络在创建时连，其他网络创建后再连。
	eps := body["NetworkingConfig"].(map[string]any)["EndpointsConfig"].(map[string]any)
	app := eps["appnet"].(map[string]any)
	if !reflect.DeepEqual(app["Aliases"], []any{"web-alias"}) {
		t.Errorf("别名里应去掉旧容器 ID：%v", app["Aliases"])
	}
	if app["IPAMConfig"].(map[string]any)["IPv4Address"] != "172.20.0.10" {
		t.Errorf("用户指定的固定 IP 应保留：%v", app)
	}
	if _, ok := app["MacAddress"]; ok {
		t.Error("动态分配的 MAC 不应固定")
	}
	if _, ok := eps["backend"]; ok || len(b.Connects) != 1 || b.Connects[0].Network != "backend" {
		t.Errorf("老版本应创建后再连 backend：%v %v", eps, b.Connects)
	}
}

func TestRebuildMultiNetworkAndAutoIP(t *testing.T) {
	b, err := Rebuild{Raw: json.RawMessage(sourceRaw), MultiNetwork: true, AutoIP: func(n string) bool { return n == "appnet" }}.Build()
	if err != nil {
		t.Fatal(err)
	}
	eps := b.Body["NetworkingConfig"].(map[string]any)["EndpointsConfig"].(map[string]any)
	if len(eps) != 2 || len(b.Connects) != 0 {
		t.Errorf("新版本创建时就连两个网络：%v", eps)
	}
	if _, ok := eps["appnet"].(map[string]any)["IPAMConfig"]; ok {
		t.Error("改为自动分配 IP 时不应带固定 IP")
	}
	// 没有镜像信息时不去掉任何东西。
	if b.Body["Entrypoint"] == nil || len(b.Body["Env"].([]any)) != 3 {
		t.Errorf("没有镜像信息时应全部保留：%v", b.Body)
	}
}

func TestRebuildContainerMode(t *testing.T) {
	raw := `{"Id": "1111", "Name": "/side", "Config": {"Hostname": "custom", "Domainname": "x", "Image": "alpine"},
	  "HostConfig": {"NetworkMode": "container:abcdef0123456789", "PidMode": "container:abcdef0123456789",
	    "PortBindings": {"80/tcp": [{"HostPort": "80"}]}},
	  "NetworkSettings": {"Networks": {}}}`
	b, err := Rebuild{Raw: json.RawMessage(raw), NameOf: func(ref string) string {
		if ref == "abcdef0123456789" {
			return "web"
		}
		return ""
	}}.Build()
	if err != nil {
		t.Fatal(err)
	}
	hc := b.Body["HostConfig"].(map[string]any)
	if hc["NetworkMode"] != "container:web" || hc["PidMode"] != "container:web" {
		t.Errorf("依附的容器应改成名字：%v", hc)
	}
	if _, ok := hc["PortBindings"]; ok {
		t.Error("借用别的容器网络时不能发布端口")
	}
	if _, ok := b.Body["Hostname"]; ok {
		t.Error("借用网络时主机名来自被借用的容器，应去掉")
	}
	if _, ok := b.Body["NetworkingConfig"]; ok {
		t.Error("借用网络时不应再连网络")
	}
}

func TestRebuildKeepVolumes(t *testing.T) {
	raw := `{"Id": "2222", "Name": "/db", "Config": {"Image": "postgres"}, "HostConfig": {"NetworkMode": "bridge"},
	  "NetworkSettings": {"Networks": {"bridge": {}}}}`
	b, _ := Rebuild{Raw: json.RawMessage(raw), KeepVolumes: map[string]string{"/var/lib/postgresql/data": "aaaa"}}.Build()
	mounts := b.Body["HostConfig"].(map[string]any)["Mounts"].([]any)
	if len(mounts) != 1 || mounts[0].(map[string]any)["Source"] != "aaaa" {
		t.Errorf("回滚重建时应沿用原来的匿名卷：%v", mounts)
	}
}
