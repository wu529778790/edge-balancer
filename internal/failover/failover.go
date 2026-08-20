// Package failover DNS 配额轮换状态机（新架构控制面核心）。
//
// 职责：每站维护一个有序目标队列（worker A → worker B → 服务器兜底），
// 双信号驱动切换：配额信号（CF 免费 10 万请求/天/账号，用尽自动切下一个）为主，
// 健康信号（连接拒绝/4xx/5xx 连续失败）为辅；配额每日重置后自动切回最早可用目标。
// 数据面不经过本包。
package failover

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/wu529778790/edge-balancer/internal/cf"
	"github.com/wu529778790/edge-balancer/internal/config"
	"github.com/wu529778790/edge-balancer/internal/dns"
)

// State 站点状态
type State string

const (
	StateAuto   State = "auto"   // 自动模式（队列按信号推进）
	StateManual State = "manual" // 手动锁定（不自动切换）
)

// SwitchEvent 一次切换记录
type SwitchEvent struct {
	Time   string `json:"time"`
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"` // auto / quota / manual
	Detail string `json:"detail"`
}

// TargetStatus 单个目标的探测 + 配额状态（面板）
type TargetStatus struct {
	Name         string  `json:"name"`
	RecordType   string  `json:"record_type"`
	DNSContent   string  `json:"dns_content"`
	OK           bool    `json:"ok"`
	Latency      string  `json:"latency"`
	Detail       string  `json:"detail"`
	QuotaAccount string  `json:"quota_account,omitempty"` // 空=无限额度兜底
	QuotaUsed    int64   `json:"quota_used,omitempty"`
	QuotaLimit   int64   `json:"quota_limit,omitempty"`
	QuotaPercent float64 `json:"quota_percent,omitempty"`
	QuotaOver    bool    `json:"quota_over,omitempty"`
	QuotaError   string  `json:"quota_error,omitempty"`
}

// SiteStatus 单站状态快照（面板）
type SiteStatus struct {
	Domain       string         `json:"domain"`
	State        State          `json:"state"`
	Current      string         `json:"current"` // 当前 DNS 指向的目标名
	CurrentIndex int            `json:"current_index"`
	Targets      []TargetStatus `json:"targets"`
	CooldownLeft int            `json:"cooldown_left"`
	Events       []SwitchEvent  `json:"events"`
}

// Switcher DNS 记录读写接口（dns.Client 实现；测试可用 fake 替换，避免真调 CF API）
type Switcher interface {
	PatchRecord(zoneID, recordID, rtype, content string, ttl int, keepProxy bool) (*dns.Record, error)
	GetRecord(zoneID, recordID string) (*dns.Record, error)
}

// QuotaQuery 配额查询函数（真实实现 cf.QueryUsage；测试注入 fake）
type QuotaQuery func(acc config.CFAccount) (cf.Usage, error)

// Site 单站配额轮换运行实例
type Site struct {
	Domain   string
	Targets  []config.TargetConfig
	Probe    config.ProbeConfig
	Accounts []config.CFAccount // 配额账号池（按 Name 查找）
	dnsTTL   int
	zoneID   string
	recordID string
	client   Switcher
	dryRun   bool // 监控模式：自动切换只决策不 PATCH；手动切换放行
	quota    QuotaQuery

	mu           sync.Mutex
	state        State
	currentIndex int
	manualIndex  int // StateManual 时锁定的目标下标；-1 未锁定
	lastCheckDay string

	failStreak    int
	cooldownUntil time.Time
	lastQuotaAt   time.Time

	targetResults []TargetStatus
	events        []SwitchEvent
	maxEvents     int
}

