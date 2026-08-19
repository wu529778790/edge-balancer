# edge-balancer 重构方案：从「流量转发」到「DNS 故障切换」

> 版本：v1 草案（2026-08-18）
> 状态：待审阅。审阅通过后按「迁移步骤」实施。
> 一句话：数据面用户直连主/备目标，edge-balancer 退出数据路径，转型为只负责探测 + 切换 DNS 的控制面。

---

## 一、问题诊断：为什么「单独打开快、经它转发就慢」

### 1.1 架构绕路（第一性原因，无法靠调优消除）

用户直连 CF Worker 时，请求在**离用户最近的 Cloudflare 边缘节点**就执行完成了，不经过任何第三方服务器。

经 edge-balancer 转发时，每次请求被强制走：

```
浏览器 → CF 边缘（新加坡接入）→ 新加坡服务器（OpenResty → edge-balancer）→ 出公网回源 CF Worker → 原路返回
```

**多走两趟公网**（用户↔新加坡、新加坡↔CF）。Go 转发本身是内存拷贝、几乎零开销，慢的是"绕路"这个事实。

### 1.2 least-conn 在低并发下退化为「固定取配置第一个」

个人站并发 1~5，所有上游 `inFlight` 恒为 0，`pickLeastConn` 永远返回配置里的第一个上游。当前配置 `cf-worker` 排第一，结果是：

- **99% 的流量 100% 走"服务器回源 CF"这条慢路**
- 同机 1ms 的本地 docker 源站被闲置

"双活分流"对低频流量是伪均衡。

### 1.3 容灾机制测「通不通」不测「快不快」

- 健康检查是 `/api/health` 小请求，线路「慢但通」时永远绿 → 流量持续往慢路送
- 直到转发超时（默认 10s）触发熔断 30s → 半开后又被 least-conn 选中 → 又慢 → 又熔断
- 用户感知 = 周期性抖动（一会儿转圈/502，一会儿正常）

### 1.4 日志实锤

- `http2: invalid Host header`：host 重写与 HTTP/2 的兼容问题，曾导致 cf-worker 转发大量失败
- `context deadline exceeded`：本地源站与 CF worker 曾同时超时，四个上游健康全 false → 容灾在「上游集体抖动」时就是全挂

### 1.5 结论

项目在解决"挂了不挂"（容灾），真实诉求是"快且别挂"。当前架构把「就近完成」变成「绕道服务器」，再用一个对低频流量失效的算法把流量全部导到最慢那条路。**方向错误，修修补补无解，必须换架构。**

---

## 二、目标架构：DNS 故障切换器（CNAME failover）

```
┌─ 数据面（直连，edge-balancer 不在路径上）──────────────────────┐
│  浏览器 → DNS 记录 → 主: CF Worker（CNAME）                    │
│                     └→ 备: 服务器 IP（A 记录）                │
│  DNS 记录平时指向主；主挂时由控制面切到备                      │
└──────────────────────────────────────────────────────────────┘
┌─ 控制面（edge-balancer 转型）─────────────────────────────────┐
│  1. 持续探测主 / 备健康                                        │
│  2. 主挂（外部探活连续失败）→ 调 Cloudflare DNS API 切到备      │
│  3. 主恢复且稳定 → 切回                                        │
│  4. 面板：当前 DNS 指向 / 主备健康 / 切换历史 / 手动切换        │
└──────────────────────────────────────────────────────────────┘
```

**为什么这样一定快**：用户访问域名时，DNS 解析直接指向 CF Worker（或服务器 IP），浏览器直连目标 —— 与你现在「单独打开」的路径完全一致。

**为什么容灾仍然成立**：worker 挂了 → 控制面把 DNS 记录改成服务器 IP → 用户下一次解析（TTL 60s 内）就指向服务器。

---

## 三、关键设计决策

### 3.1 主备目标

| 角色 | 目标 | DNS 记录 | 说明 |
|------|------|----------|------|
| 主 | 旧账号 CF Worker（`*.shenzjd.workers.dev`） | CNAME → worker 域名 | 用户本机直连 0.3s，快 |
| 备 | 新加坡服务器（`43.128.70.75`） | A → 服务器 IP | nginx 直连本地源站，用户直连服务器也快 |

每个站点（域名）独立配置主备。主备必须落在**不同故障域**（CF vs 自有服务器），这是容灾的前提。

