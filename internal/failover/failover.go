// Package failover DNS 故障切换状态机（新架构控制面核心）。
//
// 职责：按探测间隔检查每站主/备目标健康，主连续失败则调 Cloudflare DNS API
// 把记录切到备，主恢复稳定后切回；支持手动接管与切换历史。数据面不经过本包。
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

	"github.com/wu529778790/edge-balancer/internal/config"
	"github.com/wu529778790/edge-balancer/internal/dns"
)

// State 站点切换状态
type State string

const (
	StateActive     State = "active"      // DNS 指向主
	StateFailedOver State = "failed_over" // DNS 指向备
	StateManual     State = "manual"      // 手动接管（不自动切换）
)

// SwitchEvent 一次切换记录
type SwitchEvent struct {
	Time   string `json:"time"`
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"` // auto / manual
	Detail string `json:"detail"`
}

// TargetStatus 单个目标的探测状态（面板）
type TargetStatus struct {
	Name       string `json:"name"`
	OK         bool   `json:"ok"`
	Latency    string `json:"latency"`
	Detail     string `json:"detail"`
	RecordType string `json:"record_type"`
	DNSContent string `json:"dns_content"`
}

// SiteStatus 单站状态快照（面板）
type SiteStatus struct {
	Domain       string        `json:"domain"`
	State        State         `json:"state"`
	ManualTarget string        `json:"manual_target"`
	Primary      TargetStatus  `json:"primary"`
	Backup       TargetStatus  `json:"backup"`
	CooldownLeft int           `json:"cooldown_left"`
	Events       []SwitchEvent `json:"events"`
}

// Switcher DNS 记录读写接口（dns.Client 实现；测试可用 fake 替换，避免真调 CF API）
type Switcher interface {
	PatchRecord(zoneID, recordID, rtype, content string, ttl int, keepProxy bool) (*dns.Record, error)
	GetRecord(zoneID, recordID string) (*dns.Record, error)
}

// Site 单站故障切换运行实例
type Site struct {
	Domain   string
	Primary  config.TargetConfig
	Backup   config.TargetConfig
	Probe    config.ProbeConfig
	dnsTTL   int
	zoneID   string
	recordID string
	client   Switcher
	dryRun   bool // 监控模式：决策正常执行，但不实际调用 CF API 切换

	mu           sync.Mutex
	state        State
	manualTarget string // StateManual 时指定指向（primary/backup）

	failStreak      int // 当前主连续失败次数
	okStreak        int // 备状态时主连续成功次数（判恢复）
	backupFailStreak int // 备状态时备连续失败次数
	cooldownUntil   time.Time

	primaryResult TargetStatus
	backupResult  TargetStatus
	events        []SwitchEvent
	maxEvents     int
}

// NewSite 构造单站。recordID 由 Manager 注入（构造时解析并缓存）。
// dryRun=true 时进入监控模式：探测与决策照常，但不实际切换 DNS。
func NewSite(domain string, primary, backup config.TargetConfig, probe config.ProbeConfig, client Switcher, zoneID, recordID string, dnsTTL int, dryRun bool) (*Site, error) {
	if client == nil {
		return nil, fmt.Errorf("site %s: dns client 未初始化", domain)
	}
	if zoneID == "" || recordID == "" {
		return nil, fmt.Errorf("site %s: zone_id / record_id 未解析", domain)
	}
	return &Site{
		Domain:     domain,
		Primary:    primary,
		Backup:     backup,
		Probe:      probe,
		dnsTTL:     dnsTTL,
		zoneID:     zoneID,
		recordID:   recordID,
		client:     client,
		dryRun:     dryRun,
		state:      StateActive,
		maxEvents:  50,
	}, nil
}

// probeTarget 探测单个目标。server 模式：确定性失败（连接错误/5xx）判失败；
// 超时视为「慢」不判挂（服务器侧线路差不等于目标挂）。
func probeTarget(t *config.TargetConfig, healthPath string, timeout time.Duration) TargetStatus {
	st := TargetStatus{Name: t.Name, RecordType: t.RecordType, DNSContent: t.DNSContent}
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
	if resp.StatusCode >= 500 {
		st.OK = false
		st.Detail = fmt.Sprintf("HTTP %d", resp.StatusCode)
		return st
	}
	st.OK = true
	st.Detail = fmt.Sprintf("HTTP %d", resp.StatusCode)
	return st
}

// tick 一次探测与状态迁移（调用方持锁）
func (s *Site) tick() {
	if s.state == StateManual {
		return
	}
	timeout := time.Duration(s.Probe.Timeout) * time.Second
	p := probeTarget(&s.Primary, s.Primary.Health, timeout)
	b := probeTarget(&s.Backup, s.Backup.Health, timeout)
	s.primaryResult = p
	s.backupResult = b

	switch s.state {
	case StateActive:
		if !p.OK {
			s.failStreak++
			if s.failStreak >= s.Probe.FailThreshold && b.OK && s.cooldownDone() {
				s.doSwitch("backup", "auto", fmt.Sprintf("主连续失败 %d 次（%s），备健康，切到备", s.failStreak, p.Detail))
			}
		} else {
			s.failStreak = 0
		}
	case StateFailedOver:
		if !b.OK {
			s.backupFailStreak++
		} else {
			s.backupFailStreak = 0
		}
		if p.OK {
			s.okStreak++
			if s.okStreak >= s.Probe.RecoverThreshold && s.cooldownDone() {
				s.doSwitch("primary", "auto", fmt.Sprintf("主恢复稳定 %d 次，切回主", s.okStreak))
			}
		} else {
			s.okStreak = 0
		}
	}
}