// NewSite 构造单站。recordID 由 Manager 注入（构造时解析并缓存）。
// dryRun=true 时进入监控模式：探测与决策照常，自动切换不实际 PATCH。
func NewSite(domain string, targets []config.TargetConfig, accounts []config.CFAccount, probe config.ProbeConfig, client Switcher, zoneID, recordID string, dnsTTL int, dryRun bool, quota QuotaQuery) (*Site, error) {
	if client == nil {
		return nil, fmt.Errorf("site %s: dns client 未初始化", domain)
	}
	if zoneID == "" || recordID == "" {
		return nil, fmt.Errorf("site %s: zone_id / record_id 未解析", domain)
	}
	if len(targets) < 2 {
		return nil, fmt.Errorf("site %s: 至少配置 2 个 targets", domain)
	}
	if quota == nil {
		quota = cf.QueryUsage
	}
	results := make([]TargetStatus, len(targets))
	for i, t := range targets {
		results[i] = TargetStatus{Name: t.Name, RecordType: t.RecordType, DNSContent: t.DNSContent, QuotaAccount: t.QuotaAccount}
	}
	return &Site{
		Domain:        domain,
		Targets:       targets,
		Accounts:      accounts,
		Probe:         probe,
		dnsTTL:        dnsTTL,
		zoneID:        zoneID,
		recordID:      recordID,
		client:        client,
		dryRun:        dryRun,
		quota:         quota,
		state:         StateAuto,
		manualIndex:   -1,
		lastCheckDay:  time.Now().Format("2006-01-02"), // 启动当天不触发每日回切扫描
		targetResults: results,
		maxEvents:     50,
	}, nil
}

// findAccount 按名称查配额账号
func (s *Site) findAccount(name string) *config.CFAccount {
	for i := range s.Accounts {
		if s.Accounts[i].Name == name {
			return &s.Accounts[i]
		}
	}
	return nil
}

// probeTarget 探测单个目标。确定性失败（连接错误/4xx/5xx）判失败；
// 超时视为「慢」不判挂（服务器侧线路差不等于目标挂）。
func probeTarget(t *config.TargetConfig, healthPath string, timeout time.Duration) TargetStatus {
	st := TargetStatus{Name: t.Name, RecordType: t.RecordType, DNSContent: t.DNSContent, QuotaAccount: t.QuotaAccount}
	path := healthPath
	if t.Health != "" {
		path = t.Health
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u := strings.TrimRight(t.URL, "/") + path
	client := &http.Client{Timeout: timeout}
	start := time.Now()
	resp, err := client.Get(u)
	lat := time.Since(start)
	st.Latency = lat.Round(time.Millisecond).String()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "Client.Timeout") {
			st.OK = true
			st.Detail = "timeout(慢，不判挂)"
			return st
		}
		st.OK = false
		st.Detail = err.Error()
		return st
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		st.OK = false
		st.Detail = fmt.Sprintf("HTTP %d", resp.StatusCode)
		return st
	}
	st.OK = true
	st.Detail = fmt.Sprintf("HTTP %d", resp.StatusCode)
	return st
}

// refreshQuotas 节流刷新本站点引用的所有账号配额（含兜底更新到 targetResults）
func (s *Site) refreshQuotas() {
	if time.Since(s.lastQuotaAt) < time.Duration(s.Probe.QuotaInterval)*time.Second {
		return
	}
	s.lastQuotaAt = time.Now()
	seen := map[string]bool{}
	for i := range s.Targets {
		name := s.Targets[i].QuotaAccount
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		acc := s.findAccount(name)
		if acc == nil {
			continue
		}
		u, err := s.quota(*acc)
		for j := range s.Targets {
			if s.Targets[j].QuotaAccount != name {
				continue
			}
			st := &s.targetResults[j]
			st.QuotaUsed = u.Used
			st.QuotaLimit = u.Quota
			st.QuotaPercent = u.Percent
			st.QuotaOver = u.OverLimit
			st.QuotaError = u.Error
			if err != nil {
				st.QuotaError = err.Error()
			}
		}
	}
}

// targetQuotaOK 目标配额是否可用（读最近刷新结果；无 quota_account 视为无限额度）
func (s *Site) targetQuotaOK(i int) bool {
	if s.Targets[i].QuotaAccount == "" {
		return true
	}
	return !s.targetResults[i].QuotaOver
}

