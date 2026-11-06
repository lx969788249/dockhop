// dockhop.run：一行命令 bash <(curl -fsSL https://dockhop.run) 用的短地址。
//
// curl、wget 来访问时，直接返回仓库 main 分支上的 run.sh（在 Cloudflare 上缓存 5 分钟），并填上
// 成功完成迁移的次数和先从哪里下载程序；浏览器来访问时，跳到 GitHub 上的项目页。程序本身由 run.sh
// 从 Gitee 或 GitHub 下载，不经过这里。
//
// /count 是成功完成迁移的次数：程序开头的菜单显示它（通过一行命令运行时用 run.sh 里填好的，直接
// 运行程序时来这里读）；迁入成功时程序发一个空的 POST，次数加 1。/failed 是迁移失败的次数：迁出、
// 迁入失败时程序发一个空的 POST，次数加 1。/stats 显示这两个次数和成功率，浏览器、命令行都能看。
// D1 里只存这两个数字，不记录任何别的东西。

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
    if (url.pathname === "/failed" && request.method === "POST") {
      return bump(request, env, "failed");
    }
    if (url.pathname === "/stats") {
      return stats(env);
    }
    const cli = isCLI(request.headers.get("user-agent") ?? "");
    if (url.pathname !== "/" && url.pathname !== "/run.sh") {
      // 以前版本的 run.sh 会先来这里下载程序（现在不转发了）：命令行返回 404，它就马上改从 GitHub 下载。
      return cli ? text("没有这个地址\n", 404) : Response.redirect(REPO, 302);
    }
    if (url.pathname === "/" && !cli) {
      return Response.redirect(REPO, 302);
    }
    // 次数和脚本一起读，填进脚本里预留的 COUNT="" 这一行，程序开头的菜单直接显示，不用再来问
    // 一次（从国内访问这里常常要好几秒）。1 秒内读不到就不填，程序会自己再读。
    const [upstream, n] = await Promise.all([
      fetch(SCRIPT, { cf: { cacheTtl: 300, cacheEverything: true } }),
      within(readCount(env), 1000),
    ]);
    if (!upstream.ok) {
      return text(`echo "下载安装脚本失败（GitHub 返回 ${upstream.status}），请稍后重试" >&2; exit 1\n`, 502);
    }
    let script = await upstream.text();
    if (n !== null) {
      script = script.replace(/^COUNT=""$/m, `COUNT="${n}"`);
    }
    // 先从哪里下载程序：中国大陆先用 Gitee，其他地方（包括香港、澳门、台湾）先用 GitHub，省得
    // run.sh 先花 2 秒比速度。国家是 Cloudflare 本来就给每个请求标好的，不另外查，也不记录；
    // 不知道（XX）或者经过 Tor（T1）时不填，run.sh 照旧比一比。
    const country = request.cf?.country ?? "";
    const prefer = country === "CN" ? "gitee" : /^[A-Z]{2}$/.test(country) && country !== "XX" ? "github" : "";
    if (prefer) {
      script = script.replace(/^PREFER=""$/m, `PREFER="${prefer}"`);
    }
    return new Response(script, {
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
  if (request.method !== "POST") {
    const n = await readCount(env);
    return n === null ? text("暂时读不到\n", 503) : text(`${n}\n`, 200);
  }
  return bump(request, env, "n");
}

// bump 把一个次数加 1（column 是 n：成功完成迁移的次数，或者 failed：迁移失败的次数）。出了问题
// （例如免费额度用完）返回 503，程序不管结果。
async function bump(request, env, column) {
  try {
    // 只认 DockHop 发来的；同一个地址一分钟最多算 5 次（成功、失败合计），防止有人刷数字。
    const ua = request.headers.get("user-agent") ?? "";
    const ip = request.headers.get("cf-connecting-ip") ?? "";
    if (ua.startsWith("DockHop/") && (await env.LIMIT.limit({ key: ip })).success) {
      await env.DB.prepare(`UPDATE counter SET ${column} = ${column} + 1`).run();
    }
    return new Response(null, { status: 204 });
  } catch {
    return text("暂时记不了\n", 503);
  }
}

// stats 显示成功、失败的次数和成功率（最多保留一位小数；还没有记录时不算成功率）。出了问题
// （例如免费额度用完）返回 503。
async function stats(env) {
  try {
    const row = await env.DB.prepare("SELECT n, failed FROM counter").first();
    const n = row?.n ?? 0;
    const failed = row?.failed ?? 0;
    let body = `成功：${n} 次\n失败：${failed} 次\n`;
    if (n + failed > 0) {
      body += `成功率：${Math.round((1000 * n) / (n + failed)) / 10}%\n`;
    }
    return text(body, 200);
  } catch {
    return text("暂时读不到\n", 503);
  }
}

// readCount 读出成功完成迁移的次数；出了问题时为 null。
async function readCount(env) {
  try {
    const row = await env.DB.prepare("SELECT n FROM counter").first();
    const n = row?.n ?? 0;
    return Number.isSafeInteger(n) && n >= 0 ? n : null;
  } catch {
    return null;
  }
}

// within 等 p 最多 ms 毫秒，到时还没有结果时为 null。
function within(p, ms) {
  let timer;
  const late = new Promise((resolve) => (timer = setTimeout(() => resolve(null), ms)));
  return Promise.race([p, late]).finally(() => clearTimeout(timer));
}