### 3.2 探测策略：主目标从「外部视角」探测（本次方案的关键）

**矛盾点**：主目标是旧 worker，服务器侧线路差（健康检查能过、业务超时）。如果从新加坡服务器探测它，会把"服务器↔CF 线路差"误判为"worker 挂了"，导致错误切换。

**解法：主目标健康判定不依赖服务器侧，改用外部探活服务**（推荐 UptimeRobot 免费版）：

- UptimeRobot 从全球节点探测 `https://<业务域名>/api/health`（模拟用户视角，天然反映用户侧体验）
- edge-balancer 定时拉取 UptimeRobot 状态（或收 webhook），作为主目标的健康信号
- 备目标 = 服务器自身，直接本地探测 `127.0.0.1:<源站端口>/api/health`（天然准确）

**判定规则（防抖）**：

| 事件 | 条件 | 动作 |
|------|------|------|
| 判挂 | 外部探活连续 3 次失败（或 5xx） | 切 DNS 到备 |
| 判恢复 | 备本地探测正常 **且** 外部探活连续 10 次成功（约 100s+） | 切回主 |
| 冷却 | 一次切换后 2×TTL（120s）内 | 禁止反向切换，防抖 |

**降级**：暂不接入外部探活时，退化为服务器侧探测 + 宽松超时（仅把确定性失败——连接拒绝/404/5xx——判为挂，超时不判挂），并提示主目标判定可能受服务器线路影响。

### 3.3 DNS 切换（Cloudflare DNS API）

- 每站一条 DNS 记录，**TTL 设 60s**（切换生效上限 = 客户端缓存 + TTL，最坏 ~60–120s）
- 流程：`GET /zones?name=shenzjd.com` → `GET /zones/{id}/dns_records?name=<域名>` → `PATCH /zones/{id}/dns_records/{rid}` 更新 `content` + `ttl`
- 首次自动解析 zone_id / record_id 并本地缓存；记录被外部改动时重新查询
- **token 要求**：新建最小权限 token —— `Zone.Zone:Read` + `Zone.DNS:Edit`（现有配额监控 token 权限不足，需另建）

### 3.4 每站切换状态机

```
                外部探活失败×3
   ┌─ ACTIVE(主) ───────────────→ FAILED_OVER(备)
   │                                  │
   └──────────── 恢复稳定×10 ◄────────┘
                    手动模式：面板按钮直接强制切，优先级高于自动
```

- 状态：`ACTIVE`（DNS 指向主）/ `FAILED_OVER`（DNS 指向备）/ `MANUAL`（手动接管）
- 每次切换记录：时间、原因（自动探测/手动）、从→到、触发源
- 面板展示全部历史

### 3.5 面板改造

- 每站一行：当前 DNS 指向（主/备）、主健康、备健康、最近切换时间
- 按钮：切到主 / 切到备 / 恢复自动（退出手动模式）
- 切换历史表（取代原「最近转发日志」——数据面不再经过，转发日志自然消失）

---

## 四、代码改动清单

### 删除

| 文件 | 内容 |
|------|------|
| `internal/dataplane/` 全部 | 转发器（ReverseProxy）、least-conn、加权选路、请求级熔断、请求日志 |

### 新增

| 文件 | 内容 |
|------|------|
| `internal/dns/dns.go` | CF DNS API 客户端：查 zone/record、PATCH 记录、TTL 控制、记录缓存 |
| `internal/dns/probe.go` | 外部探活拉取（UptimeRobot API / webhook）+ 备目标本地探测 |
| `internal/failover/failover.go` | 每站状态机：探测 → 决策 → 切换 → 历史记录 |

### 修改

| 文件 | 改动 |
|------|------|
| `internal/config/config.go` | 配置模型改为 `dns`（zone/token/ttl）+ 每站 `primary` / `backup` / `probe` |
| `internal/server/server.go` | 删数据平面组装，接入 failover 循环（定时探测 + 切换） |
| `internal/store/store.go` | 表结构调整：sites 增加主/备/DNS 字段（或上游表语义改为目标） |
| `internal/admin/admin.go` + web | 面板改 DNS 状态 / 切换历史 / 手动切换按钮 |
| `internal/cf/cf.go` | 保留（worker 配额监控仍有用，配额超限可触发切备） |
| `cmd/edge-balancer/main.go` | 启动 failover 循环；CF token 从环境变量读（不入库） |

