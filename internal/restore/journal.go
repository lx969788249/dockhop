package restore

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lx969788249/dockhop/internal/host"
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
	opStart         Op = "start"          // 事务开始：记下进程号和时间
	opStop          Op = "stop"           // 临时停止了新服务器上已有的容器（Name、ID、State：停之前的状态）
	opRename        Op = "rename"         // 已有的同名容器改名备份（Name、ID、Backup：备份用的名字、Networks：原来连着的网络）
	opComposeOld    Op = "compose_old"    // 已有的同名 Compose 项目，即将停止并删除（Name、Old）
	opOldImages     Op = "old_images"     // 上一条里的容器已经存成回滚镜像，接下来删除容器（Name、Images）
	opVolume        Op = "volume"         // 写入数据卷（Name、Existed、Backup）
	opStage         Op = "stage"          // 新建解压用的临时目录（Path），回滚、提交后都整个删掉
	opBind          Op = "bind"           // 整个替换目录（Path、Existed；Backup：同一目录下的备份名，原来的目录改名成它）
	opContents      Op = "contents"       // 原地换单独挂载的目录里的内容（Path；Backup：里面的备份目录；Names：原来的东西；Meta：目录本身原来的属性，见 recordContents）
	opLink          Op = "link"           // 新建软链接（Path）
	opFile          Op = "file"           // 写入 Compose 项目里的文件（Path、Existed、Backup）
	opMkdir         Op = "mkdir"          // 新建目录（Path）
	opNetwork       Op = "network"        // 新建网络（Name）
	opContainer     Op = "container"      // 要新建容器（Name）：建之前先记下，建好了再记 opCreated
	opCreated       Op = "created"        // 新建的容器建好了（Name；ID：它的 ID），回滚时按 ID 删
	opHelper        Op = "helper"         // 新建临时容器（Name），用完就删
	opImages        Op = "images"         // 加载镜像之前这台服务器上已有的镜像（Names：镜像 ID），回滚时这些一个不删
	opTag           Op = "tag"            // 镜像名要改为指向迁入的镜像（Name；ID：原来指向的镜像，为空表示原来没有这个名字）
	opComposeUp     Op = "compose_up"     // 启动 Compose 项目（Name、Names：启动前已有的项目网络）
	opRebuilt       Op = "rebuilt"        // 回滚时重建了原有的 Compose 容器（Name；ID：重建后的 ID），再回滚时它就是原来的服务
	opCommit        Op = "commit"         // 全部提交：新服务都验证通过，不再撤销
	opPartialCommit Op = "partial_commit" // 只提交了一部分（Names：保留下来的组），没保留的组接着撤销（见 partial.go）
)

// partialSide 是只提交了一部分（保留了几组服务）时，一条记录归哪边（见 splitPartial）。
type partialSide int

const (
	byGroup    partialSide = iota + 1 // 跟着它的组：组没保留就撤销，保留了就清理回滚资料；Group 为空的是共用的，算保留
	alwaysUndo                        // 撤销没保留的组时要用到：事务开始、加载镜像之前已有的镜像、上次撤销时重建的原有容器
	noSide                            // 不是改动，两边都不放：提交标记
)

// opRule 是一种记录的约定：check 检查必须有的内容（写日志时检查，缺了就不写、不动手），partial
// 说明只提交了一部分时归哪边。新加一种记录，要在 opRules 里写明这两样；回滚、提交后清理时怎么处理
// 见 rollback.go，TestEveryOpHandled 检查每一种都有着落。
type opRule struct {
	check   func(Entry) bool
	partial partialSide
}

var opRules = map[Op]opRule{
	opStart:         {func(e Entry) bool { return e.PID > 0 }, alwaysUndo},
	opStop:          {func(e Entry) bool { return hasNameID(e) && e.State != "" }, byGroup},
	opRename:        {func(e Entry) bool { return hasNameID(e) && e.Backup != "" }, byGroup},
	opComposeOld:    {hasName, byGroup},
	opOldImages:     {hasName, byGroup},
	opVolume:        {func(e Entry) bool { return hasName(e) && backedUp(e) }, byGroup},
	opStage:         {hasPath, byGroup},
	opBind:          {bindOK, byGroup},
	opContents:      {contentsOK, byGroup},
	opLink:          {hasPath, byGroup},
	opFile:          {func(e Entry) bool { return hasPath(e) && backedUp(e) }, byGroup},
	opMkdir:         {hasPath, byGroup},
	opNetwork:       {hasName, byGroup},
	opContainer:     {hasName, byGroup},
	opCreated:       {hasNameID, byGroup},
	opHelper:        {hasName, byGroup},
	opImages:        {always, alwaysUndo},
	opTag:           {hasName, byGroup},
	opComposeUp:     {hasName, byGroup},
	opRebuilt:       {hasNameID, alwaysUndo},
	opCommit:        {func(e Entry) bool { return len(e.Names) == 0 }, noSide},
	opPartialCommit: {func(e Entry) bool { return len(e.Names) > 0 }, noSide},
}

