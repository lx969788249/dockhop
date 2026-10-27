package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/host"
	"github.com/lx969788249/dockhop/internal/ui"
)

// 停机记录：迁出时临时停止的容器和它们原来的状态。停机之前写到磁盘，恢复后删除。
// 程序被强制结束（kill -9、断电、内存不足）时，记录留在磁盘上，下次运行时据此把
// 容器恢复原状——docker stop 过的容器，unless-stopped 也不会自己起来。

const stateFile = "out-stopped.json"

// StoppedContainer 是一个被临时停止的容器。
type StoppedContainer struct {
	Name  string `json:"name"`
	ID    string `json:"id"`
	State string `json:"state"` // 原来的状态：running 或 paused
}

type stoppedRecord struct {
	PID        int                `json:"pid"`
	Started    time.Time          `json:"started"`
	Containers []StoppedContainer `json:"containers"`
}

func statePath(home string) string { return filepath.Join(home, "state", stateFile) }

func lockPath(home string) string { return filepath.Join(home, "locks", "out.lock") }

// writeStopped 在停机之前写下记录（先写临时文件再改名，不会留下写了一半的文件）。
func writeStopped(home string, list []StoppedContainer) error {
	p := statePath(home)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(stoppedRecord{PID: os.Getpid(), Started: time.Now(), Containers: list}, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func removeStopped(home string) error {
	err := os.Remove(statePath(home))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// RecoverInterrupted 检查上次迁出有没有被中断、留下停着的容器。有的话提示用户
// （不能提问时直接恢复，因为那就是它们原来的状态），把它们恢复原状。
func RecoverInterrupted(ctx context.Context, dc *docker.Client, home string, screen *ui.Screen, prompt *ui.Prompter) error {
	b, err := os.ReadFile(statePath(home))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	// 迁出运行时一直拿着这把锁，进程结束（包括被 kill -9、断电）锁就自动释放。
	// 拿不到锁说明那次迁出还在运行；不用进程号判断：重启后进程号可能被别的程序占用。
	lock, err := host.TryLock(lockPath(home))
	if errors.Is(err, host.ErrLocked) {
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Unlock()
	var rec stoppedRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		screen.Warn("上次迁出的停机记录读不懂，已忽略：%v", err)
		return removeStopped(home)
	}
	var still []StoppedContainer
	for _, c := range rec.Containers {
		ct, _, err := dc.InspectContainer(ctx, c.ID)
		if docker.IsNotFound(err) {
			continue // 容器已经被删掉了
		}
		if err != nil {
			// 不能当作容器已经没了：删掉记录，之后就没法自动恢复了。
			return fmt.Errorf("读不到上次迁出停下的容器 %s 的状态（%v），停机记录保留，处理好 Docker 的问题后重新运行迁出会接着恢复", c.Name, err)
		}
		if !ct.State.Running {
			still = append(still, c)
		}
	}
	if len(still) == 0 {
		return removeStopped(home)
	}
	names := make([]string, len(still))
	for i, c := range still {
		names[i] = c.Name
	}
	screen.Println(fmt.Sprintf("上次迁出（%s）被中断了，有 %d 个容器还停着：%s",
		rec.Started.Format("01-02 15:04"), len(still), strings.Join(names, "、")))
	if prompt.Interactive() {
		ok, err := prompt.Confirm("现在把它们恢复成原来的状态吗？", true)
		if err != nil {
			return err
		}
		if !ok {
			screen.Warn("没有恢复上次停下的容器：%s。需要时请手动启动", strings.Join(names, "、"))
			return removeStopped(home)
		}
	} else {
		screen.Println("没有可以输入的终端，直接把它们恢复成原来的状态。")
	}
	if failed := restoreStates(ctx, dc, still, screen); len(failed) > 0 {
		return fmt.Errorf("有 %d 个容器没能恢复：%s。请执行 docker ps -a 检查，用 docker start 手动启动", len(failed), strings.Join(failed, "、"))
	}
	screen.Info("已恢复上次迁出时停下的 %d 个容器", len(still))
	return removeStopped(home)
}

// restoreStates 按记录把容器恢复成原来的状态（运行或暂停），返回没能恢复的容器名。
// 逆序恢复：停机时先停依赖别人的容器，恢复时先启动被依赖的。
func restoreStates(ctx context.Context, dc *docker.Client, list []StoppedContainer, screen *ui.Screen) []string {
	var failed []string
	for i := len(list) - 1; i >= 0; i-- {
		c := list[i]
		ct, _, err := dc.InspectContainer(ctx, c.ID)
		if err != nil {
			failed = append(failed, c.Name)
			continue
		}
		if !ct.State.Running {
			if err := dc.StartContainer(ctx, c.ID); err != nil {
				screen.Detail("启动 %s 失败：%v", c.Name, err)
				failed = append(failed, c.Name)
				continue
			}
		}
		if c.State == "paused" && !ct.State.Paused {
			if err := dc.PauseContainer(ctx, c.ID); err != nil {
				screen.Detail("暂停 %s 失败：%v", c.Name, err)
				failed = append(failed, c.Name)
				continue
			}
		}
		screen.Detail("源容器已恢复：%s（%s）", c.Name, c.State)
	}
	return failed
}
