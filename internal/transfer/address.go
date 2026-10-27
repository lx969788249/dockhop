package transfer

import (
	"context"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Address 是链接里使用的地址，以及怎么得到的（用于提示用户）。
type Address struct {
	Host string
	Note string // 不为空时需要提醒用户，例如可能是内网地址
}

// DetectAddress 探测本机对外的地址：
//  1. 默认路由使用的本机地址，是公网地址就用它（不发出任何数据）；
//  2. 否则问几个公共的“我的 IP”服务（NAT 后面的云服务器常见）；
//  3. 纯 IPv6 服务器用 IPv6 地址；
//  4. 都不行时用网卡上的第一个地址，并提醒用户可能是内网地址。
func DetectAddress(ctx context.Context) Address {
	if ip := routeAddr("udp4", "1.1.1.1:80"); ip != nil && isPublic(ip) {
		return Address{Host: ip.String()}
	}
	if ip := externalIPv4(ctx); ip != nil {
		return Address{Host: ip.String()}
	}
	if ip := routeAddr("udp6", "[2606:4700:4700::1111]:80"); ip != nil && isPublic(ip) {
		return Address{Host: ip.String()}
	}
	if ip := routeAddr("udp4", "1.1.1.1:80"); ip != nil {
		return Address{Host: ip.String(), Note: "探测到的是内网地址，新服务器不在同一个内网时连不上；请用 --host 公网IP或域名 重新运行"}
	}
	return Address{Host: "127.0.0.1", Note: "无法探测本机对外的地址，新服务器连不上这个链接；请用 --host 公网IP或域名 重新运行"}
}

// routeAddr 返回访问 target 时本机使用的地址。UDP“连接”只查路由，不会发出数据。
func routeAddr(network, target string) net.IP {
	c, err := net.Dial(network, target)
	if err != nil {
		return nil
	}
	defer c.Close()
	if a, ok := c.LocalAddr().(*net.UDPAddr); ok {
		return a.IP
	}
	return nil
}

func isPublic(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	// 100.64.0.0/10 是运营商级 NAT 的地址
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return false
	}
	return ip.IsGlobalUnicast()
}

func externalIPv4(ctx context.Context) net.IP {
	client := &http.Client{Timeout: 3 * time.Second}
	for _, u := range []string{"https://api.ipify.org", "https://ipv4.icanhazip.com", "https://ifconfig.me/ip"} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
		resp.Body.Close()
		if ip := net.ParseIP(strings.TrimSpace(string(b))); ip != nil && ip.To4() != nil && isPublic(ip) {
			return ip
		}
	}
	return nil
}

// FirewallHint 返回放行端口的提示：检测到 ufw 或 firewalld 在运行时给出对应命令。
// 只给提示，不擅自修改防火墙。
func FirewallHint(ctx context.Context, port int) string {
	hint := "新服务器连不上时：在云服务商控制台的安全组（防火墙）里放行 TCP " + strconv.Itoa(port) + " 端口"
	run := func(name string, args ...string) string {
		if _, err := exec.LookPath(name); err != nil {
			return ""
		}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		out, _ := exec.CommandContext(cctx, name, args...).Output()
		return string(out)
	}
	switch {
	case strings.Contains(run("ufw", "status"), "Status: active"):
		hint += "，并在这台服务器执行 ufw allow " + strconv.Itoa(port) + "/tcp"
	case strings.TrimSpace(run("firewall-cmd", "--state")) == "running":
		hint += "，并在这台服务器执行 firewall-cmd --add-port=" + strconv.Itoa(port) + "/tcp"
	}
	return hint
}