func hasName(e Entry) bool   { return e.Name != "" }
func hasNameID(e Entry) bool { return e.Name != "" && e.ID != "" }
func hasPath(e Entry) bool   { return e.Path != "" }
func always(Entry) bool      { return true }

// backedUp：原来就有的（Existed），要记下备份放在哪里。
func backedUp(e Entry) bool { return !e.Existed || e.Backup != "" }

// bindOK：整个替换的备份和目录在同一个地方（改名是原子的）。
func bindOK(e Entry) bool {
	return hasPath(e) && backedUp(e) && filepath.Dir(e.Backup) == filepath.Dir(e.Path)
}

// contentsOK：原地换内容的备份目录在目录里面，还要记下目录本身原来的属性（见 recordContents）。
func contentsOK(e Entry) bool {
	return hasPath(e) && filepath.Dir(e.Backup) == e.Path && e.Meta != nil
}

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
	Networks json.RawMessage   `json:"networks,omitempty"` // 只有 opRename 用
	Old      []OldContainer    `json:"old,omitempty"`
	Images   map[string]string `json:"images,omitempty"` // 容器 ID → 回滚镜像
	Names    []string          `json:"names,omitempty"`  // 按记录类型各有含义，见上面每种记录的说明
	Group    string            `json:"group,omitempty"`  // 属于哪一组服务（见 groups.go）；为空表示整次迁入共用
	Meta     *FileMeta         `json:"meta,omitempty"`   // 原地换内容时，目录本身原来的属主、权限、修改时间
}

// FileMeta 是一个文件或目录本身的属主、权限和修改时间（纳秒）；Link 表示它是软链接（没有自己的权限）。
type FileMeta struct {
	Mode  os.FileMode `json:"mode"`
	UID   int         `json:"uid"`
	GID   int         `json:"gid"`
	MTime int64       `json:"mtime"`
	Link  bool        `json:"link,omitempty"`
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
	// 日志文件本身也要在目录里留住：断电后找不到日志，就不知道要撤销什么。
	if err := host.SyncDir(dir); err != nil {
		f.Close()
		return nil, err
	}
	if err := host.SyncDir(filepath.Dir(dir)); err != nil {
		f.Close()
		return nil, err
	}
	return j, nil
}

// appendJournal 在 dir 里已有的事务日志后面追加一条记录：回滚时记下它自己做的改动（见 opRebuilt）。
func appendJournal(dir string, e Entry) error {
	f, err := os.OpenFile(journalPath(dir), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	j := &Journal{dir: dir, f: f}
	defer j.close()
	return j.add(e)
}

// add 追加一条记录并写到磁盘。写不进去时不能继续改动。
func (j *Journal) add(e Entry) error {
	rule, ok := opRules[e.Op]
	if !ok {
		return fmt.Errorf("写入事务日志失败：不认识的记录 %q", e.Op)
	}
	if !rule.check(e) {
		return fmt.Errorf("写入事务日志失败：记录 %s 少了必须有的内容（%+v）", e.Op, e)
	}
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
		if _, ok := opRules[e.Op]; !ok {
			// 这个版本不认识的记录（例如更新的版本写的）：撤销时会悄悄跳过它，改动就永远留下了。
			return nil, fmt.Errorf("%w（第 %d 行是这个版本不认识的记录 %q）", ErrJournalDamaged, n, e.Op)
		}
		out = append(out, upgrade(e))
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// upgrade 把以前的版本写的记录换成现在的写法。读旧日志只在这里处理：v1.2.7 及以前把原地换内容
// 也记成 bind（备份目录在 Path 里面），把只提交了一部分也记成 commit（Names 不为空）。现在写的记录
// 不会是这两种样子（见 bindOK 和 opRules），对它们什么都不做。
func upgrade(e Entry) Entry {
	switch {
	case e.Op == opBind && e.Existed && filepath.Dir(e.Backup) == e.Path:
		e.Op, e.Existed = opContents, false
	case e.Op == opCommit && len(e.Names) > 0:
		e.Op = opPartialCommit
	}
	return e
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
	if err := os.Rename(tmp, journalPath(dir)); err != nil {
		return err
	}
	return host.SyncDir(dir)
}
