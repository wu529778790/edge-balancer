# edge-balancer 架构方案：DNS 配额轮换控制面（v2）

> 版本：v2（2026-08-20，与线上实现一致）
> 状态：**已落地**（parse 单站试点；worker-a ↔ server 双向切换实测通过）
> 一句话：数据面用户直连目标（CF Worker / 服务器 IP），edge-balancer 退出数据路径，转型为「配额轮换 + 故障切换」控制面——按 CF 免费配额（10 万请求/**天**/**账号**）消费有序目标队列，配额用尽或故障自动切下一个，每日零点配额重置自动回切。

---

## 一、问题诊断：为什么「单独打开快、经它转发就慢」

### 1.1 架构绕路（第一性原因，无法靠调优消除）

用户直连 CF Worker 时，请求在**离用户最近的 Cloudflare 边缘节点**就执行完成了。经 edge-balancer 转发时，每次请求被强制走：

```
浏览器 → CF 边缘（新加坡接入）→ 新加坡服务器（OpenResty → edge-balancer）→ 出公网回源 CF Worker → 原路返回
```

**多走两趟公网**（用户↔新加坡、新加坡↔CF）。Go 转发本身零开销，慢的是"绕路"这个事实。

### 1.2 least-conn 在低并发下退化 + 容灾测「通不通」不测「快不快」

- 个人站并发 1~5，所有上游 `inFlight` 恒为 0 → 永远取配置第一个上游 → 99% 流量走"服务器回源 CF"这条慢路
- 健康检查是 `/api/health` 小请求，线路「慢但通」时永远绿 → 流量持续往慢路送，直到转发超时触发熔断 → 半开又被选中 → 周期性抖动
- 日志实锤：`http2: invalid Host header`、`context deadline exceeded`（上游集体抖动时容灾 = 全挂）

**结论**：项目在解决"挂了不挂"（容灾），真实诉求是"快且别挂"。旧转发架构方向错误，**必须换架构**。

---

## 二、目标架构：DNS 配额轮换控制面（最终形态）

```
┌─ 数据面（直连，edge-balancer 不在路径上）────────────────────────┐
│  用户 → 自己域名（CF 边缘 proxied）                                │
│    Route: parse.shenzjd.com/* → worker-a（主，配额账号 A）        │
│    配额用尽 / 故障 → 删除 Route → A 记录 43.128.70.75 回源服务器   │
│    （切换 = Workers Route 增删，DNS 记录本身 proxied 可编辑）      │
└──────────────────────────────────────────────────────────────────┘
┌─ 控制面（edge-balancer）────────────────────────────────────────┐
│  1. 配额信号：CF GraphQL 按账号查当天请求数（10 万/天/账号）        │
│  2. 健康信号：服务器侧探测目标（严格判定，超时不判挂）              │
│  3. 任一信号触发 → 队列指针 +1，切到下一个可用目标                 │
│  4. 每日零点配额重置 → 扫描队列回切到最早可用目标                  │
│  5. 面板：总览 / 配置管理 / Cloudflare 配额（三页）                │
└──────────────────────────────────────────────────────────────────┘
```

**为什么一定快**：DNS/Route 指向谁，用户就直连谁——与"单独打开"路径完全一致。

