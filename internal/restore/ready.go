package restore

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/ui"
)

// 等容器就绪。有健康检查的，按它自己的设置等：Docker 要在启动宽限期过后、连续 retries 次
// 没通过才判它“不健康”，DockHop 最多等到那个时候再多 30 秒。中途不健康、在重启都接着等：
// 有的应用在小机器上启动慢，有的要等它连的服务先起来。没有健康检查的，等它稳定运行几秒、
// 不在反复重启。容器退出了、Docker 也不再重启它时，马上算失败。
//
// 等待时不把每秒一次的查询写进日志，只记状态的变化、每次健康检查的结果和容器的重启。没能
// 就绪时，把容器最后的输出写进日志（之后回滚会删掉容器，就看不到了），报错里接上最能说明
// 问题的一行。

// minHealthWait 是等容器就绪至少等多久（测试时改短）。
var minHealthWait = 60 * time.Second

// waitReady 等一个刚启动的容器就绪。
func (r *run) waitReady(ctx context.Context, it *citem) error {
	name, id := it.m.Name, it.newID
	poll := docker.Quiet(ctx)
	c, _, err := r.Docker.InspectContainer(poll, id)
	if err != nil {
		return err
	}
	since := it.startedAt
	if since.IsZero() {
		since = time.Now()
	}
	hc := c.Config.Healthcheck
	hasCheck := hc != nil && len(hc.Test) > 0 && hc.Test[0] != "NONE"
	limit, grace, what := minHealthWait, r.startupGrace(), "确认运行"
	if hasCheck {
		limit, what = healthTimeout(minHealthWait, hc.StartPeriod, hc.Interval, hc.Timeout, hc.Retries), "健康检查"
	}
	limit += it.extra
	if hasCheck {
		r.Log.Printf("等容器 %s 就绪：健康检查%s，最多等 %s", name, checkRule(hc.StartPeriod, hc.Interval, hc.Timeout, hc.Retries), ui.Elapsed(limit))
	} else {
		r.Log.Printf("等容器 %s 就绪：没有健康检查，确认它稳定运行 %s，最多等 %s", name, ui.Elapsed(grace), ui.Elapsed(limit))
	}
	w := &watch{r: r, name: name, restarts: -1}
	for {
		w.see(c)
		elapsed := time.Since(since)
		switch p := phaseOf(c); {
		case p == phaseStopped:
			how := "启动后退出了"
			if elapsed < 10*time.Second {
				how = "启动后马上退出了"
			}
			return r.notReady(ctx, it, fmt.Sprintf("容器 %s %s（%s）", name, how, exitInfo(c)), exitedHint(c))
		case hasCheck && w.status == "healthy":
			r.Log.Printf("容器 %s 就绪：健康检查通过（启动后 %s）", name, ui.Elapsed(elapsed))
			return nil
		case !hasCheck && c.State.Status == "running" && w.upFor(c) >= grace:
			r.Log.Printf("容器 %s 就绪：已经稳定运行 %s", name, ui.Elapsed(w.upFor(c)))
			return nil
		case elapsed >= limit && hasCheck:
			return r.notReady(ctx, it, fmt.Sprintf("容器 %s 的健康检查没有通过（等了 %s，%s）", name, ui.Elapsed(limit), w.summary()), "")
		case elapsed >= limit:
			return r.notReady(ctx, it, fmt.Sprintf("容器 %s 没能稳定运行（等了 %s，%s）", name, ui.Elapsed(limit), w.summary()), "")
		}
		r.Screen.Note(fmt.Sprintf("%s %s · %s · %d/%d 秒", name, what, statusText(w.status), int(elapsed.Seconds()), int(limit.Seconds())))
		if err := sleep(ctx, time.Second); err != nil {
			return err
		}
		if c, _, err = r.Docker.InspectContainer(poll, id); err != nil {
			return err
		}
	}
}

// healthTimeout 是健康检查最长等多久：到 Docker 按容器自己的设置判它“不健康”的时候，再多等
// 30 秒；至少 base，最多一小时。Docker 在启动宽限期过后，连续 retries 次检查没通过才判不健康；
// 每次检查最多花 timeout，检查完再隔 interval 才做下一次，所以最晚要
// 启动宽限期 + retries ×（interval + timeout）。没设置的按 Docker 的默认值：每 30 秒一次、
// 每次最多 30 秒、连续 3 次。
func healthTimeout(base time.Duration, startPeriod, interval, timeout int64, retries int) time.Duration {
	interval, timeout, retries = healthDefaults(interval, timeout, retries)
	need := time.Duration(startPeriod+int64(retries)*(interval+timeout)) + 30*time.Second
	return min(max(need, base), time.Hour)
}

