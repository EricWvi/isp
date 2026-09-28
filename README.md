# IP 池服务

按 [roadmap](docs/roadmap.md) 分阶段实现的单机 IP 池服务。包含配置与状态存储、Provider 选路、本地 SOCKS5 TCP `CONNECT`、健康检查与自动故障切换，以及本地管理页面。

需要 Go 1.24+ 和 Node.js 20.19+。复制 `config.example.yaml` 为自己的配置文件后，可先验证配置并初始化状态数据库：

```sh
go run ./cmd/isp config-check config.example.yaml
go run ./cmd/isp state-init config.example.yaml
```

`state-init` 会在当前工作目录下按 `server.database` 创建 SQLite 文件。配置中的 `provider.id` 和代理 `id` 必须是稳定的英文小写字母、数字、连字符；编辑代理连接信息时保留原 ID。代理 ID 在所有 Provider 中全局唯一。HTTP 与 SOCKS5 监听地址限本机，SOCKS5 端口至少为 30000。

前端工程位于 `frontend/`：

```sh
cd frontend
npm ci
npm run build
```

前端产物会嵌入 Go 可执行文件；修改前端后，应先运行 `npm run build`，再构建或运行 Go 服务。仓库包含预构建的 `frontend/dist`，因此仅运行 Go 测试不需要安装 Node.js。

阶段 2 的选路模块位于 `internal/routing`。新连接可读取一次 `Snapshot()`，已确认不可用时会得到“无代理”；手动选择允许选择已启用但尚未确认健康的代理。自动候选只包含健康状态为 `healthy` 的代理，自动切换开关开启时不会立刻轮换。选路行为可用 `go test ./internal/routing ./internal/provider` 无网络验证。

在 `providers[].proxies` 中加入真实上游 SOCKS5 代理后，可启动本地入口。服务会立即检测代理，并在首次找到健康代理时选择一个作为当前代理；也可在启动前手动指定：

```sh
go run ./cmd/isp select config.example.yaml proxy-seller your-proxy-id
go run ./cmd/isp auto-switch config.example.yaml on
go run ./cmd/isp serve config.example.yaml
```

服务启动后打开 `http://127.0.0.1:8080/`（或配置的 `server.http_listen`）。页面支持添加、编辑、启停和删除代理，手动选择或轮换代理，并控制自动故障切换；代理变更会写回 YAML。页面和 API 只监听本机地址，且没有登录认证，请勿经公网或反向代理开放。直接编辑 YAML 后仍需重启服务；服务运行时也不要用独立的 `select`、`auto-switch` 命令修改同一数据库，避免运行内存与数据库状态不一致。

管理 API 位于 `/api/`，`GET /api/state` 返回代理与健康状态，但不返回密码；`udp_capability` 是配置值，`udp_status` 是包含本次运行观测结果的当前值。写请求需以 `/api/state` 中对应的 `config_revision` 或 `selection_revision` 作为带双引号的 `If-Match` 标头；版本过期时返回 `409`，页面会提示刷新。自动切换默认关闭。

入口接受本地 SOCKS5 无认证客户端的 TCP `CONNECT` 和 `UDP ASSOCIATE`，上游可无认证或使用用户名密码。客户端请求中的目标域名会交给上游解析；没有当前代理或当前代理确认不可用时会返回 SOCKS5 失败，不会直连目标。

UDP 关联由一条控制 TCP 连接维持，关闭该连接或服务退出即关闭本地和上游 UDP relay；空闲超时默认 5 分钟，由网关的 `IdleTimeout` 控制。每个关联在建立时固定当前代理，切换只影响新关联。客户端必须把 SOCKS5 UDP 封装报文发到 `UDP ASSOCIATE` 响应中的本地地址和端口；分片报文（`FRAG != 0`）会被丢弃。仅接受与控制连接同 IP 的客户端 UDP 报文，并固定第一个有效报文的源端口。上游响应中的 `0.0.0.0`／`::` relay 地址按其控制连接的对端 IP 处理。

