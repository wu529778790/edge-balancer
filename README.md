# edge-balancer

轻量多云流量分发器（中心网关）—— 用 Docker 部署在自有服务器上，**按域名（Host 头）路由**到各自的 N 个上游（Cloudflare Worker、Vercel、或任意 HTTP 后端），并带健康检查与自动剔除。未匹配到业务域名的访问（如管理域名）直接展示**状态管理面板**。

免费替代 Cloudflare Load Balancer 的「分流 + 容灾」逻辑，部署在自己的服务器上，不消耗 Cloudflare 免费配额。

## 特性

- **多域名路由**：每个域名（site）独立一组上游，按 Host 头分发（如 `panhub.shenzjd.com` 和 `api.shenzjd.com` 各自分流）
- **管理入口**：访问未配置的域名（如 `edge-balancer.shenzjd.com`）直接看到状态面板
- **加权分流 / 灰度**：每个请求独立按权重随机决策，精确到请求级的切量（如 50:50、90:10）
- **逐级兜底（priority）**：高优先级上游健康时独享流量，挂了自动切到下一级（如 CF Worker → 本地服务）
- **最少连接（least-conn）**：组内按实时在途请求数动态均衡，避免某一台上游被突发流量压垮
- **健康检查**：定时探测上游健康状态，挂了自动剔除，恢复自动加入
- **状态面板**：`/admin` 实时查看各域名下上游的健康状态、累计转发数、在途连接与最近请求分发记录
- **纯转发、极轻量**：Go 单二进制，I/O 转发，几乎不耗 CPU
- **配置即代码**：一个 `config.yaml` 搞定

## 快速开始

```bash
# 1. 准备配置
cp config.example.yaml config.yaml
# 编辑 config.yaml，填入你的站点（域名）和上游地址

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
strategy: "least-conn"        # 默认策略：least-conn（最少连接）/ weighted（加权随机）
admin_path: "/admin"          # 状态面板路径
admin_token: "your-token"     # 面板 token（推荐配置）

sites:
  - domain: "panhub.shenzjd.com"   # 业务域名（匹配 Host 头）
    strategy: "least-conn"         # 可选：覆盖全局策略
    health_path: "/api/health"     # 可选：覆盖全局健康检查路径
    upstreams:
      - name: "cf-worker"
        url: "https://xxx.workers.dev"
        priority: 1                # 优先级：越小越优先，挂了才轮到下一级
      - name: "self-server"
        url: "http://127.0.0.1:3000"
        priority: 2                # 兜底
```

| 字段 | 必填 | 说明 |
|------|------|------|
| `domain` | ✅ | 站点域名，匹配请求的 Host 头；未匹配的域名直接显示状态面板 |
| `name` | ✅ | 上游名称（站点内唯一） |
| `url` | ✅ | 上游地址 |
| `priority` | - | 优先级，越小越优先；高优先级健康则独享流量，挂了才轮到下一级 |
| `weight` | - | 仅 `strategy: weighted` 时生效，组内分流权重 |
| `health` | - | 该上游专属健康检查路径，覆盖站点/全局 `health_path` |

### 两种使用模式

**逐级兜底（主备备）**：给每个上游设不同 `priority`，实现「CF Worker → 本地服务」的容灾链，高优先级挂了自动切下一级。

**灰度分流（多活）**：不设 `priority`（或设相同值），配合 `strategy: weighted` + 不同 `weight`，实现按比例的流量切分。

## 状态面板（v2）

- **管理入口**：访问未配置的域名（如 `edge-balancer.shenzjd.com`）直接显示面板；或访问任意已配置域名的 `/admin` 路径
- 每个站点（域名）一张表：上游健康状态（绿/红）、权重、优先级、在途连接数、累计转发次数
- 最近 200 条请求记录（时间 + 域名 + 路径 + 实际转发到了哪个站点/上游）
- 页面每 3 秒自动刷新；数据接口为 `/admin/api`（JSON）

配置了 `admin_token` 后，访问面板需携带 `?token=<token>`。

## 工作原理

```
入站流量 → edge-balancer（按 Host 头选站点）
              ├─ 未匹配域名 → 状态面板（管理入口）
              ├─ 匹配站点 → 健康检查 + 选上游（priority 兜底 / least-conn / weighted）
              └─ 反向代理：转发请求到选中的上游
```

- 灰度/切量 = 权重比例（`weight`），改配置 reload 即生效
- 容灾 = 健康检查自动剔除故障上游，流量自动切到剩余健康上游
- 内网上游（同机服务）直连 `127.0.0.1:<端口>`，可绕过公网 CDN/WAF（如 Cloudflare 托管质询）

## Roadmap

- [x] v1：加权分流 + 健康检查 + 自动剔除
- [x] v2：状态面板（上游健康/统计/最近请求分发记录，`/admin`）
- [x] v2.5：多域名路由（按 Host 头分发到各站点上游组）+ 管理入口
- [ ] v3：Web 界面增删站点/上游、拖权重（动态配置热加载，不重启）

## License

[MIT](LICENSE)
