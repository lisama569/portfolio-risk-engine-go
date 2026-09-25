package risk

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/lisama569/portfolio-risk-engine-go/market"
	"github.com/lisama569/portfolio-risk-engine-go/portfolio"
)

// StressScenario 压力情景：symbol -> 相对冲击（如 -0.2 表示跌 20%）。
type StressScenario map[string]float64

// Engine 组合风险引擎：持有行情库、组合与历史收益，支持增量更新与压力叠加。
// 所有计量均基于行情库快照，Compute/Stress 并发调用安全且无结果串扰。
type Engine struct {
	store   *market.Store
	pf      *portfolio.Portfolio
	returns map[string][]float64 // symbol -> 历史日收益序列（等长）
	n       int                  // 收益序列长度
}

// NewEngine 创建引擎。returns 为 symbol -> 历史日收益序列（等长）。
// 序列不等长时返回 nil（调用方应视为配置错误）。
func NewEngine(store *market.Store, pf *portfolio.Portfolio, returns map[string][]float64) *Engine {
	n := -1
	for s, r := range returns {
		if n < 0 {
			n = len(r)
		} else if len(r) != n {
			return nil
		}
		_ = s
	}
	if n < 0 {
		n = 0
	}
	// 拷贝收益序列，避免调用方后续修改造成串扰
	cp := make(map[string][]float64, len(returns))
	for s, r := range returns {
		buf := make([]float64, len(r))
		copy(buf, r)
		cp[s] = buf
	}
	return &Engine{store: store, pf: pf, returns: cp, n: n}
}

// ApplyQuotes 增量应用行情；重复/乱序/非法行情被拒绝且不影响已有状态。
// 返回与输入等长的错误切片（nil 表示该条成功）。
func (e *Engine) ApplyQuotes(quotes ...market.Quote) []error {
	errs := make([]error, len(quotes))
	for i, q := range quotes {
		errs[i] = e.store.Apply(q)
	}
	return errs
}

// valuationTime 取快照中最新行情时间作为估值参考时刻。
func valuationTime(snap map[string]market.Quote) time.Time {
	var t time.Time
	for _, q := range snap {
		if q.AsOf.After(t) {
			t = q.AsOf
		}
	}
	return t
}

// currentWeights 基于快照取价并归一化权重；缺失/过期行情返回可区分错误。
func (e *Engine) currentWeights(snap map[string]market.Quote) (map[string]float64, float64, error) {
	asOf := valuationTime(snap)
	prices := make(map[string]float64, len(e.pf.Positions))
	for _, pos := range e.pf.Positions {
		if _, done := prices[pos.Symbol]; done {
			continue
		}
		q, err := e.store.Get(pos.Symbol, asOf)
		if err != nil {
			return nil, 0, err // *market.RejectError，原因可区分
		}
		prices[pos.Symbol] = q.Price
	}
	w, err := e.pf.Weights(prices)
	if err != nil {
		return nil, 0, err
	}
	v, err := e.pf.Value(prices)
	if err != nil {
		return nil, 0, err
	}
	return w, v, nil
}

// pnlSeries 由权重与历史收益构造组合历史损益序列（货币单位）。
func (e *Engine) pnlSeries(weights map[string]float64, value float64) ([]float64, error) {
	if e.n == 0 {
		return nil, fmt.Errorf("no return history configured")
	}
	pnl := make([]float64, e.n)
	for sym, w := range weights {
		r, ok := e.returns[sym]
		if !ok {
			return nil, fmt.Errorf("missing return history for symbol: %s", sym)
		}
		for t := 0; t < e.n; t++ {
			pnl[t] += w * r[t] * value
		}
	}
	return pnl, nil
}

// Compute 按口径计算当前风险结果；并发调用安全。
// method 为 "historical" 或 "parametric"。同一输入下结果可复现。
func (e *Engine) Compute(method string, confidence float64) (Result, error) {
	snap := e.store.Snapshot()
	w, v, err := e.currentWeights(snap)
	if err != nil {
		return Result{}, err
	}
	pnl, err := e.pnlSeries(w, v)
	if err != nil {
		return Result{}, err
	}
	switch method {
	case "historical":
		return Historical(pnl, confidence), nil
	case "parametric":
		return Parametric(mean(pnl), stddev(pnl), confidence), nil
	default:
		return Result{}, fmt.Errorf("unknown method: %q", method)
	}
}

// Stress 在压力情景下重算组合风险（不修改基础行情）。
// 口径：情景冲击产生一次性损益 shockPnL = Σ 暴露×冲击，
// 将其叠加到当前组合的历史损益分布上（分布整体平移），再取历史模拟分位。
// 未指定冲击的标的按 0 处理。
func (e *Engine) Stress(sc StressScenario, confidence float64) (Result, error) {
	snap := e.store.Snapshot()
	w, v, err := e.currentWeights(snap)
	if err != nil {
		return Result{}, err
	}
	pnl, err := e.pnlSeries(w, v)
	if err != nil {
		return Result{}, err
	}
	var shockPnL float64
	for sym, shock := range sc {
		ww, ok := w[sym]
		if !ok {
			return Result{}, fmt.Errorf("stress scenario references symbol %q not held by portfolio %q", sym, e.pf.Name)
		}
		shockPnL += ww * v * shock
	}
	for t := range pnl {
		pnl[t] += shockPnL
	}
	r := Historical(pnl, confidence)
	r.Method = "stress"
	return r, nil
}

func mean(x []float64) float64 {
	var s float64
	for _, v := range x {
		s += v
	}
	return s / float64(len(x))
}

// stddev 样本标准差；确定性实现（先排序求和以稳定浮点累加顺序）。
func stddev(x []float64) float64 {
	if len(x) < 2 {
		return 0
	}
	m := mean(x)
	dev := make([]float64, len(x))
	for i, v := range x {
		d := v - m
		dev[i] = d * d
	}
	sort.Float64s(dev)
	var s float64
	for _, d := range dev {
		s += d
	}
	return math.Sqrt(s / float64(len(x)-1))
}
