# IP 池服务

按 [roadmap](docs/roadmap.md) 分阶段实现的单机 IP 池服务。目前完成阶段 1 至 4：配置与状态存储、Provider 选路、本地 SOCKS5 TCP `CONNECT`，以及代理健康检查和自动故障切换。管理页面尚未接入。

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

当前前端只是工程骨架。生产页面、静态文件嵌入和 API 将在后续阶段接入。

阶段 2 的选路模块位于 `internal/routing`。新连接可读取一次 `Snapshot()`，已确认不可用时会得到“无代理”；手动选择允许选择已启用但尚未确认健康的代理。自动候选只包含健康状态为 `healthy` 的代理，自动切换开关开启时不会立刻轮换。选路行为可用 `go test ./internal/routing ./internal/provider` 无网络验证。

在 `providers[].proxies` 中加入真实上游 SOCKS5 代理后，可启动本地入口。服务会立即检测代理，并在首次找到健康代理时选择一个作为当前代理；也可在启动前手动指定：

```sh
go run ./cmd/isp select config.example.yaml proxy-seller your-proxy-id
go run ./cmd/isp auto-switch config.example.yaml on
go run ./cmd/isp serve config.example.yaml
```

`select` 和 `auto-switch` 修改 SQLite 中的运行状态；自动切换默认关闭。服务运行期间尚无管理接口，若在另一个进程中修改这些设置，需重启 `serve` 才会读到新值。

入口只接受本地 SOCKS5 无认证客户端的 TCP `CONNECT`，上游可无认证或使用用户名密码。客户端请求中的域名会交给上游解析；没有当前代理或当前代理确认不可用时会返回 SOCKS5 失败，不会直连目标。

健康检查经各自的 SOCKS5 上游访问 `health_check.url`。任何 HTTP 状态码都表示代理链路可用；默认连续失败 3 次才标记不可用。正常检查间隔、超时、失败阈值和退避上限均由 YAML 控制，状态与下次检查时间保存在 SQLite。SOCKS 上游连接错误会提前安排复检，不会直接增加失败计数。自动切换开启后，只有当前代理确认不可用才会切换。
