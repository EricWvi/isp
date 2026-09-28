# IP 池服务

按 [roadmap](docs/roadmap.md) 分阶段实现的单机 IP 池服务。包含配置与状态存储、Provider 选路、本地 SOCKS5 和 HTTP 代理入口、健康检查与自动故障切换，以及本地管理页面。

需要 Go 1.24+ 和 Node.js 20.19+。从源码运行任何 Go 命令前，先构建前端资源：

```sh
cd frontend
npm ci
npm run build
cd ..
```

复制 `config.example.yaml` 为自己的配置文件后，可先验证配置并初始化状态数据库：

```sh
go run ./cmd/isp config-check config.example.yaml
go run ./cmd/isp state-init config.example.yaml
```

`state-init` 会在当前工作目录下按 `server.database` 创建 SQLite 文件。配置中的 `provider.id` 和代理 `id` 必须是稳定的英文小写字母、数字、连字符；编辑代理连接信息时保留原 ID。代理 ID 在所有 Provider 中全局唯一。管理页面和代理入口都只允许监听本机；SOCKS5 和 HTTP 代理端口至少为 30000。前端产物会嵌入 Go 可执行文件；`frontend/dist` 不纳入版本控制。修改前端后，应重新运行 `npm --prefix frontend run build`，再构建或测试 Go 服务。

阶段 2 的选路模块位于 `internal/routing`。新连接可读取一次 `Snapshot()`，已确认不可用时会得到“无代理”；手动选择允许选择已启用但尚未确认健康的代理。自动候选只包含健康状态为 `healthy` 的代理，自动切换开关开启时不会立刻轮换。SOCKS5 和 HTTP 各有一套独立的当前选择与自动切换状态。选路行为可用 `go test ./internal/routing ./internal/provider` 无网络验证。

在 `providers[].proxies` 中加入真实上游 SOCKS5 代理后，可启动本地入口。服务会立即检测代理，并在首次找到健康代理时选择一个作为当前代理；也可在启动前手动指定：

```sh
go run ./cmd/isp select config.example.yaml proxy-seller your-proxy-id
go run ./cmd/isp auto-switch config.example.yaml on
# HTTP 池使用独立命令：
go run ./cmd/isp select-http config.example.yaml http-seller your-http-proxy-id
go run ./cmd/isp auto-switch-http config.example.yaml on
go run ./cmd/isp serve config.example.yaml
```

服务启动后打开 `http://127.0.0.1:38080/`（或配置的 `server.http_listen`）。页面会随系统设置自动切换亮色和暗色主题。页面支持添加、编辑、启停和删除代理，手动选择或轮换代理，并控制自动故障切换；代理变更会写回 YAML。页面和 API 只监听本机地址，且没有登录认证，请勿经公网或反向代理开放。直接编辑 YAML 后仍需重启服务；服务运行时也不要用独立的 `select`、`auto-switch` 命令修改同一数据库，避免运行内存与数据库状态不一致。

`server.socks5_enabled` 和 `server.http_proxy_enabled` 分别控制两个代理入口，可以都关、都开或只开一个。默认只开 SOCKS5（`127.0.0.1:30001`）；HTTP 代理默认关闭，启用后监听 `127.0.0.1:30002`。`server.http_listen` 始终是独立的管理页面地址，关闭两个代理入口也不影响页面和健康检查。修改监听开关或地址后需重启；开启的监听地址不能重复。

两套上游池在 `providers` 中用 `protocol: socks5 | http` 区分；省略 `protocol` 的旧配置仍属于 SOCKS5 池。HTTP 代理填在 `protocol: http` 的 Provider 下，使用该上游自己的主机、端口和可选用户名密码。两个池的健康检测、当前选择、手动轮换和自动故障切换互不影响，选择状态分别保存在 SQLite；已有数据库会自动增量升级，保留原 SOCKS5 选择。管理页面可分别操作两个池。

