package backtest

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/lisama569/portfolio-risk-engine-go/market"
	"github.com/lisama569/portfolio-risk-engine-go/portfolio"
	"github.com/lisama569/portfolio-risk-engine-go/risk"
)

// methodAccum 单个 (组合, 口径) 在时间轴上的累计结果。
type methodAccum struct {
	method   string
	series   []PointRisk
	outcomes []PointOutcome
	issues   []Issue
}

// replayState 单个组合的时间推进状态：独立行情库，组合之间互不相串。
type replayState struct {
	pf    *portfolio.Portfolio
	store *market.Store
}

// Run 对多个组合、多口径同时执行历史回放与模型回测。
// 每个组合使用独立的回放状态（独立行情库、独立累计收益），互不影响；
// 输出按输入的组合顺序与口径顺序排列，与执行顺序无关、可复现。
func Run(ds *Dataset, pfs []*portfolio.Portfolio, cfg Config) (*ReportSet, error) {
	return runAll(ds, pfs, cfg, true)
}

// runAll 为回放/回测共用核心；withOutcomes=false 时不做真实损益对照。
func runAll(ds *Dataset, pfs []*portfolio.Portfolio, cfg Config, withOutcomes bool) (*ReportSet, error) {
	c, err := validateConfig(cfg)
	if err != nil {
		return nil, err
	}
	if err := validateDataset(ds); err != nil {
		return nil, err
	}
	if err := validatePortfolios(pfs); err != nil {
		return nil, err
	}

	// 每个组合独立回放；并发仅为提速，结果按下标收集，与执行顺序无关。
	perPortfolio := make([][]*Report, len(pfs))
	var wg sync.WaitGroup
	for i, pf := range pfs {
		wg.Add(1)
		go func(i int, pf *portfolio.Portfolio) {
			defer wg.Done()
			perPortfolio[i] = replayPortfolio(ds, pf, c, withOutcomes)
		}(i, pf)
	}
	wg.Wait()

	set := &ReportSet{}
	for _, reps := range perPortfolio {
		set.Reports = append(set.Reports, reps...)
	}
	return set, nil
}

// replayPortfolio 对单个组合沿时间轴逐时点推进。
func replayPortfolio(ds *Dataset, pf *portfolio.Portfolio, cfg *Config, withOutcomes bool) []*Report {
	st := &replayState{pf: pf, store: market.NewStore(cfg.MaxStaleness)}
	accs := make([]*methodAccum, len(cfg.Methods))
	for i, m := range cfg.Methods {
		accs[i] = &methodAccum{method: m}
	}

	// 持仓去重后的标的列表，字典序固定，保证累加顺序确定。
	held := heldSymbols(pf)

	for _, step := range ds.Steps {
		issue := advanceOnePoint(st, pf, held, step, cfg, withOutcomes, accs)
		if issue != nil {
			for _, a := range accs {
				a.issues = append(a.issues, *issue)
			}
		}
	}

	reps := make([]*Report, len(accs))
	for i, a := range accs {
		reps[i] = summarize(pf.Name, a.method, a.series, a.outcomes, a.issues, withOutcomes)
	}
	return reps
}

