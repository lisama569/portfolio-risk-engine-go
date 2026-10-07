package backtest

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/lisama569/portfolio-risk-engine-go/market"
	"github.com/lisama569/portfolio-risk-engine-go/portfolio"
	"github.com/lisama569/portfolio-risk-engine-go/risk"
)

// NewEngine 创建回放引擎并校验配置。
func NewEngine(cfg Config) (*Engine, error) {
	if len(cfg.Methods) == 0 {
		return nil, errors.New("backtest: at least one method required")
	}
	for _, m := range cfg.Methods {
		if m != "historical" && m != "parametric" {
			return nil, fmt.Errorf("backtest: unknown method %q", m)
		}
	}
	if math.IsNaN(cfg.Confidence) || math.IsInf(cfg.Confidence, 0) ||
		cfg.Confidence <= 0 || cfg.Confidence >= 1 {
		return nil, fmt.Errorf("backtest: confidence must be within (0,1), got %v", cfg.Confidence)
	}
	if cfg.MinHistory <= 0 {
		cfg.MinHistory = 1
	}
	return &Engine{config: cfg}, nil
}

// validateTimeline 校验时间轴本身：非空、时间戳严格递增（乱序时间轴属配置错误，整段拒绝）。
func validateTimeline(timeline []Point) error {
	if len(timeline) == 0 {
		return errors.New("backtest: empty timeline")
	}
	for i := range timeline {
		if timeline[i].AsOf.IsZero() {
			return fmt.Errorf("backtest: point[%d] has zero AsOf", i)
		}
		if i > 0 && !timeline[i].AsOf.After(timeline[i-1].AsOf) {
			return fmt.Errorf("backtest: timeline AsOf not strictly increasing at index %d", i)
		}
	}
	return nil
}