管理 API 位于 `/api/`，`GET /api/state` 返回代理与健康状态，但不返回密码；`udp_capability` 是配置值，`udp_status` 是包含本次运行观测结果的当前值。写请求需以 `/api/state` 中对应的 `config_revision` 或 `selection_revision` 作为带双引号的 `If-Match` 标头；版本过期时返回 `409`，页面会提示刷新。自动切换默认关闭。

入口接受本地 SOCKS5 无认证客户端的 TCP `CONNECT` 和 `UDP ASSOCIATE`，上游可无认证或使用用户名密码。客户端请求中的目标域名会交给上游解析；没有当前代理或当前代理确认不可用时会返回 SOCKS5 失败，不会直连目标。

HTTP 代理入口支持普通 `http://` 请求和 HTTPS 的 `CONNECT` 隧道，上游是 HTTP 池当前选中的 HTTP 代理，并由该上游解析目标域名。每个 HTTP 请求或隧道在建立时固定代理，切换只影响新请求；没有可用代理或上游连接失败时返回 `502`，不会直连目标。HTTP 代理同样没有客户端认证，因此只能在可信本机使用。启用后可验证：

```sh
curl --fail --show-error --proxy http://127.0.0.1:30002 https://example.com/
```

UDP 关联由一条控制 TCP 连接维持，关闭该连接或服务退出即关闭本地和上游 UDP relay；空闲超时默认 5 分钟，由网关的 `IdleTimeout` 控制。每个关联在建立时固定当前代理，切换只影响新关联。客户端必须把 SOCKS5 UDP 封装报文发到 `UDP ASSOCIATE` 响应中的本地地址和端口；分片报文（`FRAG != 0`）会被丢弃。仅接受与控制连接同 IP 的客户端 UDP 报文，并固定第一个有效报文的源端口。上游响应中的 `0.0.0.0`／`::` relay 地址按其控制连接的对端 IP 处理。

代理配置可设置 `udp_capability: unknown | supported | unsupported`，省略时为 `unknown`。标记 `unsupported` 的代理收到 UDP 请求时直接返回 SOCKS5 状态 7，不尝试上游；未知代理会尝试上游 `UDP ASSOCIATE`，若上游返回状态 7，则运行期间记住“不支持”，管理页面会显示该观测结果。修改代理连接配置后会重新判断；观测结果不跨重启持久化。这个标识不影响 TCP 健康检查或 TCP 转发。UDP 请求失败不会回退到本机直连。当前自动化测试覆盖 DNS 格式报文、1200 字节的 QUIC 最小尺寸载荷和一般 UDP 报文的透明转发；未覆盖完整 QUIC 握手或公网环境。

健康检查经各自协议的上游访问 `health_check.url`。任何 HTTP 状态码都表示代理链路可用；默认连续失败 3 次才标记不可用。正常检查间隔、超时、失败阈值和退避上限均由 YAML 控制，状态与下次检查时间保存在 SQLite。上游连接错误会提前安排复检，不会直接增加失败计数。自动切换开启后，只有该池的当前代理确认不可用才会切换。

## 发布与运行

在 Linux 或 macOS 上安装 Go 1.24+、Node.js 20.19+ 和 npm 后，从仓库根目录执行：

```sh
bash scripts/release.sh
./release/isp config-check ./config.yaml
./release/isp serve ./config.yaml
```

发布脚本使用锁定的 npm 依赖先重建 `frontend/dist`，运行 Go 测试，再以 `CGO_ENABLED=0`、`-trimpath` 和固定空 build ID 编译 `release/isp`。`isp --version` 输出构建版本：发布脚本写入 `git describe --tags` 的结果，GitHub Release 写入对应 tag，直接 `go build` 或 `go run` 则显示 `unknown`。发布时只需复制该可执行文件、自己的 YAML 配置和 SQLite 状态文件；运行时不需要 Node.js 或单独的前端目录。脚本按当前主机系统与架构编译，Linux 和 macOS 需分别执行。服务以 JSON 日志输出到标准错误；正常收到 SIGINT/SIGTERM 时停止接受新连接、关闭活动 SOCKS5 连接、等待 HTTP 请求退出并关闭数据库。管理配置已写入 YAML、但运行时更新失败时会报错退出，应先检查日志并重启以重新加载 YAML。

