package main

import (
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

// QueryCFUsage 查询单个账号当月 Workers 请求数（REST Analytics API）
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
	since := monthStart.Format("2006-01-02")
	until := now.Format("2006-01-02")

	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/workers/analytics/daily?since=%s&until=%s",
		acc.AccountID, since, until)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return CFUsage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+acc.Token)

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
		Success bool `json:"success"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
		Result []struct {
			Date     string `json:"date"`
			Requests struct {
				All int64 `json:"all"`
			} `json:"requests"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return CFUsage{}, fmt.Errorf("解析 CF 响应失败: %w", err)
	}
	if !parsed.Success {
		if len(parsed.Errors) > 0 {
			return CFUsage{}, fmt.Errorf("CF API: %s", parsed.Errors[0].Message)
		}
		return CFUsage{}, fmt.Errorf("CF API: 请求失败（HTTP %d）", resp.StatusCode)
	}

	used := int64(0)
	for _, r := range parsed.Result {
		used += r.Requests.All
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