// setTargetProbe 写入探测结果并保留该目标已刷新的配额状态（避免覆盖 QuotaOver 等字段）
func (s *Site) setTargetProbe(i int, p TargetStatus) {
	prev := s.targetResults[i]
	p.QuotaUsed = prev.QuotaUsed
	p.QuotaLimit = prev.QuotaLimit
	p.QuotaPercent = prev.QuotaPercent
	p.QuotaOver = prev.QuotaOver
	p.QuotaError = prev.QuotaError
	s.targetResults[i] = p
}

// firstAvailable 返回队列中第一个「配额可用且健康」的目标（每日回切扫描用）。
// 当前目标视为候选但不重新探测（tick 已探测）；返回 -1 表示无可回切目标。
func (s *Site) firstAvailable() int {
	timeout := time.Duration(s.Probe.Timeout) * time.Second
	for i := range s.Targets {
		if !s.targetQuotaOK(i) {
			continue
		}
		if i == s.currentIndex {
			return i
		}
		t := &s.Targets[i]
		p := probeTarget(t, t.Health, timeout)
		s.setTargetProbe(i, p)
		if p.OK {
			return i
		}
	}
	return -1
}

// nextAvailable 返回 currentIndex 之后第一个「配额可用且健康」的目标（切换推进用）。
// 不回绕（避免环形乒乓）；返回 -1 表示无可切目标（当前已是最后兜底）。
func (s *Site) nextAvailable() int {
	timeout := time.Duration(s.Probe.Timeout) * time.Second
	for i := s.currentIndex + 1; i < len(s.Targets); i++ {
		if !s.targetQuotaOK(i) {
			continue
		}
		t := &s.Targets[i]
		p := probeTarget(t, t.Health, timeout)
		s.setTargetProbe(i, p)
		if p.OK {
			return i
		}
	}
	return -1
}

// tick 一次探测与状态迁移（调用方持锁）
func (s *Site) tick() {
	if s.state == StateManual {
		return
	}
	s.refreshQuotas()

	// 每日配额重置回切扫描：新的一天找最早可用目标切回
	today := time.Now().Format("2006-01-02")
	if today != s.lastCheckDay {
		s.lastCheckDay = today
		if s.cooldownDone() {
			if idx := s.firstAvailable(); idx >= 0 && idx != s.currentIndex {
				s.doSwitch(idx, "auto", "每日配额重置，切回最早可用目标")
				return
			}
		}
	}

	cur := &s.Targets[s.currentIndex]
	timeout := time.Duration(s.Probe.Timeout) * time.Second
	p := probeTarget(cur, cur.Health, timeout)
	s.setTargetProbe(s.currentIndex, p)

	// 配额信号（主）：当前目标配额超限 → 切下一个
	if s.Targets[s.currentIndex].QuotaAccount != "" && s.targetResults[s.currentIndex].QuotaOver {
		if s.cooldownDone() {
			if idx := s.nextAvailable(); idx >= 0 {
				st := s.targetResults[s.currentIndex]
				s.doSwitch(idx, "quota", fmt.Sprintf("配额超限 %.0f%%（%d/%d），切到 %s", st.QuotaPercent, st.QuotaUsed, st.QuotaLimit, s.Targets[idx].Name))
			} else {
				log.Printf("failover %s: 配额超限但无可用后继目标（已是最后兜底）", s.Domain)
			}
		}
		return
	}

	// 健康信号（辅）：当前目标连续失败 → 切下一个
	if !p.OK {
		s.failStreak++
		if s.failStreak >= s.Probe.FailThreshold && s.cooldownDone() {
			if idx := s.nextAvailable(); idx >= 0 {
				s.doSwitch(idx, "auto", fmt.Sprintf("%s 连续失败 %d 次（%s），切到 %s", cur.Name, s.failStreak, p.Detail, s.Targets[idx].Name))
			} else {
				log.Printf("failover %s: %s 连续失败但无可用后继目标（兜底失守）", s.Domain, cur.Name)
			}
		}
	} else {
		s.failStreak = 0
	}
}

