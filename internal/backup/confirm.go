package backup

import (
	"fmt"
	"strings"

	"github.com/lx969788249/dockhop/internal/spec"
)

// 停机之前的确认：Compose 项目的文件和运行中的容器不一致、要连带停下没选中但共用数据的容器、
// 有容器一停就会被 Docker 删掉（--rm）时，先问清楚或者拒绝。这时还什么都没改动。

func (r *run) confirmDrift(col *collected) error {
	for _, p := range col.projects {
		if len(p.drift) == 0 {
			continue
		}
		msg := fmt.Sprintf("Compose 项目 %s 的服务 %s 现在运行的配置和目录里的文件对不上（启动后改过文件，或启动时用了临时设置的变量）",
			p.u.project, strings.Join(p.drift, "、"))
		if !r.Prompt.Interactive() {
			r.Screen.Warn("%s；迁入后会按现在的文件启动", msg)
			continue
		}
		r.Screen.Hold()
		r.Screen.Println(msg)
		n, err := r.Prompt.Choose([]string{
			"取消，我先在 " + p.u.workingDir + " 执行 docker compose up -d，让运行的容器和文件一致（推荐）",
			"仍然迁移，迁入后按现在的文件启动",
		})
		if err != nil {
			return err
		}
		if n == 1 {
			return r.Screen.StepFailed("已取消：Compose 项目 "+p.u.project+" 的配置和运行中的容器不一致", "", "处理好之后重新运行迁出")
		}
		r.Screen.Warn("%s；迁入后会按现在的文件启动", msg)
	}
	return nil
}

func (r *run) confirmShared(col *collected) error {
	if len(col.shared) == 0 {
		return nil
	}
	names := make([]string, len(col.shared))
	for i, it := range col.shared {
		names[i] = it.name()
	}
	list := strings.Join(names, "、")
	if r.opts.StopShared {
		r.Screen.Warn("没有选中的容器 %s 和选中的容器共用数据或网络，打包时会临时停止，打包完自动恢复", list)
		return nil
	}
	if !r.Prompt.Interactive() {
		return r.Screen.StepFailed("没有选中的容器 "+list+" 和选中的容器共用数据或网络", "",
			"加上 --stop-shared，允许打包时临时停止它们（打包完自动恢复）")
	}
	r.Screen.Hold()
	ok, err := r.Prompt.Confirm("没有选中的容器 "+list+" 和选中的容器共用数据或网络。打包时临时停止它们吗？打包完会自动恢复", true)
	if err != nil {
		return err
	}
	if !ok {
		return r.Screen.StepFailed("已取消：和选中的容器共用数据的容器不能停止，数据无法保证一致", "", "")
	}
	return nil
}

// checkAutoRemove 停机打包要停止的容器里，有用 --rm 启动的就不能继续：Docker 停止这种
// 容器时会把它连同匿名卷、容器里没挂载出来的改动一起删掉，打包不到，也恢复不了。
func (r *run) checkAutoRemove(col *collected) error {
	var names []string
	for _, it := range append(append([]*item(nil), col.sel.items...), col.shared...) {
		if spec.StopRiskOf(it.c) == spec.StopRemoves {
			names = append(names, it.name())
		}
	}
	if len(names) == 0 {
		return nil
	}
	list := strings.Join(names, "、")
	return r.Screen.StepFailed("容器 "+list+" 是用 --rm 启动的：停机打包时一停止，Docker 就会把它连同匿名卷和容器里的改动一起删掉，"+
		"没法打包，也没法恢复，已取消（什么都没有改动）", "",
		"先把它重新创建成不带 --rm 的容器再迁出")
}
