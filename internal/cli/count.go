package cli

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 成功完成迁移的次数存在 dockhop.run 上（只有这一个数字）：开头的菜单读出来显示，迁入成功时
// 加一。只是锦上添花，不能影响使用：连不上、出错、免费额度用完时，菜单里不显示这一行，加一
// 也就算了，不报错，最多多等几秒。

// shownFrom：次数满这么多才在菜单里显示。次数还少的时候不显示，满了之后照常显示真实的次数。
const shownFrom = 20

// countOn 表示要不要访问计数：没有设置地址（测试）、用户关闭（DOCKHOP_NO_STATS=1 或
// DO_NOT_TRACK=1）、或者不是正式发布的版本（自己编译、测试用的）时不访问。
func (e *Env) countOn() bool {
	off := func(name string) bool { v := e.Getenv(name); return v != "" && v != "0" }
	release := e.Version != "dev" && !strings.Contains(e.Version, "-")
	return e.Count != "" && release && !off("DOCKHOP_NO_STATS") && !off("DO_NOT_TRACK")
}

// migrations 读出成功完成迁移的次数；读不到时返回 0。通过一行命令运行时，dockhop.run 返回
// run.sh 时已经把次数填在里面，run.sh 用 DOCKHOP_COUNT 交过来，不用再问一次（从国内访问
// dockhop.run 常常要好几秒）；直接运行程序时才去读，最多等 1.5 秒，免得菜单出来得慢。
func migrations(env *Env) int {
	if !env.countOn() {
		return 0
	}
	if n, err := strconv.Atoi(env.Getenv("DOCKHOP_COUNT")); err == nil && n >= 0 {
		return n
	}
	status, body := countRequest(env, http.MethodGet, 1500*time.Millisecond)
	n, err := strconv.Atoi(body)
	if status != http.StatusOK || err != nil || n < 0 {
		return 0
	}
	return n
}

// countMigration 在迁入成功时把次数加一，结果不管。马上在后台发出，返回的 wait 等它结束：
// 从国内访问 dockhop.run 常常要好几秒，所以和收尾（删除临时文件、通知旧服务器、显示结果）
// 同时进行，最多等 10 秒。
func countMigration(env *Env) (wait func()) {
	if !env.countOn() {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		countRequest(env, http.MethodPost, 10*time.Second)
	}()
	return func() { <-done }
}

// countRequest 发一个计数请求，返回状态码和回复的内容（最多 32 个字节）；出错时状态码为 0。
func countRequest(env *Env, method string, timeout time.Duration) (int, string) {
	ctx, cancel := context.WithTimeout(env.Context, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, env.Count, nil)
	if err != nil {
		return 0, ""
	}
	req.Header.Set("User-Agent", "DockHop/"+env.Version)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, ""
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 32))
	return res.StatusCode, strings.TrimSpace(string(b))
}
