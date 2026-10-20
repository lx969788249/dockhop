package host

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
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
