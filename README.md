# IP 池服务

按 [roadmap](docs/roadmap.md) 分阶段实现的单机 IP 池服务。目前完成阶段 1 和阶段 2：配置文件、SQLite 运行状态存储、Provider 环形选择及全局当前代理。尚未启动 SOCKS5 或管理服务。

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
