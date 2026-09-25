package risk

import (
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Engine computes risk for named portfolios over a MarketStore. It is
// safe for concurrent use; every calculation runs against one immutable
// Snapshot, so concurrent updates never disturb an in-flight run and
// concurrent runs never share mutable state.
type Engine struct {
	market *MarketStore

	mu         sync.RWMutex
	portfolios map[string][]Position

	now func() time.Time // injectable clock; defaults to time.Now
}

// NewEngine constructs an Engine bound to the given market store.
func NewEngine(market *MarketStore) *Engine {
	return &Engine{market: market, portfolios: map[string][]Position{}, now: time.Now}
}

// WithClock overrides the reference clock (used for staleness checks),
// enabling deterministic tests.
func (e *Engine) WithClock(now func() time.Time) *Engine {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = now
	return e
}

// Register stores a portfolio definition under a name. Definitions are
// validated at compute time against the current snapshot, so registration
// never fails on market state.
func (e *Engine) Register(name string, positions []Position) error {
	if name == "" {
		return &LoadError{Issues: []Issue{{Kind: IssueInvalidHolding, Detail: "empty portfolio name"}}}
	}
	if len(positions) == 0 {
		return &LoadError{Issues: []Issue{{Kind: IssueInvalidHolding, Detail: "portfolio must have positions"}}}
	}
	cp := make([]Position, len(positions))
	copy(cp, positions)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.portfolios[name] = cp
	return nil
}

// CalcRequest identifies one calculation job.
type CalcRequest struct {
	Portfolio string
	Method    Method
	Level     ConfLevel
}

// Compute runs one risk calculation deterministically against a fresh
// market snapshot.
func (e *Engine) Compute(name string, m Method, c ConfLevel) (*Result, error) {
	return e.computeOn(name, m, c, e.market.View())
}

// computeOn runs one calculation against a caller-supplied immutable
// snapshot, so batches can share exactly one market view.
func (e *Engine) computeOn(name string, m Method, c ConfLevel, snap *Snapshot) (*Result, error) {
	e.mu.RLock()
	raw, ok := e.portfolios[name]
	now := e.now
	e.mu.RUnlock()
	if !ok {
		return nil, &LoadError{Issues: []Issue{{Kind: IssueUnknownAsset,
			Detail: fmt.Sprintf("portfolio %q is not registered", name)}}}
	}
	if m != MethodHistorical && m != MethodParametric {
		return nil, &LoadError{Issues: []Issue{{Kind: IssueInvalidHolding,
			Detail: fmt.Sprintf("unknown method %q", m)}}}
	}
	if c <= 0.5 || c >= 1 {
		return nil, &LoadError{Issues: []Issue{{Kind: IssueInvalidHolding,
			Detail: fmt.Sprintf("confidence level %v must be in (0.5, 1)", float64(c))}}}
	}

	pf, err := Normalize(name, raw, snap, now())
	if err != nil {
		return nil, err
	}

	res := &Result{
		Portfolio:  name,
		Method:     m,
		Confidence: c,
		Degraded:   len(pf.Issues) > 0,
		Issues:     append([]Issue(nil), pf.Issues...),
	}

	switch m {
	case MethodHistorical:
		pl, issues := PortfolioPnL(pf.Assets, pf.Weights, snapshotHist(snap, pf.Assets), snap.minHist)
		res.Issues = append(res.Issues, issues...)
		if len(pl) == 0 {
			return nil, &LoadError{Issues: res.Issues}
		}
		res.VaR, res.ES = HistoricalVaR(pl, c)
	case MethodParametric:
		v, s, issues := ParametricVaR(pf.Assets, pf.Weights, snapshotHist(snap, pf.Assets), c, snap.minHist)
		res.Issues = append(res.Issues, issues...)
		if len(issues) > 0 && v == 0 && s == 0 {
			return nil, &LoadError{Issues: res.Issues}
		}
		res.VaR, res.ES = v, s
	}
	if len(res.Issues) > 0 {
		res.Degraded = true
	}
	res.InputsHash = hashResultInputs(res, pf, snap)
	return res, nil
}

// snapshotHist extracts the history map for the given assets.
func snapshotHist(snap *Snapshot, assets []AssetID) map[AssetID]Returns {
	out := make(map[AssetID]Returns, len(assets))
	for _, a := range assets {
		if r, ok := snap.History(a); ok {
			out[a] = r
		}
	}
	return out
}

// hashResultInputs builds a deterministic digest of everything the result
// depends on: portfolio definition, method, level, snapshot sequence, and
// per-asset weight / price / full return series.
func hashResultInputs(res *Result, pf *Portfolio, snap *Snapshot) string {
	// Note: snapshot sequence (delivery metadata) is intentionally
	// excluded: identical final market state must hash identically
	// regardless of whether it was bulk-loaded or incrementally applied.
	parts := []string{
		res.Portfolio,
		string(res.Method),
		strconv.FormatFloat(float64(res.Confidence), 'g', 17, 64),
	}
	assets := append([]AssetID(nil), pf.Assets...)
	sort.Slice(assets, func(i, j int) bool { return assets[i] < assets[j] })
	for _, a := range assets {
		q, _ := snap.Quote(a)
		parts = append(parts, string(a),
			strconv.FormatFloat(pf.Weights[a], 'g', 17, 64),
			strconv.FormatFloat(q.Price, 'g', 17, 64))
		if r, ok := snap.History(a); ok {
			for _, v := range r.Values {
				parts = append(parts, strconv.FormatFloat(v, 'g', 17, 64))
			}
		}
	}
	return hashInputs(parts...)
}

// ComputeMany computes multiple requests concurrently. One immutable
// snapshot is shared by the whole batch, results are returned in request
// order, and no mutable state crosses goroutine boundaries: no data
// races, no cross-talk between portfolios.
func (e *Engine) ComputeMany(reqs []CalcRequest) ([]Result, error) {
	snap := e.market.View()
	results := make([]Result, len(reqs))
	errs := make([]error, len(reqs))
	var wg sync.WaitGroup
	for i, req := range reqs {
		wg.Add(1)
		go func(i int, req CalcRequest) {
			defer wg.Done()
			r, err := e.computeOn(req.Portfolio, req.Method, req.Level, snap)
			if err != nil {
				errs[i] = err
				return
			}
			results[i] = *r
		}(i, req)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return results, err
		}
	}
	return results, nil
}