// cooldownDone 冷却是否已过
func (s *Site) cooldownDone() bool {
	return time.Now().After(s.cooldownUntil)
}

// doSwitch 执行切换：PATCH DNS 记录 + 更新状态（调用方持锁）。
// 目标为队列下标 index；reason 为 auto/quota/manual。
// PATCH 失败回滚 currentIndex，状态机与真实 DNS 保持一致。
func (s *Site) doSwitch(index int, reason, detail string) error {
	target := s.Targets[index]
	from := s.Targets[s.currentIndex].Name
	s.cooldownUntil = time.Now().Add(time.Duration(s.Probe.Cooldown) * time.Second)
	s.failStreak = 0

	// dry-run 监控模式：自动切换只决策不实际 PATCH；手动切换放行（人有意识的操作应真实生效）
	if s.dryRun && reason != "manual" {
		s.currentIndex = index
		s.appendEvent(from, target.Name, reason, detail+"（dry-run，未实际修改 DNS）")
		log.Printf("failover %s [dry-run]: 决策切到 %s（%s, %s）", s.Domain, target.Name, reason, detail)
		return nil
	}

	if _, err := s.client.PatchRecord(s.zoneID, s.recordID, target.RecordType, target.DNSContent, s.dnsTTL, true); err != nil {
		s.appendEvent(from, target.Name, reason, "切换失败: "+err.Error())
		log.Printf("failover %s 切换失败（%s→%s）: %v", s.Domain, from, target.Name, err)
		return err
	}
	s.currentIndex = index
	if reason == "manual" {
		s.state = StateManual
		s.manualIndex = index
	}
	s.appendEvent(from, target.Name, reason, detail)
	log.Printf("failover %s: DNS 切到 %s（%s, %s）", s.Domain, target.Name, reason, detail)
	return nil
}

func (s *Site) appendEvent(from, to, reason, detail string) {
	s.events = append(s.events, SwitchEvent{
		Time:   time.Now().Format("15:04:05"),
		From:   from,
		To:     to,
		Reason: reason,
		Detail: detail,
	})
	if len(s.events) > s.maxEvents {
		s.events = s.events[len(s.events)-s.maxEvents:]
	}
}

// matchIndex 按 DNS 记录（type+content）匹配目标下标；-1 表示不匹配任何目标
func (s *Site) matchIndex(rt, content string) int {
	for i := range s.Targets {
		if s.Targets[i].RecordType == rt && s.Targets[i].DNSContent == content {
			return i
		}
	}
	return -1
}

// ManualSwitch 手动切换到指定目标（队列下标），进入手动模式
func (s *Site) ManualSwitch(index int) error {
	if index < 0 || index >= len(s.Targets) {
		return fmt.Errorf("目标下标越界（0-%d）", len(s.Targets)-1)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.doSwitch(index, "manual", "面板手动切换")
}

// ManualAuto 退出手动模式：按当前 DNS 记录实际指向恢复自动
func (s *Site) ManualAuto() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.client.GetRecord(s.zoneID, s.recordID)
	if err != nil {
		return err
	}
	idx := s.matchIndex(rec.Type, rec.Content)
	if idx < 0 {
		idx = 0
	}
	s.currentIndex = idx
	s.state = StateAuto
	s.manualIndex = -1
	log.Printf("failover %s: 恢复自动（当前 DNS 指向 %s）", s.Domain, rec.Content)
	return nil
}

