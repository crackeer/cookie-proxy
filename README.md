# cookie-proxy

用 Cookie 区分后端的反向代理：Cookie `proxy_name` 的值对应配置里的 `proxy_list[].name`，
请求被转发到该条目的 `proxy_pass`。浏览器打开 `/_select` 就能在页面上挑选后端。

- 一个入口端口，多个后端，靠一个 Cookie 切换
- 自带选择页 `/_select`：列出全部后端，点一下写入 Cookie
- 没选择时不会把请求发给任何后端（浏览器跳到选择页，其它客户端拿 403）
- 基于 Gin，每个请求打印一条访问日志；日志不含 Cookie 原文与 `Authorization` 头

> 注意：本服务不再做任何身份认证。凡是能访问这个端口的人都能选择任意后端，
> 因此它只适合放在可信网络里，或者在前面串一层做认证的网关。

## 构建与运行

```bash
go build -o cookie-proxy .
./cookie-proxy -c config.json
```

开发期可以直接 `go run . -c config.json`。

## 打包与部署（systemd）

`build.sh` 会把项目交叉编译成 Linux 静态二进制，并打成可直接安装为 systemd 服务的 tar.gz 包。
包内包含二进制、默认配置、`cookie-proxy.service`、`install.sh` 和版本号文件。

```bash
./build.sh                        # 默认 linux/amd64，版本号取自 git describe
./build.sh -a arm64               # 指定目标架构
./build.sh -v 1.2.3 -o dist       # 指定版本号与输出目录
```

| 选项 | 说明 |
| --- | --- |
| `-a, --arch ARCH` | 目标架构，如 `amd64`/`x86_64`、`arm64`/`aarch64`、`arm`、`386`、`riscv64`（默认 `amd64`） |
| `-v, --version VER` | 显式指定版本号（默认取自 `git describe`） |
| `-o, --output DIR` | 输出目录（默认 `dist`） |
| `-h, --help` | 显示帮助 |

产物形如 `dist/cookie-proxy-1.2.3-linux-amd64.tar.gz`（附同名 `.sha256` 校验文件）。
拿到目标 Linux 机器后解包并安装：

```bash
tar -xzf cookie-proxy-1.2.3-linux-amd64.tar.gz
cd cookie-proxy-1.2.3-linux-amd64
sudo ./install.sh                # 装二进制/配置/unit 并 enable --now
```

`install.sh` 把二进制装到 `/usr/local/bin`、配置装到 `/etc/cookie-proxy/config.json`、unit 装到
`/etc/systemd/system`，并以专用系统用户 `cookie-proxy` 运行服务。重复执行即升级：已有的
`config.json` 不会被覆盖，新配置会另存为 `config.json.new`。卸载用 `sudo ./install.sh --uninstall`
（会停止服务并删除安装的文件与专用用户）。常用运维命令：

```bash
systemctl status cookie-proxy       # 查看状态
journalctl -u cookie-proxy -f       # 跟踪日志（日志写 stdout，systemd 会收集）
sudo systemctl restart cookie-proxy # 改完 /etc/cookie-proxy/config.json 后重启
```

## 配置

`-c` 是必填参数，指向一个 JSON 文件：

```json
{
  "port": 8888,
  "proxy_list": [
    {
      "name": "openclacky",
      "proxy_pass": "http://10.33.207.152:7500"
    },
    {
      "name": "deepseek",
      "proxy_pass": "http://10.33.207.152:7501"
    },
    {
      "name": "download",
      "proxy_pass": "http://10.33.207.152:7502"
    }
  ]
}
```

| 字段 | 必填 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `port` | 是 | — | 监听端口，1–65535 |
| `proxy_list[].name` | 是 | — | Cookie `proxy_name` 的取值，不可重复；只允许字母、数字、`.` `-` `_` |
| `proxy_list[].proxy_pass` | 是 | — | 后端地址，必须是 `http://` 或 `https://` 开头的绝对地址，可带路径前缀 |
| `upstream_timeout_seconds` | 否 | `30` | 转发超时；`0` 或负数表示不限制。协议升级请求（WebSocket）不受此限制 |
| `shutdown_timeout_seconds` | 否 | `10` | 优雅退出的宽限期 |
| `log_format` | 否 | `"text"` | `"text"` 或 `"json"` |

配置在启动时一次性校验，所有问题会一起报出来。未识别的字段会直接报错（防止 `proxypass`
这类拼写错误被静默忽略），旧版的 `users` 配置也会因此直接报错。

`proxy_pass` 带路径前缀时会与请求路径拼接：配置 `http://backend/base`，请求 `/x?q=1` → 后端收到 `/base/x?q=1`。

## 使用

浏览器：直接访问 `http://127.0.0.1:8888/` → 被带到 `/_select` → 选一个后端 → 自动跳回原来的地址。
之后所有请求都走这个后端，想换后端再打开 `/_select` 即可。

