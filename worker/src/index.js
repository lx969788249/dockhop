// dockhop.run：一行命令 bash <(curl -fsSL https://dockhop.run) 用的短地址。
//
// curl、wget 来访问时，直接返回仓库 main 分支上的 run.sh（在 Cloudflare 上缓存 5 分钟）；
// 浏览器来访问时，跳到 GitHub 上的项目页。
//
// /v版本号/dockhop-linux-架构[.gz] 是 GitHub Release 里的程序，由这里转发并缓存在
// Cloudflare 上：国内服务器从 GitHub 下载常常很慢甚至断开。run.sh 会核对下载的程序的
// SHA-256，这里转发的内容就算出了问题也不会被运行。

const SCRIPT = "https://raw.githubusercontent.com/lx969788249/dockhop/main/run.sh";
const REPO = "https://github.com/lx969788249/dockhop";
const RELEASES = "https://github.com/lx969788249/dockhop/releases/download";

// 只转发这几个文件，不能拿来下载别的东西。
const ASSET = /^\/v(\d+\.\d+\.\d+)\/(dockhop-linux-(?:amd64|arm64|armv7)(?:\.gz)?)$/;

// 命令行工具：curl、wget、PowerShell 之类。其他（浏览器）一律跳到项目页。
const isCLI = (ua) => /^(curl|wget|fetch|libfetch|httpie|python-requests|go-http-client|powershell)/i.test(ua) || ua === "";

const text = (body, status) =>
  new Response(body, { status, headers: { "content-type": "text/plain; charset=utf-8", "cache-control": "no-store" } });

export default {
  async fetch(request, env, ctx) {
    const url = new URL(request.url);
    // 明文 HTTP 不返回脚本：路上的人可以改掉它，而它是以 root 运行的，程序的校验值也写在它里面。
    // 不写 https:// 的老命令会先走 HTTP，跳到 https 之后 curl -L 会跟过去。
    if (url.protocol === "http:") {
      url.protocol = "https:";
      return Response.redirect(url.toString(), 301);
    }
    const asset = url.pathname.match(ASSET);
    if (asset) {
      return release(request, ctx, asset[1], asset[2]);
    }
    if (url.pathname !== "/" && url.pathname !== "/run.sh") {
      return Response.redirect(REPO, 302);
    }
    if (url.pathname === "/" && !isCLI(request.headers.get("user-agent") ?? "")) {
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

// release 返回某个版本的程序。发布后的文件不会再变，第一次从 GitHub 取来后就一直缓存。
async function release(request, ctx, version, name) {
  if (request.method !== "GET" && request.method !== "HEAD") {
    return text("只支持下载\n", 405);
  }
  const cache = caches.default;
  const key = new Request(`https://dockhop.run/v${version}/${name}`);
  const hit = await cache.match(key);
  if (hit) {
    return request.method === "HEAD" ? new Response(null, hit) : hit;
  }
  const upstream = await fetch(`${RELEASES}/v${version}/${name}`, { redirect: "follow" });
  if (!upstream.ok) {
    // 没有这个版本或这个文件（例如老版本没有 .gz）时如实返回 404，run.sh 据此改下载别的文件。
    const status = upstream.status === 404 ? 404 : 502;
    return text(`没有找到 v${version}/${name}（GitHub 返回 ${upstream.status}）\n`, status);
  }
  const res = new Response(upstream.body, upstream);
  for (const h of [...res.headers.keys()]) {
    if (h !== "content-length") {
      res.headers.delete(h);
    }
  }
  res.headers.set("content-type", "application/octet-stream");
  res.headers.set("cache-control", "public, max-age=31536000, immutable");
  ctx.waitUntil(cache.put(key, res.clone()));
  return request.method === "HEAD" ? new Response(null, res) : res;
}
