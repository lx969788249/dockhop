package ui

import (
	"io"
	"os"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// 在一对虚拟终端上测试 Keys：一端当作用户的键盘，另一端是程序读的终端。
func TestKeysOnTerminal(t *testing.T) {
	keyboard, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skip("没有虚拟终端：", err)
	}
	defer keyboard.Close()
	if err := unix.IoctlSetPointerInt(int(keyboard.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(int(keyboard.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	tty, err := os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer tty.Close()

	// 之前随手按下、还没读走的键不算。
	if _, err := keyboard.WriteString("q"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	p := NewPrompter(tty, io.Discard)
	keys, restore := p.Keys()
	if keys == nil {
		t.Fatal("终端上应该能直接读按键")
	}
	press := func(s string, want Key) {
		t.Helper()
		if _, err := keyboard.WriteString(s); err != nil {
			t.Fatal(err)
		}
		select {
		case k := <-keys:
			if k != want {
				t.Errorf("按 %q：实际 %v，期望 %v", s, k, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("按 %q 以后没有读到键", s)
		}
	}
	press("\x1b", KeyEsc)
	press("\r", KeyEnter)
	press("\x1b[A", KeyOther)
	press("x", 'x')
	restore()
	restore() // 多调用一次也没关系

	st, err := unix.IoctlGetTermios(int(tty.Fd()), ioctlGetTermios)
	if err != nil {
		t.Fatal(err)
	}
	if st.Lflag&unix.ICANON == 0 || st.Lflag&unix.ECHO == 0 {
		t.Error("恢复以后应回到按行输入、显示按下的键")
	}
	// 恢复以后不再占着输入，后面的提问能读到整行。
	if _, err := keyboard.WriteString("yes\n"); err != nil {
		t.Fatal(err)
	}
	if line, err := p.Ask("", nil); err != nil || line != "yes" {
		t.Errorf("恢复以后读一行：实际 %q（%v）", line, err)
	}
}
