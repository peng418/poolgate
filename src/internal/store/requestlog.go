package store

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// RequestRecord 是一条请求流水（F6.1）。Channel/Account/Model/Status/耗时/错误分类。
type RequestRecord struct {
	Time     time.Time `json:"time"`
	Channel  string    `json:"channel"`
	Account  string    `json:"account"`
	Model    string    `json:"model"`
	Status   string    `json:"status"` // ok | error
	ErrKind  string    `json:"err_kind,omitempty"`
	Duration int64     `json:"duration_ms"`
	TTFT     int64     `json:"ttft_ms,omitempty"`
	// Note 是人话补充，如「换号后成功」「上游原话摘要」。
	// 失败行必须能在这里看到原因（红线一：零静默失败）。
	Note string `json:"note,omitempty"`
}

// RequestLog 请求流水存储（内存环形，保留最近 max 条）。
type RequestLog struct {
	mu      sync.Mutex
	records []RequestRecord
	max     int
}

// NewRequestLog 建立请求流水存储。
func NewRequestLog(max int) *RequestLog {
	if max <= 0 {
		max = 500
	}
	return &RequestLog{max: max}
}

// Add 追加一条；超出上限丢弃最旧。
func (l *RequestLog) Add(r RequestRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, r)
	if len(l.records) > l.max {
		l.records = l.records[len(l.records)-l.max:]
	}
}

// List 返回全部（时间正序）。
func (l *RequestLog) List() []RequestRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]RequestRecord, len(l.records))
	copy(out, l.records)
	return out
}

// Clear 清空流水，返回清掉的条数（设置屏危险区）。
func (l *RequestLog) Clear() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(l.records)
	l.records = nil
	return n
}

// TodayStats 返回今日请求数、失败数，以及成功请求的 TTFT 列表（总览指标卡用）。
func (l *RequestLog) TodayStats() (requests, failed int, ttfts []int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	y, m, d := now.Date()
	start := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	for _, r := range l.records {
		if r.Time.Before(start) {
			continue
		}
		requests++
		if r.Status == "error" {
			failed++
			continue
		}
		if r.TTFT > 0 {
			ttfts = append(ttfts, r.TTFT)
		}
	}
	return requests, failed, ttfts
}

// DailyPoint 是某一天的请求量汇总（原型 01-overview 的「近 7 日请求量」）。
type DailyPoint struct {
	Date     string `json:"date"` // MM-DD
	Requests int    `json:"requests"`
	Failed   int    `json:"failed"`
	TTFTms   int64  `json:"ttft_ms"`
}

// Summary 是面板指标卡要的一组聚合（原型 01-overview / 05-logs 的四张卡）。
type Summary struct {
	TodayRequests  int   `json:"today_requests"`
	TodayFailed    int   `json:"today_failed"`
	TodayTTFTms    int64 `json:"today_ttft_ms"`
	TodayP95TTFTms int64 `json:"today_p95_ttft_ms"`
	YesterdayFail  int   `json:"yesterday_failed"`
	// UpstreamFail/AccountFail 用于「失败构成」卡：上游故障与账号故障必须分开。
	UpstreamFail int `json:"upstream_failed"`
	AccountFail  int `json:"account_failed"`
	// Rotations 是自动换号次数，RotatedOK 是其中换号后成功的次数。
	Rotations int `json:"rotations"`
	RotatedOK int `json:"rotated_ok"`
}

