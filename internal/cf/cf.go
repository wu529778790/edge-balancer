// Package cf Cloudflare Workers 用量查询（GraphQL Analytics API）。
package cf

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/wu529778790/edge-balancer/internal/config"
)

// Usage 单个 Cloudflare 账号的用量
type Usage struct {
	Name      string  `json:"name"`
	Used      int64   `json:"used"`
	Quota     int64   `json:"quota"`
	Percent   float64 `json:"percent"`
	OverLimit bool    `json:"over_limit"`
	AutoOff   bool    `json:"auto_off"` // 当前是否已被配额自动停用
	Error     string  `json:"error,omitempty"`
}

// QueryUsage 查询单个配额账号当前周期的用量（GraphQL Analytics API）。
// 免费版限额 100,000 请求/天（非每月）；Period=daily 查当天，monthly 查当月累计（date_geq=当月 1 号）。
// 注意：字段名为 workersInvocationsAdaptive（无 Groups 后缀，文档有误）。
// Provider=cloudflare 走 GraphQL；其他平台（vercel/netlify/deno/edgeone/fastly）架构已就绪但未接入，
// 返回明确错误，面板显示"查询失败: provider xxx 未适配"。
// token 来源：优先 acc.TokenEnv 指定的环境变量（推荐，不落盘），回退 acc.Token（旧明文字段）。
func QueryUsage(acc config.CFAccount) (Usage, error) {
	config.NormalizeCFAccount(&acc)
	switch acc.Provider {
	case "", "cloudflare":
		return queryCloudflare(acc)
	default:
		return Usage{}, fmt.Errorf("provider %s 未适配（架构已就绪，待接入该平台用量 API）", acc.Provider)
	}
}

// queryCloudflare CF 平台用量查询（GraphQL Analytics）
func queryCloudflare(acc config.CFAccount) (Usage, error) {
	token := acc.Token
	if acc.TokenEnv != "" {
		if v := os.Getenv(acc.TokenEnv); v != "" {
			token = v
		}
	}
	if token == "" {
		return Usage{}, fmt.Errorf("账号 %s 未配置 token（TokenEnv 环境变量 %q 或 Token 字段）", acc.Name, acc.TokenEnv)
	}

	now := time.Now()
	from, to := periodRange(acc.Period, now)

	query := fmt.Sprintf(
		`query { viewer { accounts(filter: {accountTag: "%s"}) { workersInvocationsAdaptive(limit: 1, filter: {date_geq: "%s", date_leq: "%s"}) { sum { requests } } } } }`,
		acc.AccountID, from, to)

	payload, _ := json.Marshal(map[string]string{"query": query})
	req, err := http.NewRequest(http.MethodPost, "https://api.cloudflare.com/client/v4/graphql", bytes.NewReader(payload))
	if err != nil {
		return Usage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Usage{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return Usage{}, err
	}

	var parsed struct {
		Data struct {
			Viewer struct {
				Accounts []struct {
					WorkersInvocations []struct {
						Sum struct {
							Requests int64 `json:"requests"`
						} `json:"sum"`
					} `json:"workersInvocationsAdaptive"`
				} `json:"accounts"`
			} `json:"viewer"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return Usage{}, fmt.Errorf("解析 CF 响应失败: %w", err)
	}
	if len(parsed.Errors) > 0 {
		return Usage{}, fmt.Errorf("CF API: %s", parsed.Errors[0].Message)
	}

	used := int64(0)
	if len(parsed.Data.Viewer.Accounts) > 0 && len(parsed.Data.Viewer.Accounts[0].WorkersInvocations) > 0 {
		used = parsed.Data.Viewer.Accounts[0].WorkersInvocations[0].Sum.Requests
	}
	percent := float64(used) / float64(acc.Quota) * 100
	return Usage{
		Name:      acc.Name,
		Used:      used,
		Quota:     acc.Quota,
		Percent:   percent,
		OverLimit: percent >= float64(acc.Threshold),
	}, nil
}

// periodRange 按周期计算查询窗口（date_geq / date_leq）
func periodRange(period string, now time.Time) (string, string) {
	to := now.Format("2006-01-02")
	switch period {
	case "monthly":
		from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).Format("2006-01-02")
		return from, to
	default: // daily
		return to, to
	}
}

// QueryAllUsages 并发查询所有账号用量
func QueryAllUsages(accounts []config.CFAccount) []Usage {
	usages := make([]Usage, len(accounts))
	done := make(chan struct{}, len(accounts))
	for i, acc := range accounts {
		go func(i int, acc config.CFAccount) {
			defer func() { done <- struct{}{} }()
			u, err := QueryUsage(acc)
			if err != nil {
				u = Usage{Name: acc.Name, Error: err.Error()}
			}
			usages[i] = u
		}(i, acc)
	}
	for range accounts {
		<-done
	}
	return usages
}