命令行：自己带上 Cookie。

```bash
# 命中 openclacky 对应的后端
curl -b 'proxy_name=openclacky' http://127.0.0.1:8888/hello.txt

# 没带 Cookie → 403
curl -i http://127.0.0.1:8888/hello.txt

# 浏览器式请求没带 Cookie → 302 到 /_select，并用 next 记住原地址
curl -i -H 'Accept: text/html' http://127.0.0.1:8888/hello.txt
```

### 选择页 `/_select`

- `GET /_select` 列出 `proxy_list` 的全部条目（名称 + 后端地址），当前选择会被标出
- `POST /_select`（表单字段 `name`、可选 `next`）写入 Cookie，然后 303 跳到 `next`
- `next` 只接受本站绝对路径，外部地址与 `/_select` 自身都会被换成 `/`
- 该路径由本服务处理，不会转发给后端；`GET`/`HEAD`/`POST` 之外的方法返回 405

Cookie 属性：`Path=/`、`Max-Age` 30 天、`HttpOnly`、`SameSite=Lax`，仅在 HTTPS 连接上附加 `Secure`。
Cookie 的值在配置里不存在时（比如后端被删了），会被清除并重新引导到选择页。

访问日志（`text` 格式）：

```
time=2026-09-21T17:45:50.506+08:00 level=INFO msg=access client_ip=127.0.0.1 method=GET \
  path=/hello.txt status=200 duration_ms=2.046 bytes=10 proxy=alpha upstream=http://127.0.0.1:7601
time=2026-09-21T17:44:27.952+08:00 level=INFO msg=access client_ip=127.0.0.1 method=GET \
  path=/hello.txt status=403 duration_ms=0.051 bytes=129 proxy=- upstream="" reason=no_selection
```

`reason` 只在未放行时出现，取值：`no_selection`（没带 Cookie）、`unknown_proxy_name`（名称不在配置里）。
日志写标准输出，可直接交给容器或 systemd 收集。

## 状态码

| 状态码 | 含义 |
| --- | --- |
| 302 | 浏览器请求缺少有效选择，跳转到 `/_select` |
| 303 | 选择成功，跳回 `next` |
| 403 | 非浏览器请求缺少有效选择；响应体说明怎么带 Cookie |
| 400 | `POST /_select` 提交了不存在的名称，重新展示列表 |
| 405 | 用 `GET`/`HEAD`/`POST` 之外的方法访问 `/_select` |
| 502 | 后端不可达（连接被拒、DNS 失败、TLS 错误等） |
| 504 | 后端响应超过 `upstream_timeout_seconds` |
| 500 | 服务内部异常（handler panic 已被恢复） |
| 其他 | 后端返回的状态码原样透传 |

## 转发行为

- 方法、路径、查询串、请求头、请求体原样转发；响应状态码、响应头、响应体原样回传
- `Host` 设为后端主机
- 设置 `X-Forwarded-For` / `X-Forwarded-Host` / `X-Forwarded-Proto`，入站的同名头不被信任
- `Authorization` 头原样透传给后端（本服务自己不再使用它）
- 转发前从 `Cookie` 头里摘掉 `proxy_name`，其它 Cookie 原样保留
- 支持 WebSocket 升级与 SSE/分块流式响应（立即 flush）
- 不做 307/301 路径改写：`/foo/` 这类尾斜杠路径原样转发

## 进程生命周期

- 端口被占用、配置非法、配置文件读不到 → 打印错误并以非零退出码结束（缺 `-c` 退出码 2，其余为 1）
- 收到 `SIGINT` / `SIGTERM` → 停止接收新连接，等待进行中的请求完成后退出；超过宽限期则强制关闭并打警告

## 不支持的场景

- 身份认证：本服务只做选路，不校验任何凭据
- 正向代理（`CONNECT`）：这是 ProxyPass 风格的反向代理
- 配置热加载：改配置需重启
- 一个名称多后端 / 负载均衡
- 后端占用 `/_select` 这个路径（它被本服务截获）
- TLS 终止：需要 HTTPS 时在前面放 nginx 或 ALB
- `client_ip` 取真实对端地址，不解析入站 `X-Forwarded-For`；若本服务部署在别的反向代理之后，日志里的 IP 会是那个代理的地址

## 测试

```bash
go test ./...
go test -race ./...
```

## 项目结构

```
build.sh                  # 交叉编译并打包为 systemd 安装包（含 unit 与 install.sh）
main.go                    # flag 解析、中间件装配、监听与优雅退出
internal/config            # 配置加载与校验
internal/logging           # slog 日志器与访问日志中间件
internal/selector          # Cookie 选路中间件与 /_select 选择页
internal/proxy             # 名称 → 后端路由表与请求转发
```