---

## 五、新配置模型（草案）

```yaml
listen: ":6705"
admin_path: "/admin"
admin_token: "xxx"

dns:
  zone: "shenzjd.com"
  ttl: 60
  # CF_API_TOKEN 从环境变量读取（Zone.Zone:Read + Zone.DNS:Edit 最小权限），不入库

sites:
  - domain: "panhub.shenzjd.com"
    record_type: "CNAME"                 # 当前记录类型
    primary:                             # 主：旧账号 worker
      name: "cf-worker"
      url: "https://panhub-shenzjd-com.shenzjd.workers.dev"
      health: "/api/health"
    backup:                              # 备：服务器直连本地源站
      name: "server"
      dns_content: "43.128.70.75"        # 切换后的记录内容（A → 服务器 IP）
      health: "http://127.0.0.1:5253/api/health"
    probe:
      mode: "uptimerobot"                # external（外部探活，推荐）/ server（服务器侧，降级）
      monitor_id: "7812345"              # UptimeRobot 监控器 ID
```

---

## 六、迁移步骤（上线顺序）

**前置（需用户提供）**：
1. 站点完整清单（确认目前不止 panhub、parse 两个，列全）
2. UptimeRobot 账号 + 为每个主目标建监控器（探测 `https://<域名>/api/health`）
3. 新建 CF API token：`Zone.Zone:Read` + `Zone.DNS:Edit`（最小权限）

**步骤**：
1. **服务器 nginx 反代目标改回源站端口**：业务域名 `6705 → 源站端口`（5253 / 5269）。
   ⚠️ 关键：否则 DNS 直连服务器时流量又进 edge-balancer，问题复现。`edge-balancer.shenzjd.com` 仍 → 6705（面板入口不变）。
2. CF 控制台确认各业务域名记录 TTL=60。
3. 本地开发 + 单元测试（状态机：判挂/恢复/防抖/手动优先）。
4. 构建新镜像，服务器**先以「监控模式」部署**：只探测 + 面板展示，**不实际切换**。观察 1–2 天，核对判定准确（尤其旧 worker 在外部探活下的表现）。
5. 观察期通过 → 开启「自动切换」。
6. **演练**：停主 worker → 验证 ≤5 分钟内自动切到服务器 → 恢复 worker → 验证自动切回。

---

## 七、回滚方案

- 旧版本镜像保留并打 tag，Docker 一条命令回滚
- 任一步异常：在 CF 控制台手动把记录改回 `CNAME → worker`，系统降级为「只展示不切换」（控制面加全局开关）
- nginx 反代目标改动可逆（保留 6705 备份配置片段）
- 文件模式（config.yaml）与 DB 模式（Turso）双支持，配置始终可回退

---

## 八、风险与开放问题

1. **站点清单待确认**："不止三个"，需列出全部业务域名，逐站配置主备。
2. **故障感知延迟**：UptimeRobot 免费版最小探测间隔 5 分钟 → 最长 ~10 分钟感知故障。如需更快（秒级），后续可自建探活点或上付费版；首版接受分钟级（个人站场景足够）。
3. **DNS 切换生效延迟**：最坏 ~60–120s（客户端缓存 + TTL），无法秒级；切换窗口内已解析到旧地址的用户可能短暂访问失败，属 DNS 方案固有限制。
4. **旧 worker 作主的风险**：用户本机直连快，但服务器侧线路差。已通过「外部探活」设计规避误判，但外部探活不可用时降级判定可能误伤（方案 3.2 已列降级策略）。
5. **CF token 安全**：新建最小权限 token，从环境变量注入；沿用现有「敏感信息不落盘」约定。
6. **数据面不再经过 edge-balancer**：原「转发日志 / 在途连接 / 每上游统计」功能删除，面板换为「切换历史」，属预期行为变化。

---

## 九、验收标准

- 域名直连主 worker 的 TTFB ≈ 用户直连 worker 的 TTFB（差值 < 50ms）
- 停主 worker 后 ≤ 5 分钟自动切到服务器；切换窗口后访问恢复正常
- 主恢复稳定后自动切回，无来回抖动
- 面板实时显示：当前 DNS 指向、主备健康、切换历史；手动切换一键生效
- 全程无需重启容器（配置热加载沿用 DB 模式 5s 轮询）

---

## 十、实施进度与实测验证（2026-08-19）

