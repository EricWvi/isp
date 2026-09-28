# IP 池服务

按 [roadmap](docs/roadmap.md) 分阶段实现的单机 IP 池服务。目前完成阶段 1：配置文件、SQLite 运行状态存储及前端工程骨架。尚未启动 SOCKS5 或管理服务。

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
