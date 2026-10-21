package spec

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// 容器重建：把源容器的原始配置（docker inspect）直接交给新服务器的 Docker 创建容器，
// 只做下面这些明确的修改，其余字段一律原样保留（包括 DockHop 不认识的新字段）：
//
//   - 去掉 Docker 自动起的主机名（等于容器 ID 前 12 位）；借用别的网络时去掉主机名和域名；
//   - 去掉旧服务器 Docker 按版本自动填的默认值：MaskedPaths、ReadonlyPaths、ConsoleSize、
//     默认的 CgroupnsMode / IpcMode / Runtime / Isolation / 日志设置，让新服务器按自己的默认值来；
//   - 去掉和镜像相同的入口命令、命令、环境变量、标签等（结果完全一样，只是不把镜像的
//     默认值固定在容器上，以后换新版镜像时能用上新版的默认值）；
//   - container:<ID> 改为 container:<名字>（依附的容器在新服务器上有新 ID）；
//   - 镜像改为快照镜像或指定的镜像；按预检的决定改端口、不再对外开放、改绑到所有地址、
//     改为自动分配 IP；
//   - 只保留用户指定的固定 IP，动态分配的 IP 和 MAC 地址不固定；别名里去掉旧容器的 ID；
//   - Docker 25 以下不支持创建时连接多个网络：先连主网络，其余的创建后再连。

// SourceDefaults 是旧服务器 Docker 的默认值，用来判断哪些设置是自动填的。
type SourceDefaults struct {
	CgroupVersion  string // "1" 或 "2"
	LoggingDriver  string
	DefaultRuntime string
}

// Rebuild 是重建一个容器需要的全部信息。
type Rebuild struct {
	Raw   json.RawMessage // 源容器的 docker inspect
	Image string          // 用哪个镜像创建；空表示用原来的 Config.Image
	// ImageConfig 是这个镜像的 docker inspect，用来去掉和镜像相同的设置；为空时全部保留。
	ImageConfig json.RawMessage
	Source      SourceDefaults
	// NameOf 把 container:<ID> 里的 ID 换成容器名；找不到时返回空。
	NameOf func(ref string) string
	// Port 按预检的决定调整一个端口映射：返回新的宿主机地址和端口，publish 为 false 表示不再对外开放。
	Port func(hostIP, hostPort, containerPort, proto string) (newIP, newPort string, publish bool)
	// AutoIP 表示这个网络里改为自动分配 IP。
	AutoIP func(network string) bool
	// MultiNetwork 表示 Docker 支持创建时连接多个网络（接口版本 1.44 及以上）。
	MultiNetwork bool
	// KeepVolumes 把匿名卷固定为指定的卷（挂载位置 → 卷名），回滚重建旧容器时用。
	KeepVolumes map[string]string
}

// Connect 是创建之后还要连接的网络。
type Connect struct {
	Network  string
	Endpoint map[string]any
}

// Built 是重建的结果：容器名、创建请求、创建后还要连接的网络。
type Built struct {
	Name     string
	Body     map[string]any
	Connects []Connect
}