代理配置可设置 `udp_capability: unknown | supported | unsupported`，省略时为 `unknown`。标记 `unsupported` 的代理收到 UDP 请求时直接返回 SOCKS5 状态 7，不尝试上游；未知代理会尝试上游 `UDP ASSOCIATE`，若上游返回状态 7，则运行期间记住“不支持”，管理页面会显示该观测结果。修改代理连接配置后会重新判断；观测结果不跨重启持久化。这个标识不影响 TCP 健康检查或 TCP 转发。UDP 请求失败不会回退到本机直连。当前自动化测试覆盖 DNS 格式报文、1200 字节的 QUIC 最小尺寸载荷和一般 UDP 报文的透明转发；未覆盖完整 QUIC 握手或公网环境。

健康检查经各自的 SOCKS5 上游访问 `health_check.url`。任何 HTTP 状态码都表示代理链路可用；默认连续失败 3 次才标记不可用。正常检查间隔、超时、失败阈值和退避上限均由 YAML 控制，状态与下次检查时间保存在 SQLite。SOCKS 上游连接错误会提前安排复检，不会直接增加失败计数。自动切换开启后，只有当前代理确认不可用才会切换。

## 发布与运行

在 Linux 或 macOS 上安装 Go 1.24+、Node.js 20.19+ 和 npm 后，从仓库根目录执行：

```sh
bash scripts/release.sh
./release/isp config-check ./config.yaml
./release/isp serve ./config.yaml
```

发布脚本使用锁定的 npm 依赖先重建 `frontend/dist`，运行 Go 测试，再以 `CGO_ENABLED=0`、`-trimpath` 和固定空 build ID 编译 `release/isp`。发布时只需复制该可执行文件、自己的 YAML 配置和 SQLite 状态文件；运行时不需要 Node.js 或单独的前端目录。脚本按当前主机系统与架构编译，Linux 和 macOS 需分别执行。服务以 JSON 日志输出到标准错误；正常收到 SIGINT/SIGTERM 时停止接受新连接、关闭活动 SOCKS5 连接、等待 HTTP 请求退出并关闭数据库。管理配置已写入 YAML、但运行时更新失败时会报错退出，应先检查日志并重启以重新加载 YAML。

配置文件可能包含上游密码，建议权限为 `0600`，且不要提交真实凭据。`server.database` 的相对路径相对于启动时的工作目录；使用服务管理器时请固定工作目录，或在 YAML 中使用绝对路径。监听地址只允许本机地址，不要把无认证的管理页面转发到公网。启动前可用 `config-check` 校验 YAML；若 SQLite 文件损坏，服务会报错而不会自动清空它。

备份前优雅停止服务，再把 YAML 和 SQLite 数据库文件作为同一份快照复制保存。SQLite 使用 WAL 模式；如果必须在线备份，应使用 SQLite 在线备份 API，不能只复制运行中的 `.db` 文件而忽略 `-wal`。恢复时也先停止服务，保留故障现场副本，再将同一份备份中的 YAML 和数据库放回原路径，运行 `config-check` 后启动。若仅恢复 YAML 而不恢复数据库，运行时选择与健康状态可能对应不上当前代理清单；服务会按稳定 ID 重新关联，无法关联的旧状态不会参与选路。磁盘写满时应先释放空间并检查日志，再重启服务确认持久化状态。

可用以下命令验证本地 SOCKS5 客户端（先在 YAML 中填入可用上游并等待健康检查成功）：

```sh
curl --fail --show-error --socks5-hostname 127.0.0.1:30001 https://example.com/
```

`--socks5-hostname` 使域名由上游解析。自动化测试也会在安装了 curl 的系统上覆盖无认证和用户名密码认证两种上游。macOS 上仍需运行发布脚本和该客户端命令做实际系统验收。
