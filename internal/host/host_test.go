package host

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"
)

func TestParseHexAddr(t *testing.T) {
	cases := []struct {
		in   string
		addr string
		port int
	}{
		{"0100007F:1F90", "127.0.0.1", 8080},
		{"00000000:0016", "0.0.0.0", 22},
		{"00000000000000000000000000000000:01BB", "::", 443},
		{"0000000000000000FFFF00000100007F:0035", "127.0.0.1", 53},
	}
	for _, c := range cases {
		addr, port, err := parseHexAddr(c.in)
		if err != nil || addr != c.addr || port != c.port {
			t.Errorf("parseHexAddr(%q) = %q %d %v，期望 %q %d", c.in, addr, port, err, c.addr, c.port)
		}
	}
	if _, _, err := parseHexAddr("zz"); err == nil {
		t.Error("格式不对时应报错")
	}
}

func TestParseProcNet(t *testing.T) {
	p := filepath.Join(t.TempDir(), "tcp")
	content := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0000000000000000 100 0 0 10 0
   1: 0100007F:0035 0100007F:D2A4 01 00000000:00000000 00:00000000 00000000     0        0 2 1 0000000000000000 20 4 30 10 -1
`
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	ls, err := parseProcNet(p, "tcp", "0A")
	if err != nil || len(ls) != 1 || ls[0].Port != 8080 || ls[0].Addr != "0.0.0.0" {
		t.Fatalf("只应读出处于监听状态的 8080：%+v %v", ls, err)
	}
}

func TestDiskOfMissingDir(t *testing.T) {
	d, err := DiskOf(filepath.Join(t.TempDir(), "还", "没有", "创建"))
	if err != nil || d.Total == 0 {
		t.Fatalf("目录不存在时应统计已存在的父目录：%+v %v", d, err)
	}
}

func TestInfo(t *testing.T) {
	if OSName() == "" || Kernel() == "" {
		t.Fatal("系统信息不应为空")
	}
	if runtime.GOOS == "linux" {
		if _, err := Listeners(); err != nil {
			t.Fatalf("Linux 上应能读取监听端口：%v", err)
		}
	}
}

// 判断不了一个锁有没有人拿着（打不开锁文件）时，当作有人拿着：宁可留着没用的临时容器，也
// 不能删掉别的任务正在用的。只有锁文件不存在、或者自己能拿到时才算没人拿着。
func TestHeldWhenUnknown(t *testing.T) {
	dir := t.TempDir()
	if Held(filepath.Join(dir, "missing.lock")) {
		t.Error("锁文件不存在：没人拿着")
	}
	free := filepath.Join(dir, "free.lock")
	os.WriteFile(free, nil, 0o600)
	if Held(free) {
		t.Error("自己能拿到：没人拿着")
	}
	if os.Geteuid() == 0 {
		t.Skip("root 读得了任何文件")
	}
	locked := filepath.Join(dir, "unreadable.lock")
	os.WriteFile(locked, nil, 0)
	if !Held(locked) {
		t.Error("打不开锁文件时应当作有人拿着")
	}
}

// 锁文件只在拿着锁的时候存在，放掉后别人能拿到。在上一个拿着它的任务删掉之前打开的那个文件
// 已经不是锁文件了，拿到它的锁不算数。
func TestLockFileOnlyWhileHeld(t *testing.T) {
	p := filepath.Join(t.TempDir(), "locks", "in.lock")
	l, err := TryLock(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TryLock(p); !errors.Is(err, ErrLocked) || !Held(p) {
		t.Fatalf("拿着时别人应拿不到：%v", err)
	}
	old, err := os.Open(p) // 在删掉之前打开
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	l.Unlock()
	if _, err := os.Stat(p); !os.IsNotExist(err) || Held(p) {
		t.Fatalf("放掉后锁文件应删掉：%v", err)
	}
	if sameFile(old, p) {
		t.Error("删掉之前打开的不应算作锁文件")
	}
	l2, err := TryLock(p)
	if err != nil || !sameFile(l2.f, p) {
		t.Fatalf("放掉后应能再拿到：%v", err)
	}
	l2.Unlock()
}

func TestWriteFileDurable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	for _, content := range []string{"first", "second"} {
		if err := WriteFileDurable(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(p); string(b) != content {
			t.Errorf("内容：%q", b)
		}
	}
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Error("临时文件应已改名")
	}
}

// RealPath 解析掉路径里的软链接（自己或者上级目录），不存在的部分原样接在后面。
func TestRealPath(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir()) // macOS 的临时目录本身经过软链接
	if err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(root, "disk")
	os.MkdirAll(filepath.Join(disk, "app"), 0o755)
	os.Symlink(disk, filepath.Join(root, "data"))
	for _, c := range []struct{ in, want string }{
		{filepath.Join(root, "data", "app"), filepath.Join(disk, "app")},
		{filepath.Join(root, "data", "new", "x.txt"), filepath.Join(disk, "new", "x.txt")},
		{filepath.Join(root, "data") + "/", disk},
		{filepath.Join(root, "plain", "x"), filepath.Join(root, "plain", "x")},
		{"", ""},
	} {
		if got := RealPath(c.in); got != c.want {
			t.Errorf("%q：%q，应为 %q", c.in, got, c.want)
		}
	}
}

// /proc/self/mountinfo 的第 5 列是挂载点，空格之类的字符写成 \040 这样的八进制。
func TestMountedAt(t *testing.T) {
	const mountinfo = `22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw
40 22 8:17 / /data rw,relatime shared:20 - ext4 /dev/sdb1 rw
41 22 0:40 / /mnt/my\040disk rw,relatime - tmpfs tmpfs rw
42 22 8:1 /srv/a /opt/app/config rw,relatime - ext4 /dev/sda1 rw
`
	for path, want := range map[string]bool{"/data": true, "/data/app": false, "/mnt/my disk": true, "/mnt/my": false,
		"/opt/app/config": true, "/opt/app": false} {
		if got, err := mountedAt(strings.NewReader(mountinfo), path); err != nil || got != want {
			t.Errorf("mountedAt(%q) = %v %v", path, got, err)
		}
	}
	// Docker 的 overlay 挂载那一行可能很长（lowerdir 列着每一层），后面的挂载点照样要找到。
	long := "50 22 0:60 / /var/lib/docker/overlay2/x/merged rw - overlay overlay rw,lowerdir=" +
		strings.Repeat("/var/lib/docker/overlay2/l/ABCDEFGHIJ:", 3000) + "\n"
	if got, err := mountedAt(strings.NewReader(long+"40 22 8:17 / /data rw - ext4 /dev/sdb1 rw\n"), "/data"); err != nil || !got {
		t.Errorf("很长的一行后面的挂载点也要找到：%v %v", got, err)
	}
	// 读到一半出错时要报错：当成“不是挂载点”会选错替换的办法（整个改名，挂载点会报“设备忙”）。
	broken := io.MultiReader(strings.NewReader("22 1 8:1 / / rw - ext4 /dev/sda1 rw\n"), iotest.ErrReader(errors.New("读到一半出错")))
	if _, err := mountedAt(broken, "/data"); err == nil {
		t.Error("读挂载信息出错时应报错")
	}
}

// 挂在一个目录里面的挂载点（不含它自己）。
func TestMountsUnder(t *testing.T) {
	const mountinfo = `22 1 8:1 / / rw - ext4 /dev/sda1 rw
40 22 8:17 / /srv/app rw - ext4 /dev/sdb1 rw
41 40 0:50 / /srv/app/media rw - nfs host:/media rw
42 22 8:33 / /srv/apples rw - ext4 /dev/sdc1 rw
43 41 0:51 / /srv/app/media/my\040disk rw - tmpfs tmpfs rw
`
	got, err := mountsUnder(strings.NewReader(mountinfo), "/srv/app")
	if err != nil || strings.Join(got, ",") != "/srv/app/media,/srv/app/media/my disk" {
		t.Fatalf("mountsUnder = %v %v", got, err)
	}
}