// heldSymbols 返回组合去重后的持仓标的，按字典序（保证确定性）。
func heldSymbols(pf *portfolio.Portfolio) []string {
	seen := make(map[string]struct{})
	for _, p := range pf.Positions {
		seen[p.Symbol] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// validatePointQuotes 静态校验该时点全部行情（价格非法、时间戳越界），
// 并对照已有快照与本批次内序号做去重/乱序预检；任一不通过即返回跳过原因。
// 预检阶段不写入行情库，保证被跳过的时点不污染后续状态。
func validatePointQuotes(store *market.Store, batch []market.Quote, asOf time.Time) (SkipReason, string) {
	cur := store.Snapshot()
	batchSeq := make(map[string]uint64, len(batch))
	for _, q := range batch {
		if q.Symbol == "" {
			return ReasonInvalidQuote, "empty symbol"
		}
		if math.IsNaN(q.Price) || math.IsInf(q.Price, 0) || q.Price <= 0 {
			return ReasonInvalidQuote, fmt.Sprintf("symbol=%s price=%v must be finite and > 0", q.Symbol, q.Price)
		}
		if q.AsOf.After(asOf) {
			return ReasonInvalidQuote, fmt.Sprintf("symbol=%s quote AsOf=%s is after point AsOf=%s (future data forbidden)",
				q.Symbol, q.AsOf.Format(time.RFC3339), asOf.Format(time.RFC3339))
		}
		// 批次内：同一标的序号必须严格递增
		if prev, dup := batchSeq[q.Symbol]; dup {
			if q.Seq == prev {
				return ReasonDuplicateQuote, fmt.Sprintf("symbol=%s seq=%d duplicated within point", q.Symbol, q.Seq)
			}
			if q.Seq < prev {
				return ReasonOutOfOrder, fmt.Sprintf("symbol=%s seq=%d < earlier seq=%d within point", q.Symbol, q.Seq, prev)
			}
		}
		// 对照行情库已有状态
		if old, ok := cur[q.Symbol]; ok {
			if q.Seq == old.Seq {
				return ReasonDuplicateQuote, fmt.Sprintf("symbol=%s seq=%d already applied", q.Symbol, q.Seq)
			}
			if q.Seq < old.Seq {
				return ReasonOutOfOrder, fmt.Sprintf("symbol=%s seq=%d < current seq=%d", q.Symbol, q.Seq, old.Seq)
			}
		}
		batchSeq[q.Symbol] = q.Seq
		cur[q.Symbol] = q
	}
	return "", ""
}

// buildHistory 以时点 t（含）及之前的已实现收益构造各标的等长、对齐的收益历史。
// 当期（index==t）收益缺失/非法属显式缺陷；更早的脏数据仅按索引对齐剔除，绝不以零顶替。
func buildHistory(timeline []Point, held []string, t int) (map[string][]float64, int, SkipReason, string) {
	// 当期收益必须齐备且合法：风险在 t 时点重算时，截至 t 的收益历史不允许缺当期。
	for _, sym := range held {
		r, ok := timeline[t].RealizedReturn[sym]
		if !ok {
			return nil, 0, ReasonMissingReturn, fmt.Sprintf("symbol=%s missing realized return at index %d", sym, t)
		}
		if math.IsNaN(r) || math.IsInf(r, 0) {
			return nil, 0, ReasonInvalidReturn, fmt.Sprintf("symbol=%s non-finite return %v at index %d", sym, r, t)
		}
	}
	// 收集所有持仓标的在 [0,t] 内共同干净的时点索引（交集对齐）。
	clean := make(map[int]bool)
	for i := 0; i <= t; i++ {
		clean[i] = true
	}
	for _, sym := range held {
		for i := 0; i <= t; i++ {
			r, ok := timeline[i].RealizedReturn[sym]
			if !ok || math.IsNaN(r) || math.IsInf(r, 0) {
				delete(clean, i)
			}
		}
	}
	idx := make([]int, 0, t+1)
	for i := 0; i <= t; i++ {
		if clean[i] {
			idx = append(idx, i)
		}
	}
	hist := make(map[string][]float64, len(held))
	for _, sym := range held {
		buf := make([]float64, 0, len(idx))
		for _, i := range idx {
			buf = append(buf, timeline[i].RealizedReturn[sym])
		}
		hist[sym] = buf
	}
	return hist, len(idx), "", ""
}

// effectivePrices 取各持仓标的在 asOf 时点有效价格；缺失/过期映射为可区分跳过原因。
// 时效以"时点 AsOf"为基准显式判定（而非行情库内部的快照最新时刻），保证回放语义：
// 时点 t 只接受 t 及之前、且未超 MaxStaleness 的行情。
func (e *Engine) effectivePrices(store *market.Store, held []string, asOf time.Time) (map[string]float64, SkipReason, string) {
	prices := make(map[string]float64, len(held))
	maxStale := e.config.MaxStaleness
	for _, sym := range held {
		q, err := store.Get(sym, asOf)
		if err != nil {
			var re *market.RejectError
			if errors.As(err, &re) {
				switch re.Reason {
				case market.ReasonMissing:
					return nil, ReasonMissingQuote, re.Error()
				case market.ReasonStale:
					return nil, ReasonStaleQuote, re.Error()
				}
			}
			return nil, ReasonMissingQuote, err.Error()
		}
		if maxStale > 0 && asOf.Sub(q.AsOf) > maxStale {
			return nil, ReasonStaleQuote, fmt.Sprintf(
				"symbol=%s quote asOf=%s older than point asOf=%s by more than maxStaleness=%s",
				sym, q.AsOf.Format(time.RFC3339), asOf.Format(time.RFC3339), maxStale)
		}
		prices[sym] = q.Price
	}
	return prices, "", ""
}

// realizedPnL 计算时点 t 的下一期（t+1 收益覆盖区间）真实组合损益。
// nextClean=false 表示后续区间收益不完整/非法，该点不纳入突破统计（绝不用零顶替）。
func realizedPnL(pf *portfolio.Portfolio, prices map[string]float64, timeline []Point, t, next int, held []string) (float64, bool) {
	nextPrices := make(map[string]float64, len(held))
	for _, sym := range held {
		r, ok := timeline[next].RealizedReturn[sym]
		if !ok || math.IsNaN(r) || math.IsInf(r, 0) {
			return 0, false
		}
		nextPrices[sym] = prices[sym] * (1 + r)
	}
	v0, err := pf.Value(prices)
	if err != nil {
		return 0, false
	}
	v1, err := pf.Value(nextPrices)
	if err != nil || math.IsNaN(v1) || math.IsInf(v1, 0) {
		return 0, false
	}
	return v1 - v0, true
}

// ReplayOne 对单个组合沿时间轴逐时点回放+回测。
// 时间轴/组合级配置错误返回 error；时点级数据缺陷记录为可区分的 Skip，绝不中断整段回放。
func (e *Engine) ReplayOne(pf *portfolio.Portfolio, timeline []Point) (PortfolioSeries, error) {
	if pf == nil {
		return PortfolioSeries{}, errors.New("backtest: nil portfolio")
	}
	if len(pf.Positions) == 0 {
		return PortfolioSeries{}, fmt.Errorf("backtest: portfolio %q has no positions", pf.Name)
	}
	if err := validateTimeline(timeline); err != nil {
		return PortfolioSeries{}, err
	}
	held := heldSymbols(pf)
	store := market.NewStore(e.config.MaxStaleness)
	series := PortfolioSeries{Portfolio: pf.Name, Points: make([]PointResult, 0, len(timeline))}

	for t := range timeline {
		pt := timeline[t]
		pr := PointResult{Index: t, AsOf: pt.AsOf, Breaches: map[string]bool{}}

		// 1) 该时点行情先预检再写入；预检失败则一条都不写，保持状态可重入。
		if reason, detail := validatePointQuotes(store, pt.Quotes, pt.AsOf); reason != "" {
			pr.Skip(reason, detail)
			series.append(pr)
			continue
		}
		for _, q := range pt.Quotes {
			if err := store.Apply(q); err != nil {
				// 预检后不应再发生；防御性映射，保证原因可区分。
				var re *market.RejectError
				if errors.As(err, &re) {
					switch re.Reason {
					case market.ReasonDuplicate:
						pr.Skip(ReasonDuplicateQuote, re.Error())
					case market.ReasonOutOfOrder:
						pr.Skip(ReasonOutOfOrder, re.Error())
					default:
						pr.Skip(ReasonInvalidQuote, re.Error())
					}
				} else {
					pr.Skip(ReasonInvalidQuote, err.Error())
				}
			}
		}
		if !pr.OK && pr.SkipReason != "" {
			series.append(pr)
			continue
		}

		// 2) 有效价格校验（缺失/过期显式失败）——先于收益检查，无有效行情则不进入计量。
		prices, reason, detail := e.effectivePrices(store, held, pt.AsOf)
		if reason != "" {
			pr.Skip(reason, detail)
			series.append(pr)
			continue
		}

		// 3) 构造截至 t（含）的收益历史并做预热判定。
		hist, samples, reason, detail := buildHistory(timeline, held, t)
		if reason != "" {
			pr.Skip(reason, detail)
			series.append(pr)
			continue
		}
		if samples < e.config.MinHistory {
			pr.Skip(ReasonWarmup, fmt.Sprintf("clean samples=%d < minHistory=%d", samples, e.config.MinHistory))
			series.append(pr)
			continue
		}

		// 4) 逐口径重算风险（只用 t 及之前数据）。
		eng := risk.NewEngine(store, pf, hist)
		for _, m := range e.config.Methods {
			res, err := eng.Compute(m, e.config.Confidence)
			if err != nil {
				// 归一化失败等内部问题：显式呈现为退化，不伪造零风险。
				pr.Skip(ReasonDegenerate, fmt.Sprintf("method=%s compute failed: %v", m, err))
				break
			}
			if math.IsNaN(res.VaR) || math.IsInf(res.VaR, 0) ||
				math.IsNaN(res.ES) || math.IsInf(res.ES, 0) || res.VaR <= 0 {
				pr.Skip(ReasonDegenerate, fmt.Sprintf("method=%s produced non-positive/non-finite VaR=%v ES=%v", m, res.VaR, res.ES))
				break
			}
			pr.Risks = append(pr.Risks, MethodRisk{Method: m, VaR: res.VaR, ES: res.ES})
		}
		if pr.SkipReason != "" {
			series.append(pr)
			continue
		}

		// 5) 对照下一期真实损益（最后一个时点无后续区间）。
		pr.OK = true
		if t+1 < len(timeline) {
			pr.HasNext = true
			pnl, clean := realizedPnL(pf, prices, timeline, t, t+1, held)
			pr.RealizedPnL = pnl
			pr.NextClean = clean
			if clean {
				loss := -pnl
				for _, mr := range pr.Risks {
					pr.Breaches[mr.Method] = loss > mr.VaR
				}
			}
		}
		series.append(pr)
	}

	series.Stats = computeStats(&series, e.config.Methods)
	return series, nil
}

// Skip 标记时点结果为跳过。
func (pr *PointResult) Skip(reason SkipReason, detail string) {
	pr.OK = false
	pr.SkipReason = reason
	pr.SkipDetail = detail
}

// append 记录时点结果，并把跳过事件同步到序列级 Skips，保证可审计、不被静默吞掉。
func (s *PortfolioSeries) append(pr PointResult) {
	s.Points = append(s.Points, pr)
	if !pr.OK {
		s.Skips = append(s.Skips, SkipEvent{
			Index: pr.Index, AsOf: pr.AsOf, Reason: pr.SkipReason, Detail: pr.SkipDetail,
		})
	}
}
