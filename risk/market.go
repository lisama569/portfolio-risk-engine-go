package risk

import (
	"sort"
	"sync"
	"time"
)

// MarketStore is the concurrency-safe holder of quotes and return
// history. All reads happen against immutable Snapshot copies, so
// calculations in flight are never disturbed by incremental updates.
type MarketStore struct {
	mu       sync.RWMutex
	quotes   map[AssetID]Quote
	hist     map[AssetID]Returns
	seq      int64
	maxSeq   map[AssetID]int64 // last applied sequence per asset
	staleWin time.Duration
	minHist  int
}

// NewMarketStore constructs an empty MarketStore with the given staleness
// window and minimum history length.
func NewMarketStore(staleWindow time.Duration, minHistory int) *MarketStore {
	if staleWindow <= 0 {
		staleWindow = 24 * time.Hour
	}
	if minHistory < 2 {
		minHistory = 2
	}
	return &MarketStore{
		quotes:   make(map[AssetID]Quote),
		hist:     make(map[AssetID]Returns),
		maxSeq:   make(map[AssetID]int64),
		staleWin: staleWindow,
		minHist:  minHistory,
	}
}

// QuoteUpdate carries one incremental quote change together with a
// monotonically increasing per-asset sequence number.
type QuoteUpdate struct {
	Quote    Quote
	Sequence int64
}

// Snapshot is an immutable point-in-time copy of market data.
type Snapshot struct {
	quotes   map[AssetID]Quote
	hist     map[AssetID]Returns
	seq      int64
	staleWin time.Duration
	minHist  int
}

// StaleWindow / MinHistory accessors let the engine apply validation
// policy consistently from a detached snapshot.
func (s *Snapshot) StaleWindow() time.Duration { return s.staleWin }
func (s *Snapshot) MinHistory() int            { return s.minHist }

// LoadQuotes validates and installs a full quote batch (e.g. startup
// load). Invalid prices are rejected with a distinguishable issue and
// never silently zeroed or skipped.
func (s *MarketStore) LoadQuotes(qs []Quote) []Issue {
	s.mu.Lock()
	defer s.mu.Unlock()
	var issues []Issue
	for _, q := range qs {
		if q.Asset == "" {
			issues = append(issues, Issue{Kind: IssueUnknownAsset, Detail: "quote with empty asset id"})
			continue
		}
		if !isFinite(q.Price) || q.Price <= 0 {
			issues = append(issues, Issue{Asset: q.Asset, Kind: IssueInvalidPrice,
				Detail: "price must be a finite positive number"})
			continue
		}
		s.quotes[q.Asset] = q
		if q.Source == "" {
			// Source is informational; nothing to do.
		}
	}
	return issues
}

// LoadHistory validates return series: finite values, matching dates,
// chronological order. Invalid series are rejected per asset.
func (s *MarketStore) LoadHistory(rs []Returns) []Issue {
	s.mu.Lock()
	defer s.mu.Unlock()
	var issues []Issue
	for _, r := range rs {
		if r.Asset == "" {
			issues = append(issues, Issue{Kind: IssueUnknownAsset, Detail: "history with empty asset id"})
			continue
		}
		if len(r.Values) == 0 {
			issues = append(issues, Issue{Asset: r.Asset, Kind: IssueNoHistory,
				Detail: "empty return series"})
			continue
		}
		if len(r.Values) != len(r.AsOf) {
			issues = append(issues, Issue{Asset: r.Asset, Kind: IssueInvalidReturns,
				Detail: "returns and dates length mismatch"})
			continue
		}
		bad := false
		for i, v := range r.Values {
			if !isFinite(v) {
				issues = append(issues, Issue{Asset: r.Asset, Kind: IssueInvalidReturns,
					Detail: "non-finite return in series"})
				bad = true
				break
			}
			if i > 0 && !r.AsOf[i].After(r.AsOf[i-1]) {
				issues = append(issues, Issue{Asset: r.Asset, Kind: IssueInvalidReturns,
					Detail: "return dates must be strictly increasing"})
				bad = true
				break
			}
		}
		if bad {
			continue
		}
		s.hist[r.Asset] = r
	}
	return issues
}

// Apply applies incremental quote updates atomically as one batch.
// Duplicate sequences (retry/double delivery) and out-of-order sequences
// are rejected with distinguishable issues; they never overwrite data,
// cause double pricing, or move the global sequence backwards.
func (s *MarketStore) Apply(updates []QuoteUpdate) []Issue {
	s.mu.Lock()
	defer s.mu.Unlock()
	var issues []Issue
	for _, u := range updates {
		q := u.Quote
		if q.Asset == "" {
			issues = append(issues, Issue{Kind: IssueUnknownAsset, Detail: "update with empty asset id"})
			continue
		}
		if u.Sequence <= 0 {
			issues = append(issues, Issue{Asset: q.Asset, Kind: IssueOutOfOrder,
				Detail: "sequence must be positive"})
			continue
		}
		last := s.maxSeq[q.Asset]
		if u.Sequence == last {
			issues = append(issues, Issue{Asset: q.Asset, Kind: IssueDupUpdate,
				Detail: "update already applied (same sequence)"})
			continue
		}
		if u.Sequence < last {
			issues = append(issues, Issue{Asset: q.Asset, Kind: IssueOutOfOrder,
				Detail: "sequence older than last applied"})
			continue
		}
		if !isFinite(q.Price) || q.Price <= 0 {
			issues = append(issues, Issue{Asset: q.Asset, Kind: IssueInvalidPrice,
				Detail: "price must be a finite positive number"})
			continue
		}
		s.quotes[q.Asset] = q
		s.maxSeq[q.Asset] = u.Sequence
		if u.Sequence > s.seq {
			s.seq = u.Sequence
		}
	}
	return issues
}

// View returns an immutable snapshot of all current market data.
func (s *MarketStore) View() *Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap := &Snapshot{
		quotes:   make(map[AssetID]Quote, len(s.quotes)),
		hist:     make(map[AssetID]Returns, len(s.hist)),
		seq:      s.seq,
		staleWin: s.staleWin,
		minHist:  s.minHist,
	}
	for k, v := range s.quotes {
		snap.quotes[k] = v
	}
	for k, v := range s.hist {
		snap.hist[k] = cloneReturns(v)
	}
	return snap
}

func cloneReturns(r Returns) Returns {
	out := Returns{Asset: r.Asset, Values: make([]float64, len(r.Values)), AsOf: make([]time.Time, len(r.AsOf))}
	copy(out.Values, r.Values)
	copy(out.AsOf, r.AsOf)
	return out
}

func (s *Snapshot) Quote(a AssetID) (Quote, bool) {
	q, ok := s.quotes[a]
	return q, ok
}

func (s *Snapshot) History(a AssetID) (Returns, bool) {
	r, ok := s.hist[a]
	return r, ok
}

func (s *Snapshot) Sequence() int64 { return s.seq }

// AssetIDs returns asset ids present in the snapshot in deterministic
// (sorted) order, used to build stable input hashes.
func (s *Snapshot) AssetIDs() []AssetID {
	seen := make(map[AssetID]struct{}, len(s.quotes)+len(s.hist))
	for a := range s.quotes {
		seen[a] = struct{}{}
	}
	for a := range s.hist {
		seen[a] = struct{}{}
	}
	out := make([]AssetID, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// isFinite reports whether x is neither NaN nor +/-Inf.
func isFinite(x float64) bool {
	if x != x { // NaN
		return false
	}
	return x-x == 0
}
