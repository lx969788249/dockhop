package restore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestJournalTornLastLine(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rb")
	j, err := openJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	j.add(Entry{Op: opVolume, Name: "data"})
	j.close()
	// 写最后一条时断电：只写了一半。
	f, _ := os.OpenFile(journalPath(dir), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"op":"bind","path":"/sr`)
	f.Close()
	entries, err := readJournal(dir)
	if err != nil || len(entries) != 2 || entries[1].Op != opVolume || committed(entries) {
		t.Fatalf("应忽略写了一半的最后一行：%+v %v", entries, err)
	}
}

func TestEntryJSON(t *testing.T) {
	e := Entry{Op: opRename, Name: "web", Networks: json.RawMessage(`{"appnet":{"IPAMConfig":{"IPv4Address":"10.0.0.2"}}}`)}
	b, _ := json.Marshal(e)
	var back Entry
	if err := json.Unmarshal(b, &back); err != nil || string(back.Networks) != string(e.Networks) {
		t.Errorf("日志记录应能原样读回：%s", b)
	}
}

// 只删除没启动的组时，日志里只留下这些组的改动（和开头的那条），之后再被强制结束也只会删它们。
func TestRewriteJournal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rb")
	j, err := openJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	j.group = "1"
	j.add(Entry{Op: opContainer, Name: "web"})
	j.group = "2"
	j.add(Entry{Op: opContainer, Name: "api"})
	j.add(Entry{Op: opVolume, Name: "keep", Group: "1"})
	j.close()
	entries, _ := readJournal(dir)
	var drop []Entry
	for _, e := range entries {
		if e.Op == opStart || e.Group == "2" {
			drop = append(drop, e)
		}
	}
	if len(drop) != 2 || entries[3].Group != "1" {
		t.Fatalf("组记录不对：%+v", entries)
	}
	if err := rewriteJournal(dir, drop); err != nil {
		t.Fatal(err)
	}
	back, _ := readJournal(dir)
	if len(back) != 2 || back[0].Op != opStart || back[1].Name != "api" || committed(back) {
		t.Fatalf("改写后的日志不对：%+v", back)
	}
}

// 日志中间有一条记录损坏时，不知道漏掉的是哪一处改动，不能当它不存在。
func TestJournalDamagedMiddleLine(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rb")
	j, err := openJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	j.add(Entry{Op: opVolume, Name: "data"})
	j.close()
	f, _ := os.OpenFile(journalPath(dir), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString("{\"op\":\"bind\",\"pa\x00\x00\n")
	f.WriteString(`{"op":"container","name":"web"}` + "\n")
	f.Close()
	if _, err := readJournal(dir); !errors.Is(err, ErrJournalDamaged) {
		t.Fatalf("中间损坏应报错：%v", err)
	}
	// 读得懂但不是日志记录的行也算损坏。
	os.WriteFile(journalPath(dir), []byte(`{"op":"start"}`+"\n"+`{"x":1}`+"\n"+`{"op":"commit"}`+"\n"), 0o600)
	if _, err := readJournal(dir); !errors.Is(err, ErrJournalDamaged) {
		t.Fatalf("没有 op 的记录应算损坏：%v", err)
	}
}

// 写日志时检查每种记录必须有的内容：缺了就报错，不写进日志，调用的地方也就不会接着动手；不认识的
// 记录类型同样不写。
func TestJournalChecksEntries(t *testing.T) {
	j, err := openJournal(filepath.Join(t.TempDir(), "rb"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.close()
	for _, c := range []struct {
		why string
		e   Entry
	}{
		{"没有 Path", Entry{Op: opBind}},
		{"原来就有，却没有备份", Entry{Op: opBind, Path: "/srv/data", Existed: true}},
		{"整个替换，备份却在目录里面", Entry{Op: opBind, Path: "/srv/data", Existed: true, Backup: "/srv/data/.dockhop-old-1"}},
		{"原地换内容，备份不在目录里面", Entry{Op: opContents, Path: "/srv/data", Backup: "/srv/.data.old", Meta: &FileMeta{}}},
		{"原地换内容，没记目录本身的属性", Entry{Op: opContents, Path: "/srv/data", Backup: "/srv/data/.dockhop-old-1"}},
		{"全部提交却带着保留的组", Entry{Op: opCommit, Names: []string{"1"}}},
		{"部分提交却没有保留的组", Entry{Op: opPartialCommit}},
		{"没有 ID 和备份的名字", Entry{Op: opRename, Name: "web"}},
		{"不认识的记录", Entry{Op: "bogus", Name: "x"}},
	} {
		if err := j.add(c.e); err == nil {
			t.Errorf("%s：%+v 应该写不进去", c.why, c.e)
		}
	}
	if err := j.add(Entry{Op: opBind, Path: "/srv/data", Existed: true, Backup: "/srv/.data.old"}); err != nil {
		t.Fatalf("完整的记录应能写进去：%v", err)
	}
}

// 日志里有这个版本不认识的记录（例如更新的版本写的）时，不能自动撤销：撤销时会悄悄跳过它，改动就
// 永远留下了。和中间有损坏的记录一样，交给人工处理。
func TestJournalUnknownOp(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rb")
	j, err := openJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	j.close()
	f, _ := os.OpenFile(journalPath(dir), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"op":"from_the_future","name":"x"}` + "\n")
	f.Close()
	if _, err := readJournal(dir); !errors.Is(err, ErrJournalDamaged) {
		t.Fatalf("不认识的记录应当作损坏处理：%v", err)
	}
}

// 以前的版本把原地换内容也记成 bind（靠备份目录在 Path 里面区分），把只提交了一部分也记成 commit（靠
// Names 不为空区分）。读日志时换成现在各自单独的记录；这是读旧日志唯一要处理的地方。整个替换的 bind、
// 全部提交的 commit 原样不动。
func TestReadJournalUpgradesOldRecords(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rb")
	os.MkdirAll(dir, 0o700)
	lines := []string{
		`{"op":"start","pid":1}`,
		`{"op":"bind","path":"/srv/data","existed":true,"backup":"/srv/data/.dockhop-old-1","names":["a"],"meta":{"mode":493,"uid":0,"gid":0,"mtime":1}}`,
		`{"op":"bind","path":"/srv/web","existed":true,"backup":"/srv/.web.dockhop-old-1"}`,
		`{"op":"commit","names":["2"]}`,
		`{"op":"commit"}`,
	}
	os.WriteFile(journalPath(dir), []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	entries, err := readJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	var ops []string
	for _, e := range entries {
		ops = append(ops, string(e.Op))
	}
	if got := strings.Join(ops, " "); got != "start contents bind partial_commit commit" {
		t.Fatalf("读出来的记录：%s", got)
	}
	if c := entries[1]; c.Path != "/srv/data" || c.Backup != "/srv/data/.dockhop-old-1" || !slices.Equal(c.Names, []string{"a"}) || c.Meta == nil {
		t.Errorf("原地换内容的记录读错了：%+v", c)
	}
	if p := entries[3]; !slices.Equal(p.Names, []string{"2"}) {
		t.Errorf("保留下来的组读错了：%+v", p)
	}
}