### 10.1 实测发现（CF DNS 记录盘点）

- **parse.shenzjd.com 当前是 A 记录 → 43.128.70.75，proxied=True（橙色云）**，TTL 自动
- **关键设计结论**：proxied=True 模式下，用户永远先到 CF 边缘、由 CF 回源到记录目标。
  切换只是 CF 内部回源目标变化（CNAME→worker 或 A→服务器 IP），**解析 IP 不变，
  用户本地 DNS 缓存不影响切换生效** → 切换生效 ≈ 秒级~1 分钟，比 CNAME 直连（proxied=False）更干净。
  因此保持 proxied=True 不改变，切换只 PATCH type + content。
- 站点清单（A 记录全部 → 43.128.70.75，proxied=True）：panhub、parse、api、alist、
  imagegate、img、navhub、promoter、tgagent、wx-auth、www、edge-balancer、shenzjd.com 等。
  本次只迁移 parse 单站。

### 10.2 已实现（代码，commit 待打）

| 模块 | 内容 |
|------|------|
| `internal/dns/dns.go` | CF DNS API 客户端：ZoneID/RecordID 解析缓存、GetRecord、PatchRecord（type+content+ttl+proxied） |
| `internal/failover/failover.go` | 状态机：active/failed_over/manual；探测（超时视为慢不判挂）；判挂 3 次/判恢复 10 次/冷却 120s；切换历史；手动切换；dry-run 监控模式 |
| `internal/config` | 新增 DNS/Target/Probe 配置模型，旧 upstreams 模型向后兼容，两模型互斥校验 |
| `internal/server` | failover 站点不建转发器；failover 域名直连提示；failover 仅在启动构建一次（避免 DB reload 反复调 CF API） |
| `internal/admin` | /admin/api/failover 状态接口 + 手动切换/恢复自动；面板总览新增「DNS 故障切换」卡片 |
| `cmd/edge-balancer` | CF_API_TOKEN 环境变量；启动日志区分直连/转发站点 |

### 10.3 已验证（端到端）

- CF API：查 zone（0b6d056b26fe435f1f44c7561e697b46）、查/读记录、幂等 PATCH 权限 ✓
- 启动对齐：正确识别 parse 当前 DNS=A→43.128.70.75 → 状态 failed_over ✓
- 探测：主（旧 worker）HTTP 200/640ms；备（本机 5269）connection refused ✓
- 状态机：failed_over 下主恢复稳定 10 次 → 决策切回主 ✓
- **dry-run 安全阀**：决策正确触发但未修改线上 DNS（parse 记录保持 A→43.128.70.75）✓
- 单元测试 11 个全过（含熔断/恢复/冷却/手动/dry-run）✓

### 10.4 待办（部署到服务器后）

1. 服务器 nginx：parse 域名反代目标 6705 → 5269（否则 DNS 直连服务器时又绕回 edge-balancer）
2. 部署 dry_run: true 监控模式跑 1-2 天，核对判定准确
3. 观察期通过 → dry_run 改 false，开自动切换
4. 演练：停主 worker → 验证自动切服务器 → 恢复 → 验证切回
5. 迁移 panhub 及其余站点（每站配置主备）

### 10.5 failover 配置入库（DB 模式恢复，2026-08-19）

- 用户指出"既然有数据库为何还绕回 config.yaml"——正确，收回文件模式妥协。
- **store 层**：新增 `failover_sites` 表（domain 唯一 + 主/备目标 10 字段 + probe 6 字段）；CRUD；LoadConfig 从库构建 failover 站点，dns 全局配置（dns_zone/dns_ttl/dns_dry_run/dns_token_env）走 settings 表。
- **server 层**：failover 配置指纹（hash）变化才重建（DB 模式 5s reload 不频繁调 CF API）；重建时取消旧循环、启动新循环。
- **admin/面板**：/api/failover/sites CRUD（DB 模式）；配置管理页新增「DNS 直连站点」卡片（新增/编辑/删除）+ 全局设置新增 DNS 三项。
- **部署形态恢复**：.env 恢复 EDGE_DB_URL/TOKEN（DB 模式），CF_API_TOKEN 仍环境变量（安全）；config.yaml 仅本地调试用。
- 测试：store 层 2 个新测试（CRUD 往返 + LoadConfig 构建 failover/转发共存）全过；全量 15 测试通过。
