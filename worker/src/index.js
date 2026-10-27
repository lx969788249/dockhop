// dockhop.run：一行命令 bash <(curl -fsSL dockhop.run) 用的短地址。
//
// curl、wget 来访问时，直接返回仓库 main 分支上的 run.sh（在 Cloudflare 上缓存 5 分钟）；
// 浏览器来访问时，跳到 GitHub 上的项目页。run.sh 会核对下载的程序的 SHA-256。

const SCRIPT = "https://raw.githubusercontent.com/lx969788249/dockhop/main/run.sh";
const REPO = "https://github.com/lx969788249/dockhop";

// 命令行工具：curl、wget、PowerShell 之类。其他（浏览器）一律跳到项目页。
const isCLI = (ua) => /^(curl|wget|fetch|libfetch|httpie|python-requests|go-http-client|powershell)/i.test(ua) || ua === "";

export default {
  async fetch(request) {
    const url = new URL(request.url);
    if (url.pathname !== "/" && url.pathname !== "/run.sh") {
      return Response.redirect(REPO, 302);
    }
    if (url.pathname === "/" && !isCLI(request.headers.get("user-agent") ?? "")) {
      return Response.redirect(REPO, 302);
    }
    const upstream = await fetch(SCRIPT, { cf: { cacheTtl: 300, cacheEverything: true } });
    if (!upstream.ok) {
      return new Response(`echo "下载安装脚本失败（GitHub 返回 ${upstream.status}），请稍后重试" >&2; exit 1\n`, {
        status: 502,
        headers: { "content-type": "text/plain; charset=utf-8" },
      });
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
