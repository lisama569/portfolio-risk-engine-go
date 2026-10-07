package backtest

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/lisama569/portfolio-risk-engine-go/market"
	"github.com/lisama569/portfolio-risk-engine-go/portfolio"
)

// ---- 确定性测试夹具 -------------------------------------------------------

var btBase = time.Date(2026, 9, 1, 16, 0, 0, 0, time.UTC)

// 常规收益交替 +0.3% / -0.2%：历史损失分布包含真实小额损失，95% 最近秩 VaR 为正
// （=0.002×市值=2），常规 -0.2% 日损失与 VaR 相等（严格"大于"判据下不算突破）。
// crashReturn 极端冲击（-30%）：真实损失 300，必然击穿任何基于常规历史的阈值。
const (
	regUp       = 0.003
	regDown     = -0.002
	crashReturn = -0.30
	unitPrice   = 100.0
	qty         = 10.0
)

// regAt 返回第 t 个时点的常规收益（确定性交替，无随机源）。
func regAt(t int) float64 {
	if t%2 == 0 {
		return regUp
	}
	return regDown
}

// singleAssetPoint 构造单标的 AAA 在第 t 个时点的数据；ret 为当期已实现收益。
func singleAssetPoint(t int, ret float64) Point {
	return Point{
		AsOf: btBase.AddDate(0, 0, t),
		Quotes: []market.Quote{
			{Symbol: "AAA", Price: unitPrice, AsOf: btBase.AddDate(0, 0, t), Seq: uint64(t + 1)},
		},
		RealizedReturn: map[string]float64{"AAA": ret},
	}
}

// crashTimeline: n 个时点，收益在 crashAt（含）起为冲击。
func crashTimeline(n int, crashAt int) []Point {
	pts := make([]Point, n)
	for t := 0; t < n; t++ {
		r := regAt(t)
		if t >= crashAt {
			r = crashReturn
		}
		pts[t] = singleAssetPoint(t, r)
	}
	return pts
}

func singleAssetPortfolio() *portfolio.Portfolio {
	return &portfolio.Portfolio{Name: "pf-single", Positions: []portfolio.Position{
		{Symbol: "AAA", Quantity: qty},
	}}
}

func crashEngine(t *testing.T, minHistory int, staleness time.Duration) *Engine {
	t.Helper()
	eng, err := NewEngine(Config{
		Methods:      []string{"historical", "parametric"},
		Confidence:   0.95,
		MinHistory:   minHistory,
		MaxStaleness: staleness,
	})
	if err != nil {
		t.Fatalf("NewEngine failed: %v", err)
	}
	return eng
}

func logSeries(t *testing.T, s PortfolioSeries) {
	t.Helper()
	for _, p := range s.Points {
		if p.OK {
			risks := ""
			for _, mr := range p.Risks {
				risks += fmt.Sprintf(" %s(VaR=%.4f,ES=%.4f)", mr.Method, mr.VaR, mr.ES)
			}
			t.Logf("  [%d %s] OK%s pnl=%.4f hasNext=%v clean=%v breaches=%v",
				p.Index, p.AsOf.Format("01-02"), risks, p.RealizedPnL, p.HasNext, p.NextClean, p.Breaches)
		} else {
			t.Logf("  [%d %s] SKIP reason=%s detail=%q", p.Index, p.AsOf.Format("01-02"), p.SkipReason, p.SkipDetail)
		}
	}
}

// ---- 1) 预热不足 ----------------------------------------------------------

