package restore

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// 事务日志：迁入时对新服务器的每一处改动，都先在日志里追加一条记录（写到磁盘）
// 再动手。失败、被 Ctrl+C 中断时按日志倒着撤销（删除这次迁入的内容，把被替换的
// 原有服务放回去）；程序被强制结束（kill -9、断电）时，日志留在磁盘上，下次运行时
// 据此撤销。写下“已提交”之后就不再撤销。
//
// 每一种撤销操作都可以重复执行：撤销到一半又被中断，下次再从头撤销一遍也是安全的。

// Op 是一种改动。
type Op string

const (
	opStart      Op = "start"       // 事务开始：记下进程号和时间
	opStop       Op = "stop"        // 临时停止了新服务器上已有的容器（Name、ID、State）
	opRename     Op = "rename"      // 已有的同名独立容器改名备份（Name、ID、Backup、State、Networks）
	opComposeOld Op = "compose_old" // 已有的同名 Compose 项目，即将停止并删除（Name、Old）
	opOldImages  Op = "old_images"  // 上一条里的容器已经存成回滚镜像，接下来删除容器（Name、Images）
	opVolume     Op = "volume"      // 写入数据卷（Name、Existed、Backup）
	opBind       Op = "bind"        // 替换目录（Path、Existed、Backup）
	opLink       Op = "link"        // 新建软链接（Path）
	opFile       Op = "file"        // 写入 Compose 项目里的文件（Path、Existed、Backup）
	opMkdir      Op = "mkdir"       // 新建目录（Path）
	opNetwork    Op = "network"     // 新建网络（Name）
	opContainer  Op = "container"   // 新建容器（Name）
	opHelper     Op = "helper"      // 新建临时容器（Name），用完就删
	opComposeUp  Op = "compose_up"  // 启动 Compose 项目（Name、Networks：启动前已有的项目网络）
	opCommit     Op = "commit"      // 已提交：新服务都验证通过，不再撤销
)

// Entry 是日志里的一条记录。
type Entry struct {
	Op       Op                `json:"op"`
	Time     time.Time         `json:"time"`
	PID      int               `json:"pid,omitempty"`
	Name     string            `json:"name,omitempty"`
	ID       string            `json:"id,omitempty"`
	State    string            `json:"state,omitempty"`
	Path     string            `json:"path,omitempty"`
	Backup   string            `json:"backup,omitempty"`
	Existed  bool              `json:"existed,omitempty"`
	Networks json.RawMessage   `json:"networks,omitempty"`
	Old      []OldContainer    `json:"old,omitempty"`
	Images   map[string]string `json:"images,omitempty"` // 容器 ID → 回滚镜像
	Names    []string          `json:"names,omitempty"`
	Group    string            `json:"group,omitempty"` // 属于哪一组服务（见 groups.go）；为空表示整次迁入共用
}

// OldContainer 是新服务器上原有的 Compose 容器，回滚时据此原样重建。
type OldContainer struct {
	Name    string            `json:"name"`
	ID      string            `json:"id"`
	State   string            `json:"state"`
	Raw     json.RawMessage   `json:"raw"`
	Volumes map[string]string `json:"volumes,omitempty"` // 匿名卷：挂载位置 → 卷名
}

// Journal 是一个打开的事务日志。
type Journal struct {
	dir   string
	f     *os.File
	n     int
	group string // 之后的记录属于哪一组服务
}

func journalPath(dir string) string { return filepath.Join(dir, "journal.jsonl") }

// openJournal 在 dir 里新建事务日志（dir 必须不存在或为空）。
func openJournal(dir string) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(journalPath(dir), os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	j := &Journal{dir: dir, f: f}
	if err := j.add(Entry{Op: opStart, PID: os.Getpid()}); err != nil {
		f.Close()
		return nil, err
	}
	return j, nil
}

// add 追加一条记录并写到磁盘。写不进去时不能继续改动。
func (j *Journal) add(e Entry) error {
	e.Time = time.Now()
	if e.Group == "" {
		e.Group = j.group
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := j.f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("写入事务日志失败：%w", err)
	}
	if err := j.f.Sync(); err != nil {
		return fmt.Errorf("写入事务日志失败：%w", err)
	}
	j.n++
	return nil
}

// file 返回回滚资料目录里一个新的文件路径。
func (j *Journal) file(kind string) string {
	return filepath.Join(j.dir, fmt.Sprintf("%s-%04d", kind, j.n))
}

func (j *Journal) close() {
	if j != nil && j.f != nil {
		j.f.Close()
		j.f = nil
	}
}

// ErrJournalDamaged 表示事务日志中间有读不懂的记录：不知道漏掉的是哪一处改动，
// 不能自动撤销，回滚资料要全部保留、交给人工处理。
var ErrJournalDamaged = errors.New("事务日志中间有一条记录损坏了")

// readJournal 读出日志里的全部记录。最后一行写了一半（写的时候断电）时忽略那一行：
// 记录总是在改动之前写的，没写完说明那处改动还没发生。别的行读不懂时返回错误。
func readJournal(dir string) ([]Entry, error) {
	f, err := os.Open(journalPath(dir))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Entry
	bad := 0 // 读不懂的那一行的行号
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 64<<20)
	for n := 1; sc.Scan(); n++ {
		if bad > 0 {
			return nil, fmt.Errorf("%w（第 %d 行）", ErrJournalDamaged, bad)
		}
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil || e.Op == "" {
			bad = n
			continue
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// rewriteJournal 把日志换成 entries（先写临时文件再改名，换到一半断电也不会坏）。
func rewriteJournal(dir string, entries []Entry) error {
	tmp := journalPath(dir) + ".new"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	for _, e := range entries {
		b, err := json.Marshal(e)
		if err == nil {
			_, err = f.Write(append(b, '\n'))
		}
		if err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, journalPath(dir))
}

func committed(entries []Entry) bool {
	for _, e := range entries {
		if e.Op == opCommit {
			return true
		}
	}
	return false
}