推送匹配 `v*` 的 tag 后，GitHub Actions 会重建前端、运行 Go 测试，并在同名 GitHub Release 中发布两个 Linux 可执行文件：`isp-proxy-linux-amd64` 和 `isp-proxy-linux-arm64`。它们已嵌入管理页面，无需在目标机器安装 Go 或 Node.js。

### systemd 系统服务（Linux）

以运行服务的普通用户登录服务器，执行以下命令安装或更新最新 GitHub Release：

```sh
curl -fsSL https://raw.githubusercontent.com/EricWvi/isp/main/scripts/install.sh | bash
```

脚本自动选择 `amd64` 或 `arm64`，首次安装时创建程序、配置和数据目录，写入空代理池的默认配置，并通过 `sudo systemctl` 安装、启用和启动系统服务。服务进程仍以执行安装脚本的普通用户身份运行，不需要 linger 或 systemd 用户管理器。新安装默认只监听本机管理页面 `127.0.0.1:38080`，SOCKS5 和 HTTP 两个代理入口均关闭。再次执行会保留配置与数据库，先停止正在运行的服务，替换程序后重新启动；原本未运行的服务仍保持停止。已有安装如需使用新管理端口，请手动将 `~/.config/isp-proxy/config.yaml` 中的 `server.http_listen` 改为 `127.0.0.1:38080`，再运行 `sudo systemctl restart isp-proxy.service`。首次安装后请编辑配置，填写上游代理并按需将 `server.socks5_enabled`、`server.http_proxy_enabled` 改为 `true`，再重启服务。日志可通过 `sudo journalctl -u isp-proxy.service -f` 查看。

服务模板为 [packaging/isp-proxy.service.in](packaging/isp-proxy.service.in)，安装到 `/etc/systemd/system/isp-proxy.service`；每次更新会覆盖该服务文件。若检测到以前安装的用户服务，脚本会先停用，再迁移到系统服务。工作目录固定为 `~/.local/share/isp-proxy`，默认配置中的 `server.database: ./data/isp.db` 会写入该目录下的 `data/isp.db`。如需安装指定版本，可使用 `curl -fsSL https://raw.githubusercontent.com/EricWvi/isp/main/scripts/install.sh | ISP_PROXY_VERSION=v0.1.1 bash`，把版本号换成已发布的 tag。

配置文件可能包含上游密码，建议权限为 `0600`，且不要提交真实凭据。`server.database` 的相对路径相对于启动时的工作目录；使用服务管理器时请固定工作目录，或在 YAML 中使用绝对路径。监听地址只允许本机地址，不要把无认证的管理页面转发到公网。启动前可用 `config-check` 校验 YAML；若 SQLite 文件损坏，服务会报错而不会自动清空它。

备份前优雅停止服务，再把 YAML 和 SQLite 数据库文件作为同一份快照复制保存。SQLite 使用 WAL 模式；如果必须在线备份，应使用 SQLite 在线备份 API，不能只复制运行中的 `.db` 文件而忽略 `-wal`。恢复时也先停止服务，保留故障现场副本，再将同一份备份中的 YAML 和数据库放回原路径，运行 `config-check` 后启动。若仅恢复 YAML 而不恢复数据库，运行时选择与健康状态可能对应不上当前代理清单；服务会按稳定 ID 重新关联，无法关联的旧状态不会参与选路。健康状态或自动切换结果写入 SQLite 失败时只记录错误日志，健康检查和代理入口继续按内存状态运行；磁盘写满时应先释放空间并检查日志，再重启服务确认持久化状态。

可用以下命令验证本地 SOCKS5 客户端（先在 YAML 中填入可用上游并等待健康检查成功）：

```sh
curl --fail --show-error --socks5-hostname 127.0.0.1:30001 https://example.com/
```

`--socks5-hostname` 使域名由上游解析。自动化测试也会在安装了 curl 的系统上覆盖无认证和用户名密码认证两种上游。macOS 上仍需运行发布脚本和该客户端命令做实际系统验收。