func TestWarmupInsufficient(t *testing.T) {
	const minHistory = 10
	timeline := crashTimeline(15, 1000) // 无冲击
	pf := singleAssetPortfolio()
	eng := crashEngine(t, minHistory, 48*time.Hour)
	t.Logf("输入: n=%d minHistory=%d, 前 %d 个时点干净样本应不足预热窗口",
		len(timeline), minHistory, minHistory)

	s, err := eng.ReplayOne(pf, timeline)
	if err != nil {
		t.Fatalf("ReplayOne failed: %v", err)
	}
	logSeries(t, s)

	// 前 minHistory-1 个点必须全部以 WARMUP 跳过，且不得产生任何风险结论。
	for i := 0; i < minHistory-1; i++ {
		p := s.Points[i]
		if p.OK || p.SkipReason != ReasonWarmup {
			t.Fatalf("point %d expected WARMUP skip, got ok=%v reason=%v", i, p.OK, p.SkipReason)
		}
	}
	// 预热跳过后不能计入任何有效样本。
	for _, st := range s.Stats {
		t.Logf("判定依据[%s]: eligible=%d (预热点不计入)", st.Method, st.EligiblePoints)
	}
	if s.Stats[0].EligiblePoints != len(timeline)-minHistory {
		t.Fatalf("expected %d eligible after warmup, got %d",
			len(timeline)-minHistory, s.Stats[0].EligiblePoints)
	}
	t.Logf("判定依据: points[0..%d] 全部 reason=%s, 第 %d 点起产出风险",
		minHistory-2, ReasonWarmup, minHistory-1)
}

// ---- 2) 行情缺失 / 过期 / 非法 / 重复 / 乱序 / 未来 --------------------------

