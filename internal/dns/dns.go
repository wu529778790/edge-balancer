// Package dns Cloudflare DNS 记录切换客户端（新架构：DNS 故障切换）。
//
// 职责：按 zone 名解析 zone_id、按记录名解析 record_id（缓存）、
// PATCH 更新记录（type + content + ttl + proxied）。token 从环境变量读取，
// 最小权限：Zone.Zone:Read + Zone.DNS:Read + Zone.DNS:Edit。
package dns

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const apiBase = "https://api.cloudflare.com/client/v4"

// Record 一条 DNS 记录（切换涉及的最小字段）
type Record struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied *bool  `json:"proxied,omitempty"`
}

// Client Cloudflare DNS API 客户端
type Client struct {
	token   string
	http    *http.Client
	zoneID  string // 已解析的 zone_id（缓存）
	mu      sync.Mutex
	zone    string
	records map[string]string // 记录名 -> record_id 缓存
}

// New 构造客户端。tokenEnv 为读取 token 的环境变量名（默认 CF_API_TOKEN）。
func New(tokenEnv string) (*Client, error) {
	env := tokenEnv
	if env == "" {
		env = "CF_API_TOKEN"
	}
	token := os.Getenv(env)
	if token == "" {
		return nil, fmt.Errorf("环境变量 %s 未设置（CF API token）", env)
	}
	return &Client{
		token:   token,
		http:    &http.Client{Timeout: 15 * time.Second},
		records: make(map[string]string),
	}, nil
}

// do 统一请求：自动带 Authorization，2xx 返回 result 原始字节
func (c *Client) do(method, path string, body interface{}) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, apiBase+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Success bool            `json:"success"`
		Errors  []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("解析 CF 响应: %s", truncate(string(data), 300))
	}
	if !envelope.Success {
		msgs := make([]string, 0, len(envelope.Errors))
		for _, e := range envelope.Errors {
			msgs = append(msgs, fmt.Sprintf("%d:%s", e.Code, e.Message))
		}
		return nil, fmt.Errorf("CF API 失败: %s", strings.Join(msgs, "; "))
	}
	return envelope.Result, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ZoneID 解析并缓存 zone_id（按 zone 名精确匹配）
func (c *Client) ZoneID(zone string) (string, error) {
	if c.zoneID != "" && c.zone == zone {
		return c.zoneID, nil
	}
	raw, err := c.do(http.MethodGet, "/zones?name="+urlQuery(zone)+"&per_page=5", nil)
	if err != nil {
		return "", err
	}
	var zones []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &zones); err != nil {
		return "", err
	}
	for _, z := range zones {
		if strings.EqualFold(z.Name, zone) {
			c.mu.Lock()
			c.zoneID = z.ID
			c.zone = zone
			c.mu.Unlock()
			return z.ID, nil
		}
	}
	return "", fmt.Errorf("zone %s 不存在或 token 无权限（Zone.Zone:Read）", zone)
}

// RecordID 解析并缓存 record_id（按记录名匹配，取第一条非 NS/SOA）
func (c *Client) RecordID(zoneID, name string) (string, error) {
	if id, ok := c.records[name]; ok {
		return id, nil
	}
	raw, err := c.do(http.MethodGet, "/zones/"+zoneID+"/dns_records?name="+urlQuery(name)+"&per_page=10", nil)
	if err != nil {
		return "", err
	}
	var recs []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &recs); err != nil {
		return "", err
	}
	for _, r := range recs {
		if r.Type == "NS" || r.Type == "SOA" {
			continue
		}
		c.mu.Lock()
		c.records[name] = r.ID
		c.mu.Unlock()
		return r.ID, nil
	}
	return "", fmt.Errorf("记录 %s 不存在或 token 无权限（Zone.DNS:Read）", name)
}

// GetRecord 读取记录当前值（用于校验与面板展示）
func (c *Client) GetRecord(zoneID, recordID string) (*Record, error) {
	raw, err := c.do(http.MethodGet, "/zones/"+zoneID+"/dns_records/"+recordID, nil)
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// PatchRecord 更新记录（type/content/ttl/proxied）。keepProxy 为 true 时保持原 proxied。
func (c *Client) PatchRecord(zoneID, recordID, rtype, content string, ttl int, keepProxy bool) (*Record, error) {
	cur, err := c.GetRecord(zoneID, recordID)
	if err != nil {
		return nil, err
	}
	body := map[string]interface{}{
		"type":    rtype,
		"content": content,
		"ttl":     ttl,
	}
	if keepProxy {
		p := cur.Proxied != nil && *cur.Proxied
		body["proxied"] = p
	}
	raw, err := c.do(http.MethodPatch, "/zones/"+zoneID+"/dns_records/"+recordID, body)
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func urlQuery(s string) string {
	// 简单 URL 编码（域名只含合法字符，直接替换即可）
	return strings.ReplaceAll(s, "%", "%25")
}

// WorkerRoute 一条 Workers Route（可编程切换 worker ⇄ 服务器兜底的关键）
type WorkerRoute struct {
	ID      string `json:"id"`
	Pattern string `json:"pattern"`
	Script  string `json:"script"`
}

// ListRoutes 列出 zone 下全部 Workers Routes
func (c *Client) ListRoutes(zoneID string) ([]WorkerRoute, error) {
	raw, err := c.do(http.MethodGet, "/zones/"+zoneID+"/workers/routes", nil)
	if err != nil {
		return nil, err
	}
	var routes []WorkerRoute
	if err := json.Unmarshal(raw, &routes); err != nil {
		return nil, err
	}
	return routes, nil
}

// findRoute 按 pattern 精确匹配现有 route
func (c *Client) findRoute(zoneID, pattern string) (*WorkerRoute, error) {
	routes, err := c.ListRoutes(zoneID)
	if err != nil {
		return nil, err
	}
	for i := range routes {
		if routes[i].Pattern == pattern {
			return &routes[i], nil
		}
	}
	return nil, nil
}

// PutRoute 确保 pattern 指向 script（不存在则创建，存在则更新目标 worker）
func (c *Client) PutRoute(zoneID, pattern, script string) error {
	cur, err := c.findRoute(zoneID, pattern)
	if err != nil {
		return err
	}
	body := map[string]string{"pattern": pattern, "script": script}
	if cur != nil {
		if cur.Script == script {
			return nil // 已指向目标，幂等
		}
		_, err = c.do(http.MethodPut, "/zones/"+zoneID+"/workers/routes/"+cur.ID, body)
		return err
	}
	_, err = c.do(http.MethodPost, "/zones/"+zoneID+"/workers/routes", body)
	return err
}

// DeleteRoute 删除匹配 pattern 的 route（存在则删，不存在幂等）
func (c *Client) DeleteRoute(zoneID, pattern string) error {
	cur, err := c.findRoute(zoneID, pattern)
	if err != nil {
		return err
	}
	if cur == nil {
		return nil
	}
	_, err = c.do(http.MethodDelete, "/zones/"+zoneID+"/workers/routes/"+cur.ID, nil)
	return err
}