// Summary 计算面板指标卡所需的聚合值。
func (l *RequestLog) Summary() Summary {
	l.mu.Lock()
	defer l.mu.Unlock()
	var s Summary
	now := time.Now()
	todayStart := dayStart(now)
	yStart := todayStart.AddDate(0, 0, -1)

	var ttfts []int64
	for _, r := range l.records {
		if strings.Contains(r.Note, "换号") {
			s.Rotations++
			if r.Status == "ok" {
				s.RotatedOK++
			}
		}
		if r.Time.Before(yStart) {
			continue
		}
		if r.Time.Before(todayStart) {
			if r.Status == "error" {
				s.YesterdayFail++
			}
			continue
		}
		s.TodayRequests++
		if r.Status == "error" {
			s.TodayFailed++
			// 上游类 vs 账号类：UpstreamFault/Transport/Parse 算上游，其余算账号。
			switch errsKindClass(r.ErrKind) {
			case "upstream":
				s.UpstreamFail++
			default:
				s.AccountFail++
			}
			continue
		}
		if r.TTFT > 0 {
			ttfts = append(ttfts, r.TTFT)
		}
	}
	s.TodayTTFTms = medianInt64(ttfts)
	s.TodayP95TTFTms = percentileInt64(ttfts, 95)
	return s
}

// Daily 返回最近 n 天的每日汇总（时间正序，含无数据的空白日）。
func (l *RequestLog) Daily(n int) []DailyPoint {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n <= 0 {
		n = 7
	}
	now := time.Now()
	out := make([]DailyPoint, 0, n)
	// 先建好连续日期骨架，避免「某天没请求就整列消失」。
	byDate := map[string]*DailyPoint{}
	var ttfts = map[string][]int64{}
	for i := n - 1; i >= 0; i-- {
		d := now.AddDate(0, 0, -i)
		key := d.Format("01-02")
		out = append(out, DailyPoint{Date: key})
		byDate[key] = &out[len(out)-1]
	}
	for _, r := range l.records {
		key := r.Time.Format("01-02")
		p, ok := byDate[key]
		if !ok {
			continue // 超出窗口
		}
		p.Requests++
		if r.Status == "error" {
			p.Failed++
			continue
		}
		if r.TTFT > 0 {
			ttfts[key] = append(ttfts[key], r.TTFT)
		}
	}
	for key, xs := range ttfts {
		byDate[key].TTFTms = medianInt64(xs)
	}
	return out
}

func dayStart(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// errsKindClass 把错误分类归到「上游类 / 账号类」，供失败构成统计。
func errsKindClass(kind string) string {
	switch kind {
	case "UpstreamFault", "Transport", "Parse":
		return "upstream"
	default:
		return "account"
	}
}

func medianInt64(xs []int64) int64 {
	if len(xs) == 0 {
		return 0
	}
	cp := append([]int64(nil), xs...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	return cp[len(cp)/2]
}

// percentileInt64 取 p 分位（p 取 1–100）；样本不足时退回最大值。
func percentileInt64(xs []int64, p int) int64 {
	if len(xs) == 0 {
		return 0
	}
	cp := append([]int64(nil), xs...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := (len(cp)*p + 99) / 100
	if idx < 1 {
		idx = 1
	}
	if idx > len(cp) {
		idx = len(cp)
	}
	return cp[idx-1]
}

// ChannelStats 返回每个渠道的 TTFT 中位与样本数（总览条形图用）。
func (l *RequestLog) ChannelStats() map[string]struct {
	MedianTTFTms int64 `json:"median_ttft_ms"`
	Samples      int   `json:"samples"`
} {
	l.mu.Lock()
	defer l.mu.Unlock()
	byCh := map[string][]int64{}
	for _, r := range l.records {
		if r.Status == "ok" && r.TTFT > 0 {
			byCh[r.Channel] = append(byCh[r.Channel], r.TTFT)
		}
	}
	out := map[string]struct {
		MedianTTFTms int64 `json:"median_ttft_ms"`
		Samples      int   `json:"samples"`
	}{}
	for ch, xs := range byCh {
		out[ch] = struct {
			MedianTTFTms int64 `json:"median_ttft_ms"`
			Samples      int   `json:"samples"`
		}{medianInt64(xs), len(xs)}
	}
	return out
}

// Stats 按错误分类聚合计数（F6.2）。
func (l *RequestLog) Stats() map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	m := map[string]int{}
	for _, r := range l.records {
		if r.Status == "error" {
			m[r.ErrKind]++
		}
	}
	return m
}
