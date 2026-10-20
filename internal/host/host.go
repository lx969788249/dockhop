// Package host 读取本机信息：系统、磁盘空间、正在监听的端口、权限。
// 只读系统文件和系统调用，不依赖 ss、df 等外部命令。
package host

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// OSName 返回发行版名称，例如“Debian GNU/Linux 12 (bookworm)”。
func OSName() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return runtime.GOOS
	}
	name := ""
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"'`)
		switch k {
		case "PRETTY_NAME":
			return v
		case "NAME":
			name = v
		}
	}
	if name == "" {
		return runtime.GOOS
	}
	return name
}

// Kernel 返回内核版本和 CPU 架构，例如“Linux 6.1.0-18-amd64 x86_64”。
func Kernel() string {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return runtime.GOOS + " " + runtime.GOARCH
	}
	return fmt.Sprintf("%s %s %s", unix.ByteSliceToString(u.Sysname[:]),
		unix.ByteSliceToString(u.Release[:]), unix.ByteSliceToString(u.Machine[:]))
}

// IsRoot 表示是否以 root 身份运行。
func IsRoot() bool { return os.Geteuid() == 0 }

// Disk 是一个目录所在磁盘的空间。
type Disk struct {
	Path  string // 实际统计的目录（目录不存在时向上找到的第一个存在的父目录）
	Total uint64
	Free  uint64 // 普通用户可用的空间
	Dev   uint64 // 设备号，用来判断两个目录是否在同一块磁盘上
}

// DiskOf 返回目录所在磁盘的空间。目录还不存在时（例如即将创建的工作目录），
// 统计它最近的已存在的父目录。
func DiskOf(path string) (Disk, error) {
	p, err := filepath.Abs(path)
	if err != nil {
		return Disk{}, err
	}
	for {
		if _, err := os.Stat(p); err == nil {
			break
		}
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	var st unix.Statfs_t
	if err := unix.Statfs(p, &st); err != nil {
		return Disk{}, err
	}
	var fi unix.Stat_t
	if err := unix.Stat(p, &fi); err != nil {
		return Disk{}, err
	}
	bs := uint64(st.Bsize)
	return Disk{Path: p, Total: uint64(st.Blocks) * bs, Free: uint64(st.Bavail) * bs, Dev: uint64(fi.Dev)}, nil
}

// Listener 是一个正在监听的端口。
type Listener struct {
	Proto string // tcp 或 udp
	Addr  string // 监听地址，0.0.0.0 和 :: 表示所有地址
	Port  int
}

// ErrUnsupported 表示当前系统不支持读取这项信息（只有 Linux 支持）。
var ErrUnsupported = errors.New("当前系统不支持")

// Listeners 从 /proc/net 读取正在监听的 TCP 端口和已绑定的 UDP 端口。
func Listeners() ([]Listener, error) {
	if runtime.GOOS != "linux" {
		return nil, ErrUnsupported
	}
	var out []Listener
	files := []struct {
		path, proto, state string
	}{
		{"/proc/net/tcp", "tcp", "0A"}, {"/proc/net/tcp6", "tcp", "0A"},
		{"/proc/net/udp", "udp", "07"}, {"/proc/net/udp6", "udp", "07"},
	}
	read := 0
	for _, f := range files {
		ls, err := parseProcNet(f.path, f.proto, f.state)
		if err != nil {
			continue
		}
		read++
		out = append(out, ls...)
	}
	if read == 0 {
		return nil, ErrUnsupported
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Port != out[j].Port {
			return out[i].Port < out[j].Port
		}
		return out[i].Proto < out[j].Proto
	})
	return out, nil
}

func parseProcNet(path, proto, wantState string) ([]Listener, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Listener
	sc := bufio.NewScanner(f)
	sc.Scan() // 表头
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 || fields[3] != wantState {
			continue
		}
		addr, port, err := parseHexAddr(fields[1])
		if err != nil {
			continue
		}
		out = append(out, Listener{Proto: proto, Addr: addr, Port: port})
	}
	return out, sc.Err()
}

// parseHexAddr 解析 /proc/net 里“0100007F:1F90”这样的地址：IP 按 32 位小端分组存放。
func parseHexAddr(s string) (string, int, error) {
	h, p, ok := strings.Cut(s, ":")
	if !ok {
		return "", 0, errors.New("格式不对")
	}
	port, err := strconv.ParseUint(p, 16, 16)
	if err != nil {
		return "", 0, err
	}
	raw, err := hex.DecodeString(h)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return "", 0, errors.New("地址格式不对")
	}
	ip := make(net.IP, len(raw))
	for i := 0; i < len(raw); i += 4 {
		ip[i], ip[i+1], ip[i+2], ip[i+3] = raw[i+3], raw[i+2], raw[i+1], raw[i]
	}
	return ip.String(), int(port), nil
}
