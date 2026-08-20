# 后台管理面板 · 设计稿

Edge Balancer 控制台重新设计稿（浅色 Indigo Web Console 风格，1440×1152 桌面布局，2x 导出 2880×2304）。

| 页面 | 文件 | 内容 |
|---|---|---|
| 总览 | `overview.png` | 4 KPI + DNS 配额轮换（parse / panhub 双站点，目标队列、健康、配额、状态徽章、切换按钮、事件日志） |
| 配置管理 | `config.png` | 全局设置（健康探测 + DNS 故障切换）+ DNS 直连站点（目标队列 CRUD） |
| Cloudflare 配额 | `cf-quota.png` | 多账号配额监控（shenzjd / 2509818162，额度、阈值、使用率进度条） |

## 设计 Token

- 背景 `#F5F6FA` / 卡片 `#FFFFFF` / 侧边栏 `#1B1E2A`
- 主色 `#4F46E5`（Logo 渐变 `#5A5CF2 → #8C6BF8`）
- 状态色：成功 `#10B981` / 错误 `#EF4444` / 警告 `#F59E0B` / 关闭 `#9CA3AF`
- 字体：中文 Noto Sans SC + 英文 Inter

## Ardot 源文件

- https://ardot.tencent.com/file/716915021920569