**为什么轮换成立**：CF Workers Free 配额是**账号级**（10 万请求/**天**），多账号独立计数 → 跨账号才能累积免费额度。但由于 CF 机制限制（见 3.1），最终形态收敛为**单账号 worker-a + 服务器兜底**的 2 目标队列。

---

## 三、关键设计决策

### 3.1 CF 机制限制（实测确认，2026-08-20）

| 方案 | 实测结果 | 结论 |
|------|----------|------|
| CNAME → workers.dev（proxied） | **HTTP 522**，CF 不会把 CNAME 目标当 worker 执行 | ❌ 纯 CNAME 跨账号不可行 |
| 跨账号 Custom Domain | 同一域名只能绑一个账号的一个 worker；DNS 验证（CNAME/TXT 目标值）在 CF 控制台卡死，反自动化强 | ❌ 跨账号轮换在 CF 官方机制下**不可行** |
| Workers Routes 增删 + DNS 记录 | 同账号 worker 与服务器 IP 间双向切换，实测通过 | ✅ **最终方案** |

**命名澄清**：`<name>.<account-sub>.workers.dev` 中 `account-sub` 是 CF 给账号分配的子域标识，**不是 worker 名**。账号 B（2509818162@qq.com，account_id `b927f0e6...`）的 worker 名是 `parse-shenzjd-com`。

### 3.2 目标队列（targets[]，N 目标通用）

每个站点一个**有序队列**，按顺序消费：

| 位置 | 目标 | 配额信号 | 说明 |
|------|------|----------|------|
| 1..n-1 | CF Worker | 有 `quota_account` | 配额超限或健康失败 → 切下一个 |
| n（末位） | 服务器 IP | 无 `quota_account` = 无限额度 | 兜底，健康失败无处可切 → 告警 |

- 切换只动 **Workers Route**（增/删）或 DNS 记录 content；proxied=True 下用户解析 IP 不变，**切换秒级~1 分钟生效，不碰用户 DNS 缓存**
- **每日回切**：跨天（零点配额重置）扫描队列 → 回切到最早可用目标（健康信号驱动；切走不主动回切，防乒乓）

### 3.3 双信号决策（配额为主，健康为辅）

| 信号 | 来源 | 触发 |
|------|------|------|
| 配额超限 | `internal/cf.QueryUsage`（GraphQL Analytics 按账号查当天请求数，节流 `quota_interval` 默认 300s） | `used/limit ≥ threshold%`（默认 90%） |
| 健康失败 | 服务器侧探测（probe interval 10s） | **严格判定**：连接拒绝 / 4xx / 5xx 连续 3 次；**超时不判挂**（防服务器侧线路误判，2026-08-18 教训） |

任一触发 → 指针 +1 找下一个可用目标；PATCH/Route 失败回滚 `currentIndex`（防状态漂移）。

### 3.4 状态机

```
  配额超限 / 健康失败×3         每日零点扫描 / 手动切换
  ACTIVE(目标 i) ───────────────→ 目标 i+1（...→ server 兜底）
        │                              │
        └────────── 手动接管 ◄─────────┘  （手动切换放行，自动切换 dry-run 时只决策不执行）
```

- 状态：`auto`（自动轮换，当前指向目标 i）/ `manual`（手动接管，仍探测当前目标但不自动切换）
- 每次切换记录：时间、原因（配额超限/健康失败/每日回切/手动）、从→到

### 3.5 凭据安全（零落盘）

| Token | 用途 | 权限 | 来源 |
|-------|------|------|------|
| `CF_API_TOKEN` | DNS/Route 切换 | Zone.DNS:Edit + Workers Routes:Edit + 区域泛权限 | 环境变量 |
| `CF_TOKEN_SHENZJD` | 账号 A 配额查询 | Account Analytics:Read | 环境变量 `token_env` |
| `CF_TOKEN_2509818162` | 账号 B 配额查询 | Account Analytics:Read | 环境变量 `token_env` |

- DB `cf_accounts` 只存 `token_env` **引用名**，不存 token 明文（修复 P1：GET /admin/api/cf 不再回传明文）
- `admin_token` 走 URL query（已知局限，见风险 8.3）

---

## 四、代码落地（与线上实现一致）

| 模块 | 内容 |
|------|------|
| `internal/cf/cf.go` | `QueryUsage`：GraphQL 按账号查当天请求数（10 万/天），支持 `token_env` 读环境变量 |
| `internal/dns/dns.go` | CF DNS API 客户端：zone/record 解析缓存、PatchRecord；Workers Routes 增删 |
| `internal/failover/failover.go` | 配额轮换状态机：队列指针 + 双信号 + 每日回切 + dry-run 只决策不执行 + PATCH 失败回滚 currentIndex + 探测并行化 |
| `internal/config/config.go` | `sites[].targets[]` 有序队列（N 通用）+ `quota_account`；`primary/backup` 旧模型 Normalize 兼容转换；域名唯一校验 |
| `internal/store/store.go` | `failover_sites` 表 `targets` JSON 列 + `probe_quota_interval` 列（旧列保留回退） |
| `internal/admin/` | 面板三页（总览/配置管理/CF 配额，浅色 Indigo 设计稿 docs/design/）；目标队列动态编辑 + 配额水位 + 手动切换；CF 账号 `token_env` 表单 |
| `internal/dataplane/` | 旧转发模式，**兼容保留不删**（转发相关 API/JS 保留，UI 不再暴露） |

### 配置模型（与 config.example.yaml 一致）

```yaml
dns:
  zone: "shenzjd.com"
  token_env: "CF_API_TOKEN"
  dry_run: true          # 监控模式：只决策不切换

cf_accounts:
  - name: "shenzjd"
    account_id: "xxx"
    quota: 100000
    threshold: 90
    token_env: "CF_TOKEN_SHENZJD"

sites:
  - domain: "parse.shenzjd.com"
    targets:             # 有序队列：配额用尽/故障 → 切下一个；末位兜底
      - name: "worker-a"
        record_type: "CNAME"
        dns_content: "parse-shenzjd-com.shenzjd.workers.dev"
        quota_account: "shenzjd"
      - name: "server"
        record_type: "A"
        dns_content: "43.128.70.75"
        url: "http://127.0.0.1:5269"
    probe:
      mode: "server"
      interval: 10
      fail_threshold: 3
      cooldown: 120
      quota_interval: 300
```

---

## 五、迁移步骤（已执行 / 待执行）

**已完成 ✅**：
1. ✅ 方案定稿（v1 转发 → v2 配额轮换；跨账号轮换经实测判死刑，收敛单账号 + 服务器兜底）
2. ✅ 代码：config targets[] / 状态机 / cf 接入 / store / admin / 面板（33 单测 + race 全过）
3. ✅ 服务器部署：Docker `--network host` 监听 6705，1Panel OpenResty 反代 `edge-balancer.shenzjd.com` → 6705
4. ✅ 服务器 .env：`EDGE_DB_URL/TOKEN`、`CF_API_TOKEN`、`CF_TOKEN_SHENZJD`、`CF_TOKEN_2509818162`、`EDGE_LISTEN=:6705`
5. ✅ nginx：parse 反代目标 6705 → 5269（DNS 直连服务器时不绕回 edge-balancer）
6. ✅ 双向切换实测：切 server（DELETE route → 源站 5269，Nuxt 200）↔ 切回 worker-a（PUT route → worker 200）
7. ✅ 管理面板设计稿重排（docs/design/，Ardot 716915021920569）+ README 重写

**待执行 ⏳**：
1. DB `dns_dry_run=1` 监控模式观察 1–2 天（核对配额数据、每日回切行为）
2. 通过后 `dns_dry_run=0` 开自动切换
3. 迁移 panhub 等站（每站配置 targets 队列 + route）
4. 清理：账号 B custom domain 残留（parse.shenzjd.com 断开连接）用户手移除；sites 表旧 parse 转发站点清理

---

## 六、回滚方案

- 旧版本镜像保留并打 tag，Docker 一条命令回滚
- 任一步异常：CF 控制台手动恢复 Route / DNS 记录指向（A → 43.128.70.75 或 Route → worker-a），系统降级为「只展示不切换」（`dns_dry_run=1`）
- 文件模式（config.yaml）与 DB 模式（Turso）双支持，配置始终可回退
- ⚠️ 教训：`pkill -f edge-balancer` 会误杀本地调试实例（进程名含 edge-balancer）；`.env` 同步覆盖会改坏生产监听（EDGE_LISTEN / EDGE_FORCE_DRY_RUN 事故，见 10.x）

---

## 七、风险与开放问题

1. **配额轮换的单账号收敛**：跨账号轮换被 CF 机制否决后，单账号（10 万/天）打满只能切服务器。若要继续提量，需评估：多账号 → 多域名（每账号一个自有域名）平行扩容，或换付费套餐。
2. **配额查询延迟**：GraphQL 用量查询非实时（有延迟），阈值 90% 预留切换余量；`quota_interval` 节流避免打爆 API。
3. **admin_token 走 URL query**：浏览器历史/日志可见（P1 已知）；改进方向：Cookie 会话 / Header 传递，暂未排期。
4. **健康判定受服务器线路影响**：服务器侧探测可能误判 CF worker 状态（08-18 教训：旧 worker 服务器侧线路差）；已用「严格判定（连接拒绝/4xx/5xx 连续 3 次，超时不判挂）」规避，监控模式观察期验证。
5. **无单元测试覆盖 admin/cf/dns HTTP 层**：failover 状态机已有 17 个测试，HTTP 层尚未补（P1）。

---

## 八、验收标准（配额轮换语义）

- 域名直连当前目标的 TTFB ≈ 用户直连该目标的 TTFB（差值 < 50ms，无绕路）
- 配额超限（≥90%）→ 自动切下一个目标；每日零点配额重置 → 自动回切最早可用目标
- 目标故障（连接拒绝/5xx×3）→ 自动切下一个；超时**不**误切
- 手动切换一键生效；手动模式不自动切换；PATCH 失败不产生状态漂移
- 面板实时显示：当前指向 / 队列健康 / 配额水位 / 切换历史；全程无需重启容器

---

## 九、实施进度时间线

| 日期 | 事件 |
|------|------|
| 08-18 | v1 草案（转发 → DNS 故障切换）；排查旧 worker 服务器侧线路差，旧 worker 停用 |
| 08-19 | failover 控制面代码（internal/dns + internal/failover + config/server/admin 改造）+ 11 单测；dry-run 端到端验证；DB 模式 failover 配置入库 |
| 08-20 上午 | 用户拍板：跨账号配额轮换队列（A→B→服务器，每日回切，N 目标通用） |
| 08-20 下午 | CDP 实测：CNAME→workers.dev = 522、跨账号 Custom Domain 卡 DNS 验证 → **跨账号轮换判死刑**；收敛单账号 worker-a + server 兜底 |
| 08-20 16:38 | 服务器部署新代码；踩坑：本地 .env 覆盖生产（EDGE_LISTEN=:8080 + FORCE_DRY_RUN）→ 502 半小时 → 修复为生产版 |
| 08-20 17:07 | **Route 方案部署 + 双向切换闭环验证通过**（commit eb9fef1 已 push） |
| 08-20 晚 | 面板按设计稿重排（1800182 已 push）；README 重写 + GitHub 元数据更新（a6b79ed 未 push） |