// heldSymbols 返回持仓标的去重列表，按字典序排列。
func heldSymbols(pf *portfolio.Portfolio) []string {
	seen := make(map[string]bool)
	for _, pos := range pf.Positions {
		seen[pos.Symbol] = true
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// advanceOnePoint 推进单个时点：只使用该时点及之前已送达的数据。
// 返回非 nil Issue 表示该时点预热/跳过/失败；成功时向各口径累加器写入风险与对照。
func advanceOnePoint(
	st *replayState,
	pf *portfolio.Portfolio,
	held []string,
	step Step,
	cfg *Config,
	withOutcomes bool,
	accs []*methodAccum,
) *Issue {
	// 1) 按字典序应用该时点送达的行情（仅本组合持仓标的）。
	tickSyms := make([]string, 0, len(step.Quotes))
	heldSet := make(map[string]bool, len(held))
	for _, s := range held {
		heldSet[s] = true
	}
	for s := range step.Quotes {
		if heldSet[s] {
			tickSyms = append(tickSyms, s)
		}
	}
	sort.Strings(tickSyms)
	for _, s := range tickSyms {
		tk := step.Quotes[s]
		q := market.Quote{Symbol: s, Price: tk.Price, AsOf: step.AsOf, Seq: tk.Seq}
		err := st.store.Apply(q)
		if err == nil {
			continue
		}
		var re *market.RejectError
		if asReject(err, &re) {
			switch re.Reason {
			case market.ReasonDuplicate:
				// 幂等忽略，沿用既有行情（若随后过期则自然以 STALE 失败）。
				continue
			case market.ReasonInvalidPrice:
				return fail(step.AsOf, ReasonInvalidData, re.Error())
			case market.ReasonOutOfOrder:
				return fail(step.AsOf, ReasonOutOfOrder, re.Error())
			}
		}
		return fail(step.AsOf, ReasonInternal, err.Error())
	}

	// 2) 以"当前时点"为估值时刻取价：缺失/过期给出可区分原因。
	prices := make(map[string]float64, len(held))
	for _, s := range held {
		q, err := st.store.Get(s, step.AsOf)
		if err != nil {
			var re *market.RejectError
			if asReject(err, &re) {
				return fail(step.AsOf, mapReason(re.Reason), re.Error())
			}
			return fail(step.AsOf, ReasonInternal, err.Error())
		}
		prices[s] = q.Price
	}

	// 3) 取截至该时点已知的历史收益（不含未来），校验样本量、完整性与数值合法性。
	n := -1
	for _, s := range held {
		r, ok := step.Returns[s]
		if !ok || len(r) == 0 {
			return fail(step.AsOf, ReasonMissingReturn,
				fmt.Sprintf("symbol %s has no known return history at this point", s))
		}
		if n < 0 {
			n = len(r)
		} else if len(r) != n {
			return fail(step.AsOf, ReasonInvalidData, fmt.Sprintf(
				"return series length mismatch: symbol %s has %d samples, want %d", s, len(r), n))
		}
		for i, v := range r {
			if !finite(v) {
				return fail(step.AsOf, ReasonInvalidData, fmt.Sprintf(
					"symbol %s returns[%d]=%v is NaN/Inf", s, i, v))
			}
		}
	}
	if n < cfg.MinSamples {
		return &Issue{AsOf: step.AsOf, Status: StatusWarmup, Reason: ReasonWarmup,
			Detail: fmt.Sprintf("samples=%d < minSamples=%d", n, cfg.MinSamples)}
	}

	// 4) 计算各标的暴露（多空方向保留）与组合损益序列。
	//    直接用暴露×收益累加：净市值为 0 的完全对冲组合按总绝对暴露仍有量纲，
	//    不会产生"风险为 0"的假象。
	expo := make(map[string]float64, len(held))
	var gross float64
	for _, pos := range pf.Positions {
		expo[pos.Symbol] += pos.Quantity * prices[pos.Symbol]
	}
	for _, s := range held {
		gross += math.Abs(expo[s])
	}
	if gross == 0 {
		return fail(step.AsOf, ReasonDegenerate, "portfolio has zero gross exposure at this point")
	}
	pnl := make([]float64, n)
	for t := 0; t < n; t++ {
		var p float64
		for _, s := range held { // held 已排序：累加顺序确定，可跨机器复现
			p += expo[s] * step.Returns[s][t]
		}
		pnl[t] = p
	}
	// 损益序列退化为常数（如多空收益完全相抵）：任何分位都等于该常数，
	// 参数法方差为 0 —— 明确标记失败，不给零风险/伪精确结论。
	constant := true
	for _, p := range pnl[1:] {
		if p != pnl[0] {
			constant = false
			break
		}
	}
	if constant {
		return fail(step.AsOf, ReasonDegenerate,
			"portfolio PnL series is constant; long/short offsets cancel all dispersion")
	}

	// 5) 各口径在同一份"该时点可见"数据上计量风险，结果按口径顺序暂存供对照使用。
	results := make([]risk.Result, len(accs))
	for i, a := range accs {
		res, err := riskOn(pnl, a.method, cfg.Confidence)
		if err != nil {
			// 口径本身只可能是已校验过的两种，这里兜底，绝不静默。
			return fail(step.AsOf, ReasonInternal, err.Error())
		}
		results[i] = res
		a.series = append(a.series, PointRisk{AsOf: step.AsOf, Result: res})
	}

	// 6) 回测对照：用下一期真实收益算真实损益。该信息绝不参与上面的风险计量。
	if !withOutcomes {
		return nil
	}
	realized, iss := realizedPnL(held, expo, step)
	if iss != nil {
		return iss // SKIPPED(NO_REALIZED_PNL) 或 FAILED(INVALID_DATA)
	}
	for i, a := range accs {
		loss := -realized
		a.outcomes = append(a.outcomes, PointOutcome{
			AsOf: step.AsOf, VaR: results[i].VaR, ES: results[i].ES,
			Realized: realized, Loss: loss, Breached: loss > results[i].VaR,
		})
	}
	return nil
}

// realizedPnL 用该时点暴露与下一期真实收益计算真实组合损益。
func realizedPnL(held []string, expo map[string]float64, step Step) (float64, *Issue) {
	var pnl float64
	for _, s := range held {
		r, ok := step.NextReturns[s]
		if !ok {
			return 0, &Issue{AsOf: step.AsOf, Status: StatusSkipped, Reason: ReasonNoRealizedPnL,
				Detail: fmt.Sprintf("no next-period realized return for symbol %s", s)}
		}
		if !finite(r) {
			return 0, fail(step.AsOf, ReasonInvalidData,
				fmt.Sprintf("symbol %s next return=%v is NaN/Inf", s, r))
		}
		pnl += expo[s] * r
	}
	return pnl, nil
}

// riskOn 按口径在给定损益序列上计量。
func riskOn(pnl []float64, method string, confidence float64) (risk.Result, error) {
	switch method {
	case "historical":
		return risk.Historical(pnl, confidence), nil
	case "parametric":
		m, sd := meanStd(pnl)
		return risk.Parametric(m, sd, confidence), nil
	default:
		return risk.Result{}, fmt.Errorf("unknown method: %q", method)
	}
}

// meanStd 与引擎一致的确定性矩估计（样本标准差）。
func meanStd(x []float64) (float64, float64) {
	var sum float64
	for _, v := range x {
		sum += v
	}
	m := sum / float64(len(x))
	dev := make([]float64, len(x))
	for i, v := range x {
		d := v - m
		dev[i] = d * d
	}
	sort.Float64s(dev)
	var ss float64
	for _, d := range dev {
		ss += d
	}
	sd := math.Sqrt(ss / float64(len(x)-1))
	return m, sd
}

// summarize 汇总单个口径的序列、对照与问题为回测结论。
func summarize(
	pfName, method string,
	series []PointRisk,
	outcomes []PointOutcome,
	issues []Issue,
	withOutcomes bool,
) *Report {
	r := &Report{
		Portfolio: pfName,
		Method:    method,
		Series:    series,
		Outcomes:  outcomes,
		Issues:    issues,
		Evaluated: len(outcomes),
		WorstLoss: math.NaN(),
	}
	for _, o := range outcomes {
		if o.Breached {
			r.Breaches++
		}
		if o.Loss > r.WorstLoss || math.IsNaN(r.WorstLoss) {
			r.WorstLoss = o.Loss
		}
	}
	if r.Evaluated > 0 {
		r.BreachRate = float64(r.Breaches) / float64(r.Evaluated)
	}
	hasFailure := false
	for _, is := range issues {
		if is.Status == StatusFailed {
			hasFailure = true
			break
		}
	}
	switch {
	case hasFailure:
		r.Healthy = false // 有失败时点：结论显式标记为不可信，不包装成正常
	case withOutcomes:
		r.Healthy = r.Evaluated > 0
	default:
		r.Healthy = len(series) > 0
	}
	return r
}

// fail 构造该时点的硬失败问题（FAILED 状态，原因可区分）。
func fail(at time.Time, reason Reason, detail string) *Issue {
	return &Issue{AsOf: at, Status: StatusFailed, Reason: reason, Detail: detail}
}

// asReject 是 errors.As 的窄封装，判别 *market.RejectError。
func asReject(err error, target **market.RejectError) bool {
	var re *market.RejectError
	if errors.As(err, &re) {
		*target = re
		return true
	}
	return false
}

// mapReason 把行情库拒绝原因映射为回测时点原因（保持口径一致、可区分）。
func mapReason(r market.RejectReason) Reason {
	switch r {
	case market.ReasonMissing:
		return ReasonMissingQuote
	case market.ReasonStale:
		return ReasonStaleQuote
	case market.ReasonInvalidPrice:
		return ReasonInvalidData
	case market.ReasonDuplicate:
		return ReasonDuplicateSeq
	case market.ReasonOutOfOrder:
		return ReasonOutOfOrder
	default:
		return ReasonInternal
	}
}
