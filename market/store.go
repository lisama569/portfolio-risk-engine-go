package market

import (
	"fmt"
	"math"
	"sync"
	"time"
)

// Store 线程安全的行情库，支持增量更新与快照读取。
type Store struct {
	mu           sync.RWMutex
	quotes       map[string]Quote
	maxStaleness time.Duration
}

// NewStore 创建行情库，maxStaleness 为行情最大允许时效。
func NewStore(maxStaleness time.Duration) *Store {
	return &Store{quotes: make(map[string]Quote), maxStaleness: maxStaleness}
}

// Apply 应用一条增量行情；非法、重复、乱序时返回可区分错误。
// 重复序号（== 当前 Seq）幂等忽略；小于当前 Seq 视为乱序拒绝。
func (s *Store) Apply(q Quote) error {
	if q.Symbol == "" {
		return &RejectError{Symbol: q.Symbol, Reason: ReasonInvalidPrice, Detail: "empty symbol"}
	}
	if math.IsNaN(q.Price) || math.IsInf(q.Price, 0) || q.Price <= 0 {
		return &RejectError{Symbol: q.Symbol, Reason: ReasonInvalidPrice,
			Detail: fmt.Sprintf("price=%v must be finite and > 0", q.Price)}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.quotes[q.Symbol]
	if ok {
		switch {
		case q.Seq == cur.Seq:
			return &RejectError{Symbol: q.Symbol, Reason: ReasonDuplicate,
				Detail: fmt.Sprintf("seq=%d already applied", q.Seq)}
		case q.Seq < cur.Seq:
			return &RejectError{Symbol: q.Symbol, Reason: ReasonOutOfOrder,
				Detail: fmt.Sprintf("seq=%d < current seq=%d", q.Seq, cur.Seq)}
		}
	}
	s.quotes[q.Symbol] = q
	return nil
}

// Get 读取某标的在 asOf 时刻有效的行情；缺失或过期返回 RejectError。
func (s *Store) Get(symbol string, asOf time.Time) (Quote, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	q, ok := s.quotes[symbol]
	if !ok {
		return Quote{}, &RejectError{Symbol: symbol, Reason: ReasonMissing, Detail: "no quote available"}
	}
	if s.maxStaleness > 0 && asOf.Sub(q.AsOf) > s.maxStaleness {
		return Quote{}, &RejectError{Symbol: symbol, Reason: ReasonStale,
			Detail: fmt.Sprintf("quote asOf=%s, required asOf=%s, maxStaleness=%s",
				q.AsOf.Format(time.RFC3339), asOf.Format(time.RFC3339), s.maxStaleness)}
	}
	return q, nil
}

// Snapshot 返回当前全部行情的拷贝（读多写少下无数据竞争）。
func (s *Store) Snapshot() map[string]Quote {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]Quote, len(s.quotes))
	for k, v := range s.quotes {
		out[k] = v
	}
	return out
}