func TestQuoteDefectsDistinct(t *testing.T) {
	day := 24 * time.Hour
	pf := &portfolio.Portfolio{Name: "pf-two", Positions: []portfolio.Position{
		{Symbol: "AAA", Quantity: qty}, {Symbol: "BBB", Quantity: 4},
	}}
	rets := func(a, b float64) map[string]float64 {
		return map[string]float64{"AAA": a, "BBB": b}
	}
	q := func(sym string, px float64, t int, seq uint64) market.Quote {
		return market.Quote{Symbol: sym, Price: px, AsOf: btBase.AddDate(0, 0, t), Seq: seq}
	}
	// 6 个时点，前 3 个用于满足预热（minHistory=3），后 3 个分别制造缺陷。
	// AAA 用含真实下跌的混合收益（保证预热后 VaR 为正），BBB 收益恒为 0（不影响判因）。
	timeline := []Point{
		{AsOf: btBase, Quotes: []market.Quote{q("AAA", 100, 0, 1), q("BBB", 50, 0, 1)}, RealizedReturn: rets(regAt(0), 0.0)},
		{AsOf: btBase.Add(day), Quotes: []market.Quote{q("AAA", 100, 1, 2), q("BBB", 50, 1, 2)}, RealizedReturn: rets(regAt(1), 0.0)},
		{AsOf: btBase.Add(2 * day), Quotes: []market.Quote{q("AAA", 100, 2, 3), q("BBB", 50, 2, 3)}, RealizedReturn: rets(regAt(2), 0.0)},
		// t=3: BBB 无新行情且上一条距今 2 天（>25h）→ STALE；AAA 正常
		{AsOf: btBase.Add(3 * day), Quotes: []market.Quote{q("AAA", 100, 3, 4)}, RealizedReturn: rets(regAt(3), 0.0)},
		// t=4: BBB 非法负价 → INVALID；该时点整批拒绝，AAA 也不写入
		{AsOf: btBase.Add(4 * day), Quotes: []market.Quote{q("AAA", 100, 4, 5), q("BBB", -1, 4, 5)}, RealizedReturn: rets(regAt(4), 0.0)},
		// t=5: AAA 序号倒退（当前库内 seq=4，给 seq=3）→ OUT_OF_ORDER
		{AsOf: btBase.Add(5 * day), Quotes: []market.Quote{q("AAA", 100, 5, 3), q("BBB", 50, 5, 6)}, RealizedReturn: rets(regAt(5), 0.0)},
	}
	eng, err := NewEngine(Config{
		Methods: []string{"historical"}, Confidence: 0.95,
		MinHistory: 3, MaxStaleness: 23 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewEngine failed: %v", err)
	}
	s, err := eng.ReplayOne(pf, timeline)
	if err != nil {
		t.Fatalf("ReplayOne failed: %v", err)
	}
	logSeries(t, s)

	cases := []struct {
		idx    int
		reason SkipReason
	}{
		{3, ReasonStaleQuote},
		{4, ReasonInvalidQuote},
		{5, ReasonOutOfOrder},
	}
	for _, c := range cases {
		p := s.Points[c.idx]
		t.Logf("判定依据[%d]: 期望 reason=%s, 实际 ok=%v reason=%s detail=%q",
			c.idx, c.reason, p.OK, p.SkipReason, p.SkipDetail)
		if p.OK || p.SkipReason != c.reason {
			t.Fatalf("point %d expected %s, got ok=%v reason=%v", c.idx, c.reason, p.OK, p.SkipReason)
		}
	}
	// 缺失行情单独验证：构造仅含 AAA 行情的时间轴，用 minHistory=1 绕开预热，
	// 使缺失成为该时点唯一的失败原因（不与预热混淆）。
	missing := []Point{
		{AsOf: btBase, Quotes: []market.Quote{q("AAA", 100, 0, 1)}, RealizedReturn: rets(0.01, 0.0)},
	}
	eng1, _ := NewEngine(Config{Methods: []string{"historical"}, Confidence: 0.95, MinHistory: 1, MaxStaleness: 25 * time.Hour})
	s3, err := eng1.ReplayOne(pf, missing)
	if err != nil {
		t.Fatalf("ReplayOne missing(min1) failed: %v", err)
	}
	p := s3.Points[0]
	t.Logf("判定依据[缺失]: 持仓 AAA+BBB 但仅有 AAA 行情, 期望 reason=%s, 实际 reason=%s detail=%q",
		ReasonMissingQuote, p.SkipReason, p.SkipDetail)
	if p.OK || p.SkipReason != ReasonMissingQuote {
		t.Fatalf("expected MISSING_QUOTE, got ok=%v reason=%v", p.OK, p.SkipReason)
	}
}

// 重复序号与未来行情时间戳必须被显式拒绝，且不污染后续时点。
func TestDuplicateAndFutureQuote(t *testing.T) {
	day := 24 * time.Hour
	pf := singleAssetPortfolio()
	q := func(t int, seq uint64, asOf time.Time) market.Quote {
		return market.Quote{Symbol: "AAA", Price: unitPrice, AsOf: asOf, Seq: seq}
	}
	timeline := []Point{
		{AsOf: btBase, Quotes: []market.Quote{q(0, 1, btBase)}, RealizedReturn: map[string]float64{"AAA": regAt(0)}},
		// t=1: 序号与库内 seq=1 重复 → DUPLICATE
		{AsOf: btBase.Add(day), Quotes: []market.Quote{q(1, 1, btBase.Add(day))}, RealizedReturn: map[string]float64{"AAA": regAt(1)}},
		// t=2: 行情时间戳晚于时点 → INVALID（杜绝未来数据）
		{AsOf: btBase.Add(2 * day), Quotes: []market.Quote{q(2, 3, btBase.Add(3*day))}, RealizedReturn: map[string]float64{"AAA": regAt(2)}},
		// t=3: 正常推进，seq=4 必须可用 → OK（证明前两次拒绝未污染状态）
		{AsOf: btBase.Add(3 * day), Quotes: []market.Quote{q(3, 4, btBase.Add(3*day))}, RealizedReturn: map[string]float64{"AAA": regAt(3)}},
	}
	eng, _ := NewEngine(Config{Methods: []string{"historical"}, Confidence: 0.95, MinHistory: 1, MaxStaleness: 48 * time.Hour})
	s, err := eng.ReplayOne(pf, timeline)
	if err != nil {
		t.Fatalf("ReplayOne failed: %v", err)
	}
	logSeries(t, s)
	if s.Points[1].SkipReason != ReasonDuplicateQuote {
		t.Fatalf("point1 expected DUPLICATE, got %v", s.Points[1].SkipReason)
	}
	if s.Points[2].SkipReason != ReasonInvalidQuote {
		t.Fatalf("point2 expected INVALID(future), got %v", s.Points[2].SkipReason)
	}
	if !s.Points[3].OK {
		t.Fatalf("point3 should recover and be OK, got skip %s %q", s.Points[3].SkipReason, s.Points[3].SkipDetail)
	}
}

// ---- 3) 非法收益：缺失 / NaN，不得零顶替 ------------------------------------

func TestInvalidReturns(t *testing.T) {
	pf := singleAssetPortfolio()
	timeline := []Point{
		singleAssetPoint(0, regAt(0)),
		singleAssetPoint(1, regAt(1)),
		// t=2: 收益缺失
		{AsOf: btBase.AddDate(0, 0, 2), Quotes: []market.Quote{
			{Symbol: "AAA", Price: unitPrice, AsOf: btBase.AddDate(0, 0, 2), Seq: 3}},
			RealizedReturn: map[string]float64{}},
		// t=3: 收益 NaN
		{AsOf: btBase.AddDate(0, 0, 3), Quotes: []market.Quote{
			{Symbol: "AAA", Price: unitPrice, AsOf: btBase.AddDate(0, 0, 3), Seq: 4}},
			RealizedReturn: map[string]float64{"AAA": math.NaN()}},
		// t=4: 恢复正常；干净样本（0,1,4）>= minHistory=3 时应重新可算
		singleAssetPoint(4, regAt(4)),
	}
	eng, _ := NewEngine(Config{Methods: []string{"historical"}, Confidence: 0.95, MinHistory: 3, MaxStaleness: 48 * time.Hour})
	s, err := eng.ReplayOne(pf, timeline)
	if err != nil {
		t.Fatalf("ReplayOne failed: %v", err)
	}
	logSeries(t, s)
	if r := s.Points[2].SkipReason; r != ReasonMissingReturn {
		t.Fatalf("point2 expected MISSING_RETURN, got %v", r)
	}
	if r := s.Points[3].SkipReason; r != ReasonInvalidReturn {
		t.Fatalf("point3 expected INVALID_RETURN, got %v", r)
	}
	if !s.Points[4].OK {
		t.Fatalf("point4 should recover using aligned clean samples only, got %s %q",
			s.Points[4].SkipReason, s.Points[4].SkipDetail)
	}
	t.Logf("判定依据: 脏收益点被显式跳过且不以零顶替，恢复后按干净索引对齐重算")
}

// ---- 4) 突破次数 / 比例 / 最差亏损 -----------------------------------------

func TestBreachStatistics(t *testing.T) {
	const n, crashAt, minHistory = 40, 20, 10
	timeline := crashTimeline(n, crashAt)
	eng := crashEngine(t, minHistory, 48*time.Hour)
	s, err := eng.ReplayOne(singleAssetPortfolio(), timeline)
	if err != nil {
		t.Fatalf("ReplayOne failed: %v", err)
	}
	logSeries(t, s)

	eligible := n - 1 - (minHistory - 1) // 最后一点无后续区间
	// 突破区间：t=19（收益在 20 实现）与 t=20（收益在 21 实现），其余常规收益不击穿。
	const wantBreaches = 2
	const wantWorst = qty * unitPrice * (-crashReturn) // 300.0

	st, ok := s.FindStats("historical")
	if !ok {
		t.Fatalf("historical stats missing")
	}
	t.Logf("判定依据[historical]: eligible=%d breaches=%d rate=%.4f worst=%.4f (期望 eligible=%d breaches=%d rate=%.4f worst=%.1f)",
		st.EligiblePoints, st.Breaches, st.BreachRate, st.WorstLoss,
		eligible, wantBreaches, float64(wantBreaches)/float64(eligible), wantWorst)
	if st.EligiblePoints != eligible {
		t.Fatalf("eligible=%d want %d", st.EligiblePoints, eligible)
	}
	if st.Breaches != wantBreaches {
		t.Fatalf("breaches=%d want %d", st.Breaches, wantBreaches)
	}
	if math.Abs(st.BreachRate-float64(wantBreaches)/float64(eligible)) > 1e-12 {
		t.Fatalf("breachRate=%v want %v", st.BreachRate, float64(wantBreaches)/float64(eligible))
	}
	if math.Abs(st.WorstLoss-wantWorst) > 1e-9 {
		t.Fatalf("worstLoss=%v want %v", st.WorstLoss, wantWorst)
	}
	if !s.Points[crashAt-1].Breaches["historical"] || !s.Points[crashAt].Breaches["historical"] {
		t.Fatalf("breach points mismatch: t19=%v t20=%v",
			s.Points[crashAt-1].Breaches, s.Points[crashAt].Breaches)
	}
	// 参数法：常规日 σ 很小不击穿；冲击日必然击穿（随冲击进入样本，σ 上升后不再击穿），
	// 因此至少捕获前两个冲击日，最差亏损同样为 300。
	if ps, ok := s.FindStats("parametric"); ok {
		t.Logf("判定依据[parametric]: eligible=%d breaches=%d worst=%.4f",
			ps.EligiblePoints, ps.Breaches, ps.WorstLoss)
		if ps.Breaches < wantBreaches || math.Abs(ps.WorstLoss-wantWorst) > 1e-9 {
			t.Fatalf("parametric breaches=%d (want>=%d) worst=%v", ps.Breaches, wantBreaches, ps.WorstLoss)
		}
	}
}

// ---- 5) 可复现性：重跑逐位一致；不同执行顺序（goroutine 调度）结果一致 ---------

func TestReplayDeterministic(t *testing.T) {
	timeline := crashTimeline(40, 25)
	pf := singleAssetPortfolio()
	eng := crashEngine(t, 10, 48*time.Hour)
	s1, err := eng.ReplayOne(pf, timeline)
	if err != nil {
		t.Fatalf("run1 failed: %v", err)
	}
	s2, err := eng.ReplayOne(pf, timeline)
	if err != nil {
		t.Fatalf("run2 failed: %v", err)
	}
	if !seriesEqual(s1, s2) {
		t.Fatalf("same data replayed twice produced different results")
	}
	t.Logf("判定依据: 同一份 %d 点时间轴重跑两次, %d 个时点结果与 %d 条统计逐字段一致",
		len(timeline), len(s1.Points), len(s1.Stats))
}

// 杜绝未来函数：时点 t 的风险只能由 [0,t] 的数据决定。
// 构造 A（干净）与 B（在早期时点夹带一条"未来时间戳"行情，以及尾部存在极端冲击数据），
// 验证：(1) 夹带未来时间戳的时点被显式拒绝；(2) 截断掉尾部冲击后，前序时点风险与
// 完整重跑（若不含未来时间戳）完全一致，即尾部数据不会向前泄漏。
func TestNoLookahead(t *testing.T) {
	const n, minHistory = 30, 10
	clean := crashTimeline(n, 1000) // 全程常规收益，作为基线
	// 污染版：在第 5 个时点夹带一条时间戳指向第 25 天的"未来行情"。
	polluted := make([]Point, n)
	copy(polluted, clean)
	future := market.Quote{Symbol: "AAA", Price: unitPrice, AsOf: btBase.AddDate(0, 0, 25), Seq: 6}
	polluted[5].Quotes = append(append([]market.Quote{}, polluted[5].Quotes...), future)

	eng := crashEngine(t, minHistory, 48*time.Hour)
	base, err := eng.ReplayOne(singleAssetPortfolio(), clean)
	if err != nil {
		t.Fatalf("baseline failed: %v", err)
	}
	contam, err := eng.ReplayOne(singleAssetPortfolio(), polluted)
	if err != nil {
		t.Fatalf("polluted failed: %v", err)
	}
	logSeries(t, contam)
	if contam.Points[5].OK || contam.Points[5].SkipReason != ReasonInvalidQuote {
		t.Fatalf("future-dated quote must be rejected as INVALID, got ok=%v reason=%v",
			contam.Points[5].OK, contam.Points[5].SkipReason)
	}
	// 除被拒时点外，其余所有时点风险必须与基线逐位一致：未来数据既没被使用，也没污染状态。
	for i := 0; i < n; i++ {
		if i == 5 {
			continue
		}
		if !pointEqual(base.Points[i], contam.Points[i]) {
			t.Fatalf("point %d diverged after future-quote injection", i)
		}
	}
	// 截断验证：只回放前 k 个点，与完整时间轴上同一时点的风险完全一致。
	const k = 15
	head, err := eng.ReplayOne(singleAssetPortfolio(), clean[:k])
	if err != nil {
		t.Fatalf("truncated replay failed: %v", err)
	}
	for i := 0; i < k; i++ {
		if !pointEqual(base.Points[i], head.Points[i]) {
			t.Fatalf("point %d risk changed when later timeline data removed (lookahead leak)", i)
		}
	}
	t.Logf("判定依据: 未来时间戳行情在时点5被拒(%s); 其余时点与无未来数据基线逐位一致; 截断前%d点风险不随后续数据变化",
		ReasonInvalidQuote, k)
}

func pointEqual(a, b PointResult) bool {
	if a.OK != b.OK || a.SkipReason != b.SkipReason || len(a.Risks) != len(b.Risks) {
		return false
	}
	for i := range a.Risks {
		if a.Risks[i] != b.Risks[i] {
			return false
		}
	}
	return true
}

// ---- 6) 多组合并发回放：隔离、无串扰、顺序确定 -------------------------------

func TestMultiPortfolioConcurrentIsolation(t *testing.T) {
	timeline := crashTimeline(40, 25)
	pfs := []*portfolio.Portfolio{
		{Name: "p-long", Positions: []portfolio.Position{{Symbol: "AAA", Quantity: 10}}},
		{Name: "p-short", Positions: []portfolio.Position{{Symbol: "AAA", Quantity: -8}}},
		{Name: "p-big", Positions: []portfolio.Position{{Symbol: "AAA", Quantity: 100}}},
	}
	eng := crashEngine(t, 10, 48*time.Hour)

	// 串行基线
	var serial Report
	{
		ser := make([]PortfolioSeries, len(pfs))
		for i, pf := range pfs {
			s, err := eng.ReplayOne(pf, timeline)
			if err != nil {
				t.Fatalf("serial replay %s failed: %v", pf.Name, err)
			}
			ser[i] = s
		}
		serial = Report{Portfolios: ser}
	}

	// 多次并发，覆盖不同调度交织
	for run := 0; run < 5; run++ {
		rep, err := eng.Replay(pfs, timeline)
		if err != nil {
			t.Fatalf("concurrent Replay failed: %v", err)
		}
		for i := range pfs {
			if !seriesEqual(rep.Portfolios[i], serial.Portfolios[i]) {
				t.Fatalf("run %d portfolio %d concurrent diverged from serial baseline", run, i)
			}
		}
		if len(rep.Portfolios) != len(pfs) || rep.Portfolios[0].Portfolio != "p-long" {
			t.Fatalf("result order not aligned with input order")
		}
	}

	// 多空与不同规模组合的最差亏损必须与其持仓方向/规模一致，证明无串扰。
	rep, _ := eng.Replay(pfs, timeline)
	w0 := rep.Portfolios[0].Stats[0].WorstLoss
	w1 := rep.Portfolios[1].Stats[0].WorstLoss
	w2 := rep.Portfolios[2].Stats[0].WorstLoss
	t.Logf("判定依据: 多头最差亏损=%.4f(正), 空头冲击日为盈利故最差常规亏损=%.4f, 大持仓=%.4f", w0, w1, w2)
	if w0 <= 0 || w2 <= 0 {
		t.Fatalf("long portfolios should realize positive worst loss: %v %v", w0, w2)
	}
	if math.Abs(w2-w0*10) > 1e-6 {
		t.Fatalf("size scaling broken: w2=%v want %v", w2, w0*10)
	}
	// 空头在 -30% 冲击日是盈利，其最差亏损来自常规下跌日（-0.3%×8×100=2.4，误差容限内）；
	// 且冲击日绝不能被误记为亏损，故最差亏损必须显著小于多头的冲击损失。
	if !(w1 >= 0) || w1 > w0/2 {
		t.Fatalf("short portfolio worst loss should be a small regular-day loss, got %v (long worst=%v)", w1, w0)
	}
	if math.Abs(w1-0.8*qty*0.003*unitPrice) > 1e-6 {
		t.Fatalf("short worst loss=%v want ~2.4", w1)
	}
}

// ---- 7) 对冲组合不得伪造零风险 ----------------------------------------------

func TestNeutralPortfolioNoFakeZero(t *testing.T) {
	day := 24 * time.Hour
	// 多空完全相抵、两标的收益完全相同：组合损益恒为 0，风险阈值退化，必须显式标 DEGENERATE。
	pf := &portfolio.Portfolio{Name: "neutral", Positions: []portfolio.Position{
		{Symbol: "AAA", Quantity: 10}, {Symbol: "BBB", Quantity: -10},
	}}
	n := 12
	timeline := make([]Point, n)
	for i := 0; i < n; i++ {
		timeline[i] = Point{
			AsOf: btBase.Add(time.Duration(i) * day),
			Quotes: []market.Quote{
				{Symbol: "AAA", Price: 100, AsOf: btBase.Add(time.Duration(i) * day), Seq: uint64(i + 1)},
				{Symbol: "BBB", Price: 100, AsOf: btBase.Add(time.Duration(i) * day), Seq: uint64(i + 1)},
			},
			RealizedReturn: map[string]float64{"AAA": 0.01, "BBB": 0.01},
		}
	}
	eng, _ := NewEngine(Config{Methods: []string{"historical"}, Confidence: 0.95, MinHistory: 5, MaxStaleness: 48 * time.Hour})
	s, err := eng.ReplayOne(pf, timeline)
	if err != nil {
		t.Fatalf("ReplayOne failed: %v", err)
	}
	logSeries(t, s)
	degen := 0
	for _, p := range s.Points {
		if !p.OK {
			if p.SkipReason != ReasonWarmup && p.SkipReason != ReasonDegenerate {
				t.Fatalf("unexpected skip reason %s", p.SkipReason)
			}
			if p.SkipReason == ReasonDegenerate {
				degen++
			}
			continue
		}
		for _, mr := range p.Risks {
			if mr.VaR == 0 {
				t.Fatalf("VaR=0 fake-safe result must never be reported as OK")
			}
		}
	}
	if degen == 0 {
		t.Fatalf("fully neutral portfolio must be flagged DEGENERATE, got none")
	}
	if st := s.Stats[0]; st.EligiblePoints != 0 {
		t.Fatalf("degenerate points must not enter backtest stats, eligible=%d", st.EligiblePoints)
	}
	t.Logf("判定依据: 预热后 %d 个时点全部 reason=%s, 绝不输出 VaR=0 的正常结论, 有效样本=0",
		degen, ReasonDegenerate)
}

// ---- 8) 配置/结构级错误直接失败 ----------------------------------------------

func TestConfigErrors(t *testing.T) {
	if _, err := NewEngine(Config{Confidence: 0.95}); err == nil {
		t.Fatalf("empty methods must fail")
	}
	if _, err := NewEngine(Config{Methods: []string{"historical"}, Confidence: 1.5}); err == nil {
		t.Fatalf("bad confidence must fail")
	}
	eng, _ := NewEngine(Config{Methods: []string{"historical"}, Confidence: 0.95, MinHistory: 1})
	if _, err := eng.ReplayOne(singleAssetPortfolio(), nil); err == nil {
		t.Fatalf("empty timeline must fail")
	}
	if _, err := eng.Replay(nil, crashTimeline(5, 100)); err == nil {
		t.Fatalf("empty portfolios must fail")
	}
	// 时间轴非严格递增属于配置错误，整段拒绝而非逐点跳过。
	bad := []Point{singleAssetPoint(0, 0.01), singleAssetPoint(0, 0.01)}
	if _, err := eng.ReplayOne(singleAssetPortfolio(), bad); err == nil {
		t.Fatalf("non-increasing timeline must fail")
	}
	t.Logf("判定依据: 空口径/非法置信/空时间轴/空组合/时间乱序均直接返回 error")
}

// ---- 9) 单个组合的数据缺陷不影响其他组合 ------------------------------------

func TestOnePortfolioDefectDoesNotAffectOthers(t *testing.T) {
	timeline := crashTimeline(30, 1000)
	good := &portfolio.Portfolio{Name: "good", Positions: []portfolio.Position{{Symbol: "AAA", Quantity: 10}}}
	bad := &portfolio.Portfolio{Name: "missing-symbol", Positions: []portfolio.Position{
		{Symbol: "AAA", Quantity: 10}, {Symbol: "ZZZ", Quantity: 1},
	}}
	eng := crashEngine(t, 5, 48*time.Hour)
	rep, err := eng.Replay([]*portfolio.Portfolio{good, bad}, timeline)
	if err != nil {
		t.Fatalf("Replay failed: %v", err)
	}
	g, b := rep.Portfolios[0], rep.Portfolios[1]
	// good 组合除预热外不得有任何数据缺陷跳过。
	for _, sk := range g.Skips {
		if sk.Reason != ReasonWarmup {
			t.Fatalf("good portfolio unexpectedly skipped: %s %q", sk.Reason, sk.Detail)
		}
	}
	for _, p := range b.Points {
		if p.OK {
			t.Fatalf("portfolio holding ZZZ with no quote must never be OK")
		}
		if p.SkipReason != ReasonMissingQuote && p.SkipReason != ReasonWarmup {
			t.Fatalf("bad portfolio unexpected reason %s", p.SkipReason)
		}
	}
	t.Logf("判定依据: good 组合 skips=%v 全部可算; bad 组合每个可评估时点均 %s, 两者结果互不影响",
		g.SkipsByReason(), ReasonMissingQuote)
}

// ---- helpers ---------------------------------------------------------------

func seriesEqual(a, b PortfolioSeries) bool {
	if a.Portfolio != b.Portfolio || len(a.Points) != len(b.Points) ||
		len(a.Stats) != len(b.Stats) || len(a.Skips) != len(b.Skips) {
		return false
	}
	for i := range a.Points {
		x, y := a.Points[i], b.Points[i]
		if x.OK != y.OK || x.SkipReason != y.SkipReason || x.SkipDetail != y.SkipDetail ||
			x.HasNext != y.HasNext || x.NextClean != y.NextClean ||
			x.RealizedPnL != y.RealizedPnL || len(x.Risks) != len(y.Risks) ||
			len(x.Breaches) != len(y.Breaches) {
			return false
		}
		for j := range x.Risks {
			if x.Risks[j] != y.Risks[j] {
				return false
			}
		}
		for k, v := range x.Breaches {
			if y.Breaches[k] != v {
				return false
			}
		}
	}
	for i := range a.Stats {
		if a.Stats[i] != b.Stats[i] {
			return false
		}
	}
	return true
}