// cooldownDone 冷却是否已过
func (s *Site) cooldownDone() bool {
	return time.Now().After(s.cooldownUntil)
}

// doSwitch 执行切换：PATCH DNS 记录 + 更新状态（调用方持锁）。
// 目标为 primary/backup；reason 为 auto/manual。
func (s *Site) doSwitch(target, reason, detail string) error {
	t := &s.Primary
	to := StateActive
	if target == "backup" {
		t = &s.Backup
		to = StateFailedOver
	}
	from := s.currentTargetName()
	s.cooldownUntil = time.Now().Add(time.Duration(s.Probe.Cooldown) * time.Second)
	s.failStreak, s.okStreak, s.backupFailStreak = 0, 0, 0
	s.state = to
	if reason == "manual" {
		s.manualTarget = target
		s.state = StateManual
	}
	s.events = append(s.events, SwitchEvent{
		Time:   time.Now().Format("15:04:05"),
		From:   from,
		To:     t.Name,
		Reason: reason,
		Detail: detail,
	})
	if len(s.events) > s.maxEvents {
		s.events = s.events[len(s.events)-s.maxEvents:]
	}
	if s.dryRun {
		log.Printf("failover %s [dry-run]: 决策切到 %s（%s, %s），未实际修改 DNS", s.Domain, t.Name, reason, detail)
		return nil
	}
	_, err := s.client.PatchRecord(s.zoneID, s.recordID, t.RecordType, t.DNSContent, s.dnsTTL, true)
	if err != nil {
		log.Printf("failover %s 切换失败（%s→%s）: %v", s.Domain, from, t.Name, err)
		return err
	}
	log.Printf("failover %s: DNS 切到 %s（%s, %s）", s.Domain, t.Name, reason, detail)
	return nil
}

// currentTargetName 当前状态对应的目标名
func (s *Site) currentTargetName() string {
	if s.state == StateFailedOver {
		return s.Backup.Name
	}
	if s.state == StateManual && s.manualTarget == "backup" {
		return s.Backup.Name
	}
	return s.Primary.Name
}

// ManualSwitch 手动切换到指定目标（primary/backup），进入手动模式
func (s *Site) ManualSwitch(target string) error {
	if target != "primary" && target != "backup" {
		return fmt.Errorf("目标必须是 primary 或 backup")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.doSwitch(target, "manual", "面板手动切换")
}

// ManualAuto 退出手动模式：按当前 DNS 记录实际指向恢复自动
func (s *Site) ManualAuto() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.client.GetRecord(s.zoneID, s.recordID)
	if err != nil {
		return err
	}
	s.state = StateActive
	if rec.Content == s.Backup.DNSContent && rec.Type == s.Backup.RecordType {
		s.state = StateFailedOver
	}
	s.manualTarget = ""
	log.Printf("failover %s: 恢复自动（当前 DNS 指向 %s）", s.Domain, rec.Content)
	return nil
}

// SyncActual 启动时对齐实际 DNS 指向（调用方持锁或构造后调用）
func (s *Site) SyncActual() error {
	rec, err := s.client.GetRecord(s.zoneID, s.recordID)
	if err != nil {
		return err
	}
	s.state = StateActive
	if rec.Content == s.Backup.DNSContent && rec.Type == s.Backup.RecordType {
		s.state = StateFailedOver
	}
	s.primaryResult = TargetStatus{Name: s.Primary.Name, RecordType: s.Primary.RecordType, DNSContent: s.Primary.DNSContent}
	s.backupResult = TargetStatus{Name: s.Backup.Name, RecordType: s.Backup.RecordType, DNSContent: s.Backup.DNSContent}
	log.Printf("failover %s: 启动对齐，当前 DNS 指向 %s（%s）", s.Domain, rec.Content, s.state)
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
	evs := make([]SwitchEvent, len(s.events))
	copy(evs, s.events)
	p := s.primaryResult
	if p.Name == "" {
		p = TargetStatus{Name: s.Primary.Name, RecordType: s.Primary.RecordType, DNSContent: s.Primary.DNSContent}
	}
	b := s.backupResult
	if b.Name == "" {
		b = TargetStatus{Name: s.Backup.Name, RecordType: s.Backup.RecordType, DNSContent: s.Backup.DNSContent}
	}
	return SiteStatus{
		Domain:       s.Domain,
		State:        s.state,
		ManualTarget: s.manualTarget,
		Primary:      p,
		Backup:       b,
		CooldownLeft: cd,
		Events:       evs,
	}
}

// Manager 所有 failover 站点的调度器
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

func (m *Manager) tickAll() {
	for _, s := range m.sites {
		s.mu.Lock()
		s.tick()
		s.mu.Unlock()
	}
}

// Snapshot 全部站点状态
func (m *Manager) Snapshot() []SiteStatus {
	st := make([]SiteStatus, 0, len(m.sites))
	for _, s := range m.sites {
		st = append(st, s.Snapshot())
	}
	return st
}

// BuildSites 从配置构建 failover 站点列表（含 zone_id/record_id 解析）。
// 仅包含配置了 primary/backup 的站点。
func BuildSites(cfg *config.Config, client *dns.Client) ([]*Site, error) {
	var sites []*Site
	for _, sc := range cfg.Sites {
		if sc.Primary.Name == "" || sc.Backup.Name == "" {
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
		s, err := NewSite(sc.Domain, sc.Primary, sc.Backup, sc.Probe, client, zoneID, recordID, cfg.DNS.TTL, cfg.DNS.DryRun)
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
