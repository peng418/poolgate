// checkin.go 签到结果与保活记录的存储（需求 F1.8）。
//
// 只存「结论 + 上游原话」，不存凭证：签到会带着 token 走，但落盘的东西必须干净。
package store

import (
	"sync"
	"time"

	"poolgate/internal/channel"
)

// CheckinRecord 是一次签到/保活的结果快照，供账号屏与设置屏显示。
type CheckinRecord struct {
	OK         bool      `json:"ok"`
	Message    string    `json:"message"`
	Reward     string    `json:"reward,omitempty"`
	NoActivity bool      `json:"no_activity,omitempty"`
	At         time.Time `json:"at"`
}

// CheckinStore 保存各账号最近一次签到结果。
type CheckinStore struct {
	mu   sync.RWMutex
	byID map[string]CheckinRecord // key = kind + "/" + uid
}

// NewCheckinStore 建立签到结果存储。
func NewCheckinStore() *CheckinStore {
	return &CheckinStore{byID: map[string]CheckinRecord{}}
}

// Set 记录一次签到结果。
func (s *CheckinStore) Set(kind string, uid string, r channel.CheckinResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[kind+"/"+uid] = CheckinRecord{
		OK: r.OK, Message: r.Message, Reward: r.Reward, NoActivity: r.NoActivity,
		At: time.Now().UTC(),
	}
}

// Get 取某账号最近一次签到结果。
func (s *CheckinStore) Get(kind, uid string) (CheckinRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.byID[kind+"/"+uid]
	return r, ok
}

// LastAt 返回某渠道最近一次签到时间（设置屏「上次结果」）。
func (s *CheckinStore) LastAt(kind string) (CheckinRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var (
		best CheckinRecord
		ok   bool
	)
	for k, r := range s.byID {
		if len(k) <= len(kind) || k[:len(kind)+1] != kind+"/" {
			continue
		}
		if !ok || r.At.After(best.At) {
			best, ok = r, true
		}
	}
	return best, ok
}
