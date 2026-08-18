package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// CFUsage 单个 Cloudflare 账号的用量
type CFUsage struct {
	Name      string  `json:"name"`
	Used      int64   `json:"used"`
	Quota     int64   `json:"quota"`
	Percent   float64 `json:"percent"`
	OverLimit bool    `json:"over_limit"`
	AutoOff   bool    `json:"auto_off"` // 当前是否已被配额自动停用
	Error     string  `json:"error,omitempty"`
}

// QueryCFUsage 查询单个账号当月 Workers 请求数（GraphQL Analytics API）
func QueryCFUsage(acc CFAccount) (CFUsage, error) {
	quota := acc.Quota
	if quota <= 0 {
		quota = 100000
	}
	threshold := acc.Threshold
	if threshold <= 0 {
		threshold = 90
	}

	now := time.Now()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	date := monthStart.Format("2006-01-02")

	query := fmt.Sprintf(
		`query { viewer { accounts(filter: {accountTag: "%s"}) { workersInvocationsAdaptiveGroups(limit: 1, filter: {date_geq: "%s"}) { sum { requests } } } } }`,
		acc.AccountID, date)

	payload, _ := json.Marshal(map[string]string{"query": query})
	req, err := http.NewRequest(http.MethodPost, "https://api.cloudflare.com/client/v4/graphql", bytes.NewReader(payload))
	if err != nil {
		return CFUsage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+acc.Token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return CFUsage{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return CFUsage{}, err
	}

	var parsed struct {
		Data struct {
			Viewer struct {
				Accounts []struct {
					WorkersInvocations []struct {
						Sum struct {
							Requests int64 `json:"requests"`
						} `json:"sum"`
					} `json:"workersInvocationsAdaptiveGroups"`
				} `json:"accounts"`
			} `json:"viewer"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return CFUsage{}, fmt.Errorf("解析 CF 响应失败: %w", err)
	}
	if len(parsed.Errors) > 0 {
		return CFUsage{}, fmt.Errorf("CF API: %s", parsed.Errors[0].Message)
	}

	used := int64(0)
	if len(parsed.Data.Viewer.Accounts) > 0 && len(parsed.Data.Viewer.Accounts[0].WorkersInvocations) > 0 {
		used = parsed.Data.Viewer.Accounts[0].WorkersInvocations[0].Sum.Requests
	}
	percent := float64(used) / float64(quota) * 100
	return CFUsage{
		Name:      acc.Name,
		Used:      used,
		Quota:     quota,
		Percent:   percent,
		OverLimit: percent >= float64(threshold),
	}, nil
}

// QueryAllCFUsages 并发查询所有账号用量
func QueryAllCFUsages(accounts []CFAccount) []CFUsage {
	usages := make([]CFUsage, len(accounts))
	done := make(chan struct{}, len(accounts))
	for i, acc := range accounts {
		go func(i int, acc CFAccount) {
			defer func() { done <- struct{}{} }()
			u, err := QueryCFUsage(acc)
			if err != nil {
				u = CFUsage{Name: acc.Name, Error: err.Error()}
			}
			usages[i] = u
		}(i, acc)
	}
	for range accounts {
		<-done
	}
	return usages
}