// SyncActual 启动时对齐实际 DNS 指向（调用方持锁或构造后调用）
func (s *Site) SyncActual() error {
	rec, err := s.client.GetRecord(s.zoneID, s.recordID)
	if err != nil {
		return err
	}
	if idx := s.matchIndex(rec.Type, rec.Content); idx >= 0 {
		s.currentIndex = idx
	}
	log.Printf("failover %s: 启动对齐，当前 DNS 指向 %s（目标 %s）", s.Domain, rec.Content, s.Targets[s.currentIndex].Name)
	return nil
}

// Snapshot 导出面板状态
func (s *Site) Snapshot() SiteStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	cd := 0
	if !s.cooldownDone() {
		cd = int(time.Until(s.cooldownUntil).Seconds())
	}
	tr := make([]TargetStatus, len(s.targetResults))
	copy(tr, s.targetResults)
	evs := make([]SwitchEvent, len(s.events))
	copy(evs, s.events)
	return SiteStatus{
		Domain:       s.Domain,
		State:        s.state,
		Current:      s.Targets[s.currentIndex].Name,
		CurrentIndex: s.currentIndex,
		Targets:      tr,
		CooldownLeft: cd,
		Events:       evs,
	}
}

// Manager 所有轮换站点的调度器
type Manager struct {
	sites    []*Site
	interval time.Duration
}

// NewManager 构造调度器。interval 为探测间隔（秒）。
func NewManager(sites []*Site, interval int) *Manager {
	d := time.Duration(interval) * time.Second
	if d <= 0 {
		d = 10 * time.Second
	}
	return &Manager{sites: sites, interval: d}
}

// Sites 返回全部站点
func (m *Manager) Sites() []*Site { return m.sites }

// Site 按域名取站点
func (m *Manager) Site(domain string) *Site {
	for _, s := range m.sites {
		if strings.EqualFold(s.Domain, domain) {
			return s
		}
	}
	return nil
}

// Start 启动探测循环
func (m *Manager) Start(ctx context.Context) {
	go m.loop(ctx)
}

func (m *Manager) loop(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.tickAll()
		}
	}
}

// tickAll 逐站 tick；每站并发执行（探测/配额查询互不阻塞）
func (m *Manager) tickAll() {
	var wg sync.WaitGroup
	for _, s := range m.sites {
		wg.Add(1)
		go func(s *Site) {
			defer wg.Done()
			s.mu.Lock()
			s.tick()
			s.mu.Unlock()
		}(s)
	}
	wg.Wait()
}

// Snapshot 全部站点状态
func (m *Manager) Snapshot() []SiteStatus {
	st := make([]SiteStatus, 0, len(m.sites))
	for _, s := range m.sites {
		st = append(st, s.Snapshot())
	}
	return st
}

// BuildSites 从配置构建轮换站点列表（含 zone_id/record_id 解析）。
// 仅包含配置了 Targets 的站点。accounts 为配额账号池（settings.cf_accounts）。
func BuildSites(cfg *config.Config, client *dns.Client, accounts []config.CFAccount) ([]*Site, error) {
	var sites []*Site
	for _, sc := range cfg.Sites {
		if len(sc.Targets) == 0 {
			continue // 旧转发站点，不归 failover 管
		}
		zoneID, err := client.ZoneID(cfg.DNS.Zone)
		if err != nil {
			return nil, fmt.Errorf("site %s: %w", sc.Domain, err)
		}
		recordID, err := client.RecordID(zoneID, sc.Domain)
		if err != nil {
			return nil, fmt.Errorf("site %s: %w", sc.Domain, err)
		}
		s, err := NewSite(sc.Domain, sc.Targets, accounts, sc.Probe, client, zoneID, recordID, cfg.DNS.TTL, cfg.DNS.DryRun, cf.QueryUsage)
		if err != nil {
			return nil, err
		}
		if err := s.SyncActual(); err != nil {
			log.Printf("failover %s: 启动对齐失败（继续启动，首次切换时重试）: %v", sc.Domain, err)
		}
		sites = append(sites, s)
	}
	return sites, nil
}

// 保证 os 被引用（urlQuery 相关预留）
var _ = os.Getenv