func healthDefaults(interval, timeout int64, retries int) (int64, int64, int) {
	if interval <= 0 {
		interval = int64(30 * time.Second)
	}
	if timeout <= 0 {
		timeout = int64(30 * time.Second)
	}
	if retries <= 0 {
		retries = 3
	}
	return interval, timeout, retries
}

// checkRule 用大白话写出健康检查的设置。
func checkRule(startPeriod, interval, timeout int64, retries int) string {
	interval, timeout, retries = healthDefaults(interval, timeout, retries)
	s := fmt.Sprintf("每 %s查一次（每次最多 %s），连续 %d 次没通过算不健康",
		ui.Elapsed(time.Duration(interval)), ui.Elapsed(time.Duration(timeout)), retries)
	if startPeriod > 0 {
		s = "启动后 " + ui.Elapsed(time.Duration(startPeriod)) + "内不算，之后" + s
	}
	return "（" + s + "）"
}

func (r *run) startupGrace() time.Duration {
	if v := r.Getenv("DOCKHOP_TEST_STARTUP_GRACE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return 3 * time.Second
}

// watch 记下等待中看到的变化，写进日志。
type watch struct {
	r            *run
	name         string
	status       string    // 运行状态；在运行、有健康检查时是健康状态
	restarts     int       // Docker 自动重启的次数；-1 表示还没看过
	streak       int       // 健康检查连续没通过的次数
	probed       time.Time // 已经写进日志的最后一次健康检查
	runningSince time.Time // 这次连续运行从什么时候开始（按这里的时间）
}

func (w *watch) see(c *docker.Container) {
	log := w.r.Log.Printf
	status := c.State.Status
	if c.State.Health != nil && status == "running" {
		status = c.State.Health.Status
	}
	switch {
	case w.restarts < 0 && c.RestartCount > 0:
		log("容器 %s 已经自动重启过 %d 次", w.name, c.RestartCount)
	case w.restarts >= 0 && c.RestartCount > w.restarts && c.State.Status == "restarting":
		log("容器 %s 退出了（%s），Docker 正在自动重启它（第 %d 次）", w.name, exitInfo(c), c.RestartCount)
	case w.restarts >= 0 && c.RestartCount > w.restarts:
		log("容器 %s 退出后被 Docker 自动重启了（共 %d 次）", w.name, c.RestartCount)
	}
	if w.restarts < 0 || c.RestartCount != w.restarts || c.State.Status != "running" {
		w.runningSince = time.Now()
	}
	w.restarts = c.RestartCount
	if status != w.status {
		log("容器 %s：%s", w.name, statusText(status))
		w.status = status
	}
	h := c.State.Health
	if h == nil {
		return
	}
	w.streak = h.FailingStreak
	for _, p := range h.Log {
		end, err := time.Parse(time.RFC3339Nano, p.End)
		if err != nil || !end.After(w.probed) {
			continue
		}
		w.probed = end
		out := strings.Join(strings.Fields(p.Output), " ")
		if out != "" {
			out = "：" + clip(out, 200)
		}
		if p.ExitCode == 0 {
			log("容器 %s 健康检查通过", w.name)
		} else {
			log("容器 %s 健康检查没通过（退出码 %d）%s", w.name, p.ExitCode, out)
		}
	}
}

// upFor 是容器这次已经连续运行了多久：优先看 Docker 记的启动时间，读不出来时按这里看到的。
func (w *watch) upFor(c *docker.Container) time.Duration {
	if t, err := time.Parse(time.RFC3339Nano, c.State.StartedAt); err == nil && t.Year() > 1 {
		if d := time.Since(t); d >= 0 {
			return d
		}
	}
	return time.Since(w.runningSince)
}

// summary 是没能就绪时容器最后的样子，接在报错里。
func (w *watch) summary() string {
	parts := []string{"最后的状态：" + statusText(w.status)}
	if w.streak > 0 {
		parts = append(parts, fmt.Sprintf("健康检查连续 %d 次没通过", w.streak))
	}
	if w.restarts > 0 {
		parts = append(parts, fmt.Sprintf("自动重启了 %d 次", w.restarts))
	}
	return strings.Join(parts, "，")
}

func statusText(s string) string {
	switch s {
	case "starting":
		return "启动中"
	case "healthy":
		return "健康"
	case "unhealthy":
		return "不健康"
	case "running":
		return "运行中"
	case "restarting":
		return "正在重启"
	case "created":
		return "还没启动"
	case "exited", "dead":
		return "已停止"
	case "paused":
		return "已暂停"
	}
	return s
}

// exitInfo 说明容器为什么停了：退出码，是不是内存不够被系统杀掉，Docker 报的错。
func exitInfo(c *docker.Container) string {
	s := fmt.Sprintf("退出码 %d", c.State.ExitCode)
	if c.State.OOMKilled {
		s += "，内存不够被系统杀掉了"
	}
	if e := strings.TrimSpace(c.State.Error); e != "" {
		s += "，" + clip(e, 200)
	}
	return s
}

// confirmReady 在决定提交之前把就绪了的容器再查一遍：没有健康检查的稳定运行几秒就算就绪，
// 之后还可能停掉；一组服务启动、等待的时候，先就绪的组也可能已经出了问题。停了、在重启、
// 不健康了的，它所在的组和没能启动的一样处理。
func (r *migrate) confirmReady(ctx context.Context) error {
	for _, g := range r.sc.groups {
		for _, it := range g.items {
			if g.reason != "" {
				break
			}
			if !it.starts() {
				continue
			}
			c, err := r.inspectRetry(ctx, it.newID)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var fail error
			switch {
			case err != nil:
				fail = r.notReady(ctx, it, fmt.Sprintf("容器 %s 就绪之后读不到了：%v", it.m.Name, err), "")
			case phaseOf(c) == phaseStopped:
				fail = r.notReady(ctx, it, "容器 "+it.m.Name+" 就绪之后又停了（"+exitInfo(c)+"）", exitedHint(c))
			case phaseOf(c) == phaseWaiting:
				// 在重启、不健康：和等它就绪时一样接着等，从现在重新算时间。可能只是重启了一下。
				it.startedAt = time.Time{}
				if fail = r.waitReady(ctx, it); fail != nil && ctx.Err() != nil {
					return ctx.Err()
				}
			}
			if fail != nil {
				r.groupFailed(g, fail.Error(), groupHint(fail))
			}
		}
	}
	return nil
}

// inspectRetry 读容器的状态，Docker 偶尔回不了话时隔一秒重试，最多三次。
func (r *run) inspectRetry(ctx context.Context, id string) (*docker.Container, error) {
	var err error
	for i := 0; i < 3; i++ {
		var c *docker.Container
		if c, _, err = r.Docker.InspectContainer(docker.Quiet(ctx), id); err == nil || docker.IsNotFound(err) {
			return c, err
		}
		if sleep(ctx, time.Second) != nil {
			break
		}
	}
	return nil, err
}

// phase 是容器眼下处在哪个阶段：等它就绪（waitReady）和提交前复查（confirmReady）都按它判断。
type phase int

const (
	// phaseUp：在运行（健康检查没判它不健康），或者暂停着。
	phaseUp phase = iota + 1
	// phaseStopped：退出了（不管退出码是多少：原来在运行的，迁完自己退出了就是没成功，见 citem.want），或者坏了。
	phaseStopped
	// phaseWaiting：在重启、被判不健康、刚创建，要接着等。
	phaseWaiting
)

func phaseOf(c *docker.Container) phase {
	switch s := c.State.Status; {
	case s == "exited" || s == "dead":
		return phaseStopped
	case s == "paused", s == "running" && (c.State.Health == nil || c.State.Health.Status != "unhealthy"):
		return phaseUp
	}
	return phaseWaiting
}

// notReady 是容器没能就绪的报错。容器最后的输出写进日志，报错里接上最能说明问题的一行；
// 按名字找不到别的服务时，在建议里说明可能的原因。报错会显示在屏幕和结果里，所以先隐藏秘密。
func (r *run) notReady(ctx context.Context, it *citem, reason, hint string) error {
	logs := r.Redactor.Redact(r.Docker.ContainerLogs(ctx, it.newID, 100))
	if logs == "" {
		return &startError{reason, hint}
	}
	r.Log.Printf("容器 %s 最后的输出：\n%s", it.m.Name, logs)
	if l := keyLine(logs); l != "" {
		reason += "，最后的输出：" + l
	}
	if hint == "" {
		hint = unresolvedHint(it.m.Name, logs)
	}
	return &startError{reason, hint}
}

// exitedHint 是原来在运行的容器迁过来之后自己退出了时的建议：退出码是 0 的，多半是做完就退出的一次性任务。
func exitedHint(c *docker.Container) string {
	if c.State.ExitCode != 0 || c.State.OOMKilled {
		return ""
	}
	return "它在旧服务器上一直在运行，迁过来却自己做完退出了。如果它本来就是做完就退出的一次性任务，" +
		"等它在旧服务器上做完、停下之后再迁移（停着的容器迁过去照旧停着，不会启动）"
}

// keyLine 从容器的输出里挑出最能说明问题的一行：从后往前第一行带着出错字样的（error、
// exception、failed 之类），跳过报错时打印的调用栈（Java 的 at …、... 12 more，Python 的
// File "…"）；没有的话用最后一行。最多 160 个字。
func keyLine(logs string) string {
	lines := strings.Split(strings.TrimSpace(logs), "\n")
	last := ""
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" || stackFrame(l) {
			continue
		}
		if last == "" {
			last = l
		}
		if looksLikeError(l) {
			return clip(l, 160)
		}
	}
	return clip(last, 160)
}