// Build 生成创建容器的请求。
func (r Rebuild) Build() (*Built, error) {
	var top map[string]any
	if err := json.Unmarshal(r.Raw, &top); err != nil {
		return nil, fmt.Errorf("容器配置读不懂：%v", err)
	}
	cfg, _ := top["Config"].(map[string]any)
	hc, _ := top["HostConfig"].(map[string]any)
	if cfg == nil || hc == nil {
		return nil, fmt.Errorf("容器配置不完整")
	}
	id, _ := top["Id"].(string)
	name := strings.TrimPrefix(str(top["Name"]), "/")
	short := id
	if len(short) > 12 {
		short = short[:12]
	}

	netMode := str(hc["NetworkMode"])
	for _, key := range []string{"NetworkMode", "PidMode", "IpcMode"} {
		if ref, ok := strings.CutPrefix(str(hc[key]), "container:"); ok && r.NameOf != nil {
			if n := r.NameOf(ref); n != "" {
				hc[key] = "container:" + n
			}
		}
	}
	sharedNet := netMode == "host" || strings.HasPrefix(netMode, "container:")

	// ---- Config ----
	if h := str(cfg["Hostname"]); h == "" || h == short || sharedNet {
		delete(cfg, "Hostname")
	}
	if sharedNet {
		delete(cfg, "Domainname")
	}
	if r.Image != "" {
		cfg["Image"] = r.Image
	}
	if len(r.ImageConfig) > 0 {
		var img struct {
			Config map[string]any `json:"Config"`
		}
		if json.Unmarshal(r.ImageConfig, &img) == nil && img.Config != nil {
			dropInherited(cfg, img.Config)
		}
	}

	// ---- HostConfig ----
	for _, k := range []string{"MaskedPaths", "ReadonlyPaths", "ConsoleSize", "ContainerIDFile"} {
		delete(hc, k)
	}
	defaultCgroupns := "host"
	if r.Source.CgroupVersion == "2" {
		defaultCgroupns = "private"
	}
	if v := str(hc["CgroupnsMode"]); v == "" || v == defaultCgroupns {
		delete(hc, "CgroupnsMode")
	}
	if v := str(hc["IpcMode"]); v == "" || v == "private" {
		delete(hc, "IpcMode")
	}
	if v := str(hc["Runtime"]); v == "" || v == "runc" || v == r.Source.DefaultRuntime {
		delete(hc, "Runtime")
	}
	if v := str(hc["Isolation"]); v == "" || v == "default" {
		delete(hc, "Isolation")
	}
	if lc, ok := hc["LogConfig"].(map[string]any); ok {
		opts, _ := lc["Config"].(map[string]any)
		if t := str(lc["Type"]); (t == "" || t == r.Source.LoggingDriver) && len(opts) == 0 {
			delete(hc, "LogConfig")
		}
	}
	if sharedNet {
		delete(hc, "PortBindings")
		delete(hc, "PublishAllPorts")
	} else if pb, ok := hc["PortBindings"].(map[string]any); ok && r.Port != nil {
		hc["PortBindings"] = adjustPorts(pb, r.Port)
	}
	if len(r.KeepVolumes) > 0 {
		mounts, _ := hc["Mounts"].([]any)
		dests := make([]string, 0, len(r.KeepVolumes))
		for d := range r.KeepVolumes {
			dests = append(dests, d)
		}
		sort.Strings(dests)
		for _, d := range dests {
			mounts = append(mounts, map[string]any{"Type": "volume", "Source": r.KeepVolumes[d], "Target": d})
		}
		hc["Mounts"] = mounts
	}

	// ---- 网络 ----
	body := cfg
	body["HostConfig"] = hc
	built := &Built{Name: name, Body: body}
	if sharedNet || netMode == "none" {
		return built, nil
	}
	ns, _ := top["NetworkSettings"].(map[string]any)
	nets, _ := ns["Networks"].(map[string]any)
	primary := netMode
	if primary == "" || primary == "default" {
		primary = "bridge"
	}
	if _, ok := nets[primary]; !ok {
		// NetworkMode 可能写的是网络 ID：按 ID 找到对应的网络名。
		for n, ep := range nets {
			if nid := str(asMap(ep)["NetworkID"]); nid != "" && strings.HasPrefix(nid, primary) {
				primary = n
				hc["NetworkMode"] = n
				break
			}
		}
	}
	names := make([]string, 0, len(nets))
	for n := range nets {
		names = append(names, n)
	}
	sort.Strings(names)
	endpoints := map[string]any{}
	for _, n := range names {
		ep := endpoint(asMap(nets[n]), short, r.AutoIP != nil && r.AutoIP(n))
		if n == primary || r.MultiNetwork {
			endpoints[n] = ep
		} else {
			built.Connects = append(built.Connects, Connect{Network: n, Endpoint: ep})
		}
	}
	if len(endpoints) > 0 {
		body["NetworkingConfig"] = map[string]any{"EndpointsConfig": endpoints}
	}
	return built, nil
}

