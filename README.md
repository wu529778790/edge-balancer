# edge-balancer

轻量多云流量分发器 —— 用 Docker 部署在自有服务器上，把入站流量按权重分流到 N 个上游（Cloudflare Worker、Vercel、或任意 HTTP 后端），并带健康检查与自动剔除。

免费替代 Cloudflare Load Balancer 的「分流 + 容灾」逻辑，部署在自己的服务器上，不消耗 Cloudflare 免费配额。

## 特性

- **加权分流 / 灰度**：每个请求独立按权重随机决策，精确到请求级的切量（如 50:50、90:10）
- **健康检查**：定时探测上游健康状态，挂了自动剔除，恢复自动加入
- **纯转发、极轻量**：Go 单二进制，I/O 转发，几乎不耗 CPU
- **配置即代码**：一个 `config.yaml` 搞定

## 快速开始

```bash
# 1. 准备配置
cp config.example.yaml config.yaml
# 编辑 config.yaml，填入你的上游地址和权重

# 2. 本地运行
go run . -config config.yaml

# 3. 或 Docker 运行
docker build -t edge-balancer .
docker run -p 8080:8080 -v $(pwd)/config.yaml:/app/config.yaml edge-balancer
```

## 配置说明

```yaml
listen: ":8080"               # 监听地址
health_interval: 10           # 健康检查间隔（秒）
health_timeout: 5             # 健康检查超时（秒）
health_path: "/api/health"    # 默认健康检查路径

upstreams:
  - name: "cf-worker-a"
    url: "https://xxx.workers.dev"
    weight: 50
  - name: "vercel"
    url: "https://your-app.vercel.app"
    weight: 50
```

| 字段 | 必填 | 说明 |
|------|------|------|
| `name` | ✅ | 上游唯一名称 |
| `url` | ✅ | 上游地址 |
| `weight` | ✅ | 分流权重（灰度比例），如 50:50、90:10 |
| `health` | - | 该上游专属健康检查路径，覆盖全局 `health_path` |

## 工作原理

```
入站流量 → edge-balancer
              ├─ 健康检查：定时探测上游，维护健康状态
              ├─ 加权随机：按 weight 选一个健康上游
              └─ 反向代理：转发请求到选中的上游
```

- 灰度/切量 = 权重比例（`weight`），改配置 reload 即生效
- 容灾 = 健康检查自动剔除故障上游，流量自动切到剩余健康上游

## Roadmap

- [x] v1：加权分流 + 健康检查 + 自动剔除（当前）
- [ ] v2：后台管理（Web 界面增删上游、拖权重、看健康状态）
- [ ] v3：动态配置热加载（不重启改配置）

## License

[MIT](LICENSE)