func stackFrame(l string) bool {
	return strings.HasPrefix(l, "at ") || strings.HasPrefix(l, "...") || strings.HasPrefix(l, "…") || strings.HasPrefix(l, `File "`)
}

var errorWords = []string{"error", "exception", "fatal", "panic", "fail", "refused", "denied", "cannot", "can't",
	"could not", "couldn't", "unable", "not found", "no such", "not known", "nxdomain", "unknown host", "bad address",
	"timed out", "timeout", "invalid", "permission", "killed", "unreachable"}

func looksLikeError(l string) bool {
	low := strings.ToLower(l)
	for _, w := range errorWords {
		if strings.Contains(low, w) {
			return true
		}
	}
	return false
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// unresolvedPatterns 是各种程序“按名字找不到主机”时的报错，括号里是那个名字。
var unresolvedPatterns = []*regexp.Regexp{
	regexp.MustCompile(`Failed to resolve '([^'\s]+)'`),                                             // Netty（Halo 等 Java 程序）
	regexp.MustCompile(`(?m)UnknownHostException: ([A-Za-z0-9_.-]+)(?::|\s*$)`),                     // Java
	regexp.MustCompile(`getaddrinfo (?:ENOTFOUND|EAI_AGAIN) ([A-Za-z0-9_.-]+)`),                     // Node.js
	regexp.MustCompile(`could not translate host name "([^"]+)"`),                                   // PostgreSQL 客户端
	regexp.MustCompile(`lookup ([A-Za-z0-9_.-]+)(?: on \S+)?: (?:no such host|server misbehaving)`), // Go
	regexp.MustCompile(`bad address '([^'\s]+)'`),                                                   // busybox
	regexp.MustCompile(`Unknown (?:MySQL|MariaDB) server host '([^'\s]+)'`),                         // MySQL、MariaDB 客户端
	regexp.MustCompile(`getaddrinfo for ([A-Za-z0-9_.-]+) failed`),                                  // PHP
}

// unresolvedHost 是输出里最后一次“按名字找不到”的那个名字；没有时返回空。
func unresolvedHost(logs string) string {
	host, at := "", -1
	for _, re := range unresolvedPatterns {
		for _, m := range re.FindAllStringSubmatchIndex(logs, -1) {
			if m[0] > at {
				host, at = logs[m[2]:m[3]], m[0]
			}
		}
	}
	return host
}

// unresolvedHint 是容器按名字找不到别的服务时的建议。名字里有点的是外面的域名；没有点的是
// 同一台服务器上的服务（服务名、容器名）。
func unresolvedHint(name, logs string) string {
	host := unresolvedHost(logs)
	switch {
	case host == "":
		return ""
	case strings.Contains(host, "."):
		return fmt.Sprintf("容器 %s 解析不了域名 %s，检查新服务器的 DNS 和网络能不能访问它", name, host)
	}
	return fmt.Sprintf("容器 %s 按名字找不到它要连的 %s：这个服务没有一起迁过来、没能启动，或者不在同一个 Docker 网络里", name, host)
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