// endpoint 从 inspect 里的网络设置生成创建时用的设置：只保留用户指定的部分。
func endpoint(src map[string]any, shortID string, autoIP bool) map[string]any {
	ep := map[string]any{}
	if ipam := asMap(src["IPAMConfig"]); len(ipam) > 0 && !autoIP {
		ep["IPAMConfig"] = ipam
	}
	var aliases []any
	for _, a := range asSlice(src["Aliases"]) {
		if s, _ := a.(string); s != "" && s != shortID {
			aliases = append(aliases, s)
		}
	}
	if len(aliases) > 0 {
		ep["Aliases"] = aliases
	}
	for _, k := range []string{"Links", "DriverOpts", "GwPriority"} {
		if v, ok := src[k]; ok && v != nil {
			ep[k] = v
		}
	}
	return ep
}

func adjustPorts(pb map[string]any, rule func(hostIP, hostPort, containerPort, proto string) (string, string, bool)) map[string]any {
	out := map[string]any{}
	for key, v := range pb {
		cport, proto, _ := strings.Cut(key, "/")
		if proto == "" {
			proto = "tcp"
		}
		var kept []any
		for _, b := range asSlice(v) {
			m := asMap(b)
			ip, port := str(m["HostIp"]), str(m["HostPort"])
			if port == "" { // Docker 随机分配的端口，不需要调整
				kept = append(kept, m)
				continue
			}
			nip, nport, publish := rule(ip, port, cport, proto)
			if !publish {
				continue
			}
			kept = append(kept, map[string]any{"HostIp": nip, "HostPort": nport})
		}
		if len(kept) > 0 {
			out[key] = kept
		}
	}
	return out
}

// dropInherited 去掉和镜像相同的设置：Docker 创建容器时会自动用镜像的值补上，
// 结果完全一样。
func dropInherited(cfg, img map[string]any) {
	if reflect.DeepEqual(cfg["Entrypoint"], img["Entrypoint"]) {
		delete(cfg, "Entrypoint")
		// 入口命令没改时，命令和镜像相同才能去掉（改了入口命令，Docker 不会再用镜像的命令）。
		if reflect.DeepEqual(cfg["Cmd"], img["Cmd"]) {
			delete(cfg, "Cmd")
		}
	}
	for _, k := range []string{"WorkingDir", "User", "StopSignal", "Shell"} {
		if v, ok := cfg[k]; ok && reflect.DeepEqual(v, img[k]) {
			delete(cfg, k)
		}
	}
	if hcheck, ok := cfg["Healthcheck"]; ok && reflect.DeepEqual(hcheck, img["Healthcheck"]) {
		delete(cfg, "Healthcheck")
	}
	delete(cfg, "OnBuild")
	if env := asSlice(cfg["Env"]); len(env) > 0 {
		inherited := map[string]bool{}
		for _, e := range asSlice(img["Env"]) {
			if s, ok := e.(string); ok {
				inherited[s] = true
			}
		}
		var keep []any
		for _, e := range env {
			if s, _ := e.(string); !inherited[s] {
				keep = append(keep, e)
			}
		}
		cfg["Env"] = keep
	}
	for _, k := range []string{"Labels", "ExposedPorts", "Volumes"} {
		own, ok := cfg[k].(map[string]any)
		if !ok {
			continue
		}
		base := asMap(img[k])
		keep := map[string]any{}
		for key, v := range own {
			if bv, ok := base[key]; ok && reflect.DeepEqual(bv, v) {
				continue
			}
			keep[key] = v
		}
		cfg[k] = keep
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}
