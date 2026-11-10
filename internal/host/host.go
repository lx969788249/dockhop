// Package host 读取本机信息：系统、磁盘空间、挂载点、路径的真实位置、正在监听的端口。
// 只读系统文件和系统调用，不依赖 ss、df 等外部命令。
package host

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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

// IsMountPoint 表示 path 本身是不是挂载点（单独挂载的磁盘、绑定挂载的目录等）。挂载点不能改名、
// 不能删除，只能改它里面的内容。path 的上级目录里有软链接时按真实路径找；path 本身是软链接时
// 不算（改名改的是软链接）。读不到挂载信息时返回错误，不猜：猜错了会选错替换的办法。
func IsMountPoint(path string) (bool, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, fmt.Errorf("读不到挂载信息：%w", err)
	}
	defer f.Close()
	return mountedAt(f, filepath.Join(RealPath(filepath.Dir(path)), filepath.Base(path)))
}

// MountsUnder 返回挂在 path 里面（不含 path 本身）的挂载点。path 里有软链接时按真实路径找。
// 读不到挂载信息时返回错误。
func MountsUnder(path string) ([]string, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("读不到挂载信息：%w", err)
	}
	defer f.Close()
	return mountsUnder(f, RealPath(path))
}

// RealPath 是 p 在本机的真实位置：解析掉路径里的软链接（自己或者上级目录），不存在的部分原样
// 接在后面。判断两个路径是不是同一处、是不是互相包含都按它：经过软链接指过来的也是同一处。
// 要求路径必须存在、读不到要报错的地方直接用 filepath.EvalSymlinks。
func RealPath(p string) string {
	if p == "" {
		return ""
	}
	p = filepath.Clean(p)
	rest := ""
	for cur := p; ; {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

func mountedAt(r io.Reader, path string) (bool, error) {
	found := false
	err := eachMount(r, func(mp string) bool {
		found = mp == path
		return found
	})
	return found, err
}

func mountsUnder(r io.Reader, path string) ([]string, error) {
	var out []string
	err := eachMount(r, func(mp string) bool {
		if strings.HasPrefix(mp, path+"/") {
			out = append(out, mp)
		}
		return false
	})
	return out, err
}

// eachMount 把 /proc/self/mountinfo 里的每个挂载点交给 fn，fn 返回 true 时停下：第 5 列是挂载点，
// 空格、制表符、换行和反斜杠写成 \040 这样的三位八进制。一行可能很长（overlay 的 lowerdir 列着
// 每一层），不限长度。读到一半出错时返回错误。
func eachMount(r io.Reader, fn func(mountPoint string) bool) error {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if f := strings.Fields(line); len(f) > 4 && fn(unescapeOctal(f[4])) {
			return nil
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("读挂载信息失败：%w", err)
		}
	}
}

func unescapeOctal(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
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
