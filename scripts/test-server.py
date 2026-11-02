#!/usr/bin/env python3
# 测试 run.sh 用的下载服务（见 test-run-sh.sh）：python3 scripts/test-server.py 端口 目录
#
# 和 python3 -m http.server 一样提供目录里的文件，另外支持接着下载（Range，像 GitHub），并按地址的
# 第一段模拟不同的下载地址，几种可以用 - 连起来（例如 /norange-slow/…）：
#   slow     每秒只发 500KB
#   cut      发到一半就断开连接
#   norange  不支持接着下载，总是从头发（像 Gitee）
# 每个请求除了常规的日志，还记一行“range 路径 Range 请求头”。
import os
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

root = sys.argv[2]


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        flags = self.path.split("/")[1].split("-")
        path = os.path.join(root, self.path.lstrip("/"))
        rng = self.headers.get("Range", "")
        self.log_message("range %s %s", self.path, rng or "-")
        if not os.path.isfile(path):
            self.send_error(404)
            return
        with open(path, "rb") as f:
            data = f.read()
        start = 0
        if rng.startswith("bytes=") and "norange" not in flags:
            start = int(rng[len("bytes="):].split("-")[0])
        if start:
            self.send_response(206)
            self.send_header("Content-Range", "bytes %d-%d/%d" % (start, len(data) - 1, len(data)))
        else:
            self.send_response(200)
        self.send_header("Content-Length", str(len(data) - start))
        self.end_headers()
        body = data[start:]
        if "cut" in flags:
            body = body[: max(len(data) // 2 - start, 0)]
        step = 50000 if "slow" in flags else max(len(body), 1)
        try:
            for i in range(0, len(body), step):
                self.wfile.write(body[i : i + step])
                if "slow" in flags:
                    time.sleep(0.1)
        except (BrokenPipeError, ConnectionResetError):
            pass


ThreadingHTTPServer.daemon_threads = True
ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
