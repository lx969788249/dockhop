// dockhop.run：一行命令 bash <(curl -fsSL https://dockhop.run) 用的短地址。
//
// curl、wget 来访问时，直接返回仓库 main 分支上的 run.sh（在 Cloudflare 上缓存 5 分钟）；
// 浏览器来访问时，跳到 GitHub 上的项目页。程序本身由 run.sh 从 Gitee 或 GitHub 下载，不经过这里。
//
// /count 是成功完成迁移的次数（D1 里只存这一个数字）：程序开头的菜单读出来显示；迁入
// 成功时程序发一个空的 POST，次数加 1。不记录任何别的东西。

const SCRIPT = "https://raw.githubusercontent.com/lx969788249/dockhop/main/run.sh";
const REPO = "https://github.com/lx969788249/dockhop";

// 命令行工具：curl、wget、PowerShell 之类。其他（浏览器）一律跳到项目页。
const isCLI = (ua) => /^(curl|wget|fetch|libfetch|httpie|python-requests|go-http-client|powershell)/i.test(ua) || ua === "";

const text = (body, status) =>
  new Response(body, { status, headers: { "content-type": "text/plain; charset=utf-8", "cache-control": "no-store" } });

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    // 明文 HTTP 不返回脚本：路上的人可以改掉它，而它是以 root 运行的，程序的校验值也写在它里面。
    // 不写 https:// 的老命令会先走 HTTP，跳到 https 之后 curl -L 会跟过去。
    if (url.protocol === "http:") {
      url.protocol = "https:";
      return Response.redirect(url.toString(), 301);
    }
    if (url.pathname === "/count") {
      return count(request, env);
    }
    const cli = isCLI(request.headers.get("user-agent") ?? "");
    if (url.pathname !== "/" && url.pathname !== "/run.sh") {
      // 以前版本的 run.sh 会先来这里下载程序（现在不转发了）：命令行返回 404，它就马上改从 GitHub 下载。
      return cli ? text("没有这个地址\n", 404) : Response.redirect(REPO, 302);
    }
    if (url.pathname === "/" && !cli) {
      return Response.redirect(REPO, 302);
    }
    const upstream = await fetch(SCRIPT, { cf: { cacheTtl: 300, cacheEverything: true } });
    if (!upstream.ok) {
      return text(`echo "下载安装脚本失败（GitHub 返回 ${upstream.status}），请稍后重试" >&2; exit 1\n`, 502);
    }
    return new Response(upstream.body, {
      headers: {
        "content-type": "text/plain; charset=utf-8",
        "cache-control": "public, max-age=300",
        "x-content-type-options": "nosniff",
      },
    });
  },
};

// count 读出或增加成功完成迁移的次数。出了问题（例如免费额度用完）返回 503，程序会当作
// 读不到：菜单里不显示这一行，迁移照常进行。
async function count(request, env) {
  try {
    if (request.method === "POST") {
      // 只认 DockHop 发来的；同一个地址一分钟最多算 5 次，防止有人刷数字。
      const ua = request.headers.get("user-agent") ?? "";
      const ip = request.headers.get("cf-connecting-ip") ?? "";
      if (ua.startsWith("DockHop/") && (await env.LIMIT.limit({ key: ip })).success) {
        await env.DB.prepare("UPDATE counter SET n = n + 1").run();
      }
      return new Response(null, { status: 204 });
    }
    const row = await env.DB.prepare("SELECT n FROM counter").first();
    return text(`${row?.n ?? 0}\n`, 200);
  } catch {
    return text("暂时读不到\n", 503);
  }
}
