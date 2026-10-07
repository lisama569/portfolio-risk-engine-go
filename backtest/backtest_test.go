package backtest

import (
	"math"
	"testing"
	"time"

	"github.com/lisama569/portfolio-risk-engine-go/portfolio"
)

var t0 = time.Date(2026, 1, 5, 16, 0, 0, 0, time.UTC)

func day(i int) time.Time { return t0.AddDate(0, 0, i) }

// pfLong 仅持有 AAA 多头 1 股。
func pfLong(name string) *portfolio.Portfolio {
	return &portfolio.Portfolio{Name: name, Positions: []portfolio.Position{{Symbol: "AAA", Quantity: 1}}}
}

// tickRow 构造该时点的行情：AAA=100，可选 BBB=50。
func tickRow(seq uint64, withBBB bool) map[string]QuoteTick {
	q := map[string]QuoteTick{"AAA": {Price: 100, Seq: seq}}
	if withBBB {
		q["BBB"] = QuoteTick{Price: 50, Seq: seq}
	}
	return q
}

// controlledWindow 返回 40 期可控收益：38 期 +0.001、2 期 -0.001。
// 多头 1 股、价格 100 时，损益为 38 个 +0.1 与 2 个 -0.1；
// 95% 历史模拟 VaR 恰好为 -0.1（k=38），因此下一期收益：
//
//	-0.001 -> 损失 0.1 > -0.1 => 突破；+0.001 -> 不突破。
func controlledWindow() map[string][]float64 {
	r := make([]float64, 40)
	for i := range r {
		r[i] = 0.001
	}
	r[0] = -0.001
	r[1] = -0.001
	return map[string][]float64{"AAA": r}
}

func logIssues(t *testing.T, reps []*Report) {
	t.Helper()
	for _, r := range reps {
		for _, is := range r.Issues {
			t.Logf("  issue: portfolio=%s method=%s asOf=%s status=%s reason=%s detail=%s",
				r.Portfolio, r.Method, is.AsOf.Format(time.RFC3339), is.Status, is.Reason, is.Detail)
		}
	}
}

func findReport(set *ReportSet, name, method string) *Report {
	for _, r := range set.Reports {
		if r.Portfolio == name && r.Method == method {
			return r
		}
	}
	return nil
}

// 场景 1：预热不足必须显式跳过（WARMUP），不参与统计，且不能冒充成功/失败。
func TestWarmupSkippedDistinct(t *testing.T) {
	ds := &Dataset{Steps: []Step{
		{AsOf: day(0), Quotes: tickRow(1, false),
			Returns:     map[string][]float64{"AAA": repeatVal(0.001, 5)},
			NextReturns: map[string]float64{"AAA": 0.001}},
		{AsOf: day(1), Quotes: tickRow(2, false),
			Returns:     controlledWindow(),
			NextReturns: map[string]float64{"AAA": 0.001}},
	}}
	cfg := Config{Methods: []string{"historical"}, Confidence: 0.95, MinSamples: 30, MaxStaleness: time.Hour}
	t.Logf("输入快照: 2 个时点, 第 1 时点仅 5 期收益, minSamples=%d", cfg.MinSamples)

	set, err := Run(ds, []*portfolio.Portfolio{pfLong("p1")}, cfg)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	r := findReport(set, "p1", "historical")
	if r == nil {
		t.Fatalf("report missing")
	}
	logIssues(t, []*Report{r})
	t.Logf("判定依据: 期望 1 条 WARMUP 跳过 + 1 个评估点; 实际 issues=%d series=%d evaluated=%d healthy=%v",
		len(r.Issues), len(r.Series), r.Evaluated, r.Healthy)
	if len(r.Issues) != 1 || r.Issues[0].Reason != ReasonWarmup || r.Issues[0].Status != StatusWarmup {
		t.Fatalf("warmup not surfaced distinctly: %+v", r.Issues)
	}
	if len(r.Series) != 1 || r.Evaluated != 1 {
		t.Fatalf("warmup point must not enter series/evaluation: series=%d evaluated=%d", len(r.Series), r.Evaluated)
	}
}

// 场景 2：突破次数、突破比例与最差一期亏损的统计必须与逐点对照一致。
func TestBacktestBreachStats(t *testing.T) {
	win := controlledWindow()
	ds := &Dataset{Steps: []Step{
		{AsOf: day(1), Quotes: tickRow(2, false), Returns: win, NextReturns: map[string]float64{"AAA": -0.001}}, // 突破
		{AsOf: day(2), Quotes: tickRow(3, false), Returns: win, NextReturns: map[string]float64{"AAA": 0.001}},  // 不突破
		{AsOf: day(3), Quotes: tickRow(4, false), Returns: win, NextReturns: map[string]float64{"AAA": -0.001}}, // 突破
		{AsOf: day(4), Quotes: tickRow(5, false), Returns: win},                                                 // 无真实损益 -> 跳过
	}}
	cfg := Config{Methods: []string{"historical"}, Confidence: 0.95, MinSamples: 30, MaxStaleness: time.Hour}
	t.Logf("输入快照: 4 个时点（窗口 40 期, 95%% VaR=-0.1）, 下一期收益序列 [-0.001,+0.001,-0.001, 缺]")

	set, err := Run(ds, []*portfolio.Portfolio{pfLong("stat")}, cfg)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	r := findReport(set, "stat", "historical")
	logIssues(t, []*Report{r})
	for _, o := range r.Outcomes {
		t.Logf("  对照: asOf=%s VaR=%.4f realized=%.4f loss=%.4f breached=%v",
			o.AsOf.Format(time.RFC3339), o.VaR, o.Realized, o.Loss, o.Breached)
	}
	t.Logf("判定依据: 期望 evaluated=3 breaches=2 rate=0.6667 worstLoss=0.1 healthy=true")
	t.Logf("实际: evaluated=%d breaches=%d rate=%.4f worstLoss=%.4f healthy=%v",
		r.Evaluated, r.Breaches, r.BreachRate, r.WorstLoss, r.Healthy)
	if r.Evaluated != 3 || r.Breaches != 2 {
		t.Fatalf("breach counting wrong: %+v", r)
	}
	if math.Abs(r.BreachRate-2.0/3.0) > 1e-12 {
		t.Fatalf("breach rate wrong: %v", r.BreachRate)
	}
	if math.Abs(r.WorstLoss-0.1) > 1e-12 {
		t.Fatalf("worst loss wrong: %v", r.WorstLoss)
	}
	if len(r.Issues) != 1 || r.Issues[0].Reason != ReasonNoRealizedPnL || r.Issues[0].Status != StatusSkipped {
		t.Fatalf("no-realized-pnl point not skipped distinctly: %+v", r.Issues)
	}
	if !r.Healthy {
		t.Fatalf("report with only benign skips should be healthy")
	}
}

// 场景 3：持仓标的在某时点完全没有行情 -> MISSING_QUOTE 失败，整段结论标记为不可信。
func TestMissingQuoteFailsPoint(t *testing.T) {
	pf := &portfolio.Portfolio{Name: "two", Positions: []portfolio.Position{
		{Symbol: "AAA", Quantity: 1}, {Symbol: "BBB", Quantity: 1},
	}}
	win := map[string][]float64{
		"AAA": variedWindow(0.001),
		"BBB": variedWindow(0.002),
	}
	ds := &Dataset{Steps: []Step{
		{AsOf: day(1), Quotes: tickRow(2, true), Returns: win, NextReturns: map[string]float64{"AAA": 0.001, "BBB": 0.001}},
		{AsOf: day(2), Quotes: tickRow(3, false), Returns: win, NextReturns: map[string]float64{"AAA": 0.001, "BBB": 0.001}}, // BBB 无新行情
	}}
	// maxStaleness=0 关闭时效校验：此时 BBB 沿用旧价仍可计量；
	// 要让 MISSING 与 STALE 可区分，本用例让 BBB 从未送达过（见下方第二个时点独立库）。
	staleFree := Config{Methods: []string{"historical"}, Confidence: 0.95, MinSamples: 30, MaxStaleness: 0}
	dsMissing := &Dataset{Steps: []Step{
		{AsOf: day(1), Quotes: tickRow(2, false) /* BBB 全程缺失 */, Returns: win,
			NextReturns: map[string]float64{"AAA": 0.001, "BBB": 0.001}},
	}}
	t.Logf("输入快照: 持仓 AAA+BBB, 行情仅送达 AAA（BBB 从未出现）, staleness 关闭")
	set, err := Run(dsMissing, []*portfolio.Portfolio{pf}, staleFree)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	r := findReport(set, "two", "historical")
	logIssues(t, []*Report{r})
	t.Logf("判定依据: 期望 FAILED reason=%s, healthy=false", ReasonMissingQuote)
	if len(r.Issues) != 1 || r.Issues[0].Status != StatusFailed || r.Issues[0].Reason != ReasonMissingQuote {
		t.Fatalf("missing quote not surfaced: %+v", r.Issues)
	}
	if r.Healthy || r.Evaluated != 0 {
		t.Fatalf("failed point must make report untrusted: healthy=%v evaluated=%d", r.Healthy, r.Evaluated)
	}
	// 兼容性补充：后续时点送达行情不应受失败时点影响（此处用完整两时点数据集验证）。
	set2, err := Run(ds, []*portfolio.Portfolio{pf}, Config{
		Methods: []string{"historical"}, Confidence: 0.95, MinSamples: 30, MaxStaleness: 0})
	if err != nil {
		t.Fatalf("run2 failed: %v", err)
	}
	r2 := findReport(set2, "two", "historical")
	t.Logf("对照: 两时点均有行情（BBB 第二时点沿用旧价）实际 issues=%d evaluated=%d",
		len(r2.Issues), r2.Evaluated)
	if r2.Evaluated != 2 || len(r2.Issues) != 0 || !r2.Healthy {
		t.Fatalf("valid dataset misclassified: %+v issues=%+v", r2, r2.Issues)
	}
}

// variedWindow 返回 40 期略有离散的确定性序列（每期 d 与 -d 交替错一位）。
func variedWindow(d float64) []float64 {
	r := make([]float64, 40)
	for i := range r {
		if i%3 == 0 {
			r[i] = -d
		} else {
			r[i] = d
		}
	}
	return r
}

// repeatVal 返回 n 个 v 的确定性序列。
func repeatVal(v float64, n int) []float64 {
	r := make([]float64, n)
	for i := range r {
		r[i] = v
	}
	return r
}

// 场景 4：行情相对该时点过期 -> STALE_QUOTE，必须与 MISSING_QUOTE 可区分。
func TestStaleQuoteFailsPoint(t *testing.T) {
	win := controlledWindow()
	ds := &Dataset{Steps: []Step{
		{AsOf: day(1), Quotes: tickRow(2, false), Returns: win, NextReturns: map[string]float64{"AAA": 0.001}},
		{AsOf: day(4), Quotes: nil /* 该时点无新行情，旧价已过期 */, Returns: win,
			NextReturns: map[string]float64{"AAA": 0.001}},
	}}
	cfg := Config{Methods: []string{"historical"}, Confidence: 0.95, MinSamples: 30, MaxStaleness: 48 * time.Hour}
	t.Logf("输入快照: 第 1 时点 day1 送达行情, 第 2 时点 day4 无新行情, maxStaleness=48h（间隔 72h 已过期）")
	set, err := Run(ds, []*portfolio.Portfolio{pfLong("stale")}, cfg)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	r := findReport(set, "stale", "historical")
	logIssues(t, []*Report{r})
	t.Logf("判定依据: 期望 issues=[OK, FAILED/%s]", ReasonStaleQuote)
	if len(r.Series) != 1 {
		t.Fatalf("first point should evaluate, got series=%d", len(r.Series))
	}
	if len(r.Issues) != 1 || r.Issues[0].Reason != ReasonStaleQuote || r.Issues[0].Status != StatusFailed {
		t.Fatalf("stale quote not surfaced distinctly: %+v", r.Issues)
	}
	if r.Healthy {
		t.Fatalf("stale failure must make report untrusted")
	}
}

// 场景 5：非法数值（价格 <=0/NaN、收益 NaN）必须给出 INVALID_DATA，且不得顶替为零。
func TestInvalidDataRejected(t *testing.T) {
	winBad := controlledWindow()
	bad := make([]float64, len(winBad["AAA"]))
	copy(bad, winBad["AAA"])
	bad[10] = math.NaN()

	cases := []struct {
		name   string
		quotes map[string]QuoteTick
		ret    map[string][]float64
	}{
		{"price-zero", map[string]QuoteTick{"AAA": {Price: 0, Seq: 1}}, controlledWindow()},
		{"price-negative", map[string]QuoteTick{"AAA": {Price: -5, Seq: 1}}, controlledWindow()},
		{"price-nan", map[string]QuoteTick{"AAA": {Price: math.NaN(), Seq: 1}}, controlledWindow()},
		{"return-nan", tickRow(1, false), map[string][]float64{"AAA": bad}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ds := &Dataset{Steps: []Step{{
				AsOf: day(1), Quotes: tc.quotes, Returns: tc.ret,
				NextReturns: map[string]float64{"AAA": 0.001},
			}}}
			cfg := Config{Methods: []string{"historical"}, Confidence: 0.95, MinSamples: 30, MaxStaleness: time.Hour}
			t.Logf("输入快照[%s]: quotes=%v returns[10]=%v", tc.name, tc.quotes, tc.ret["AAA"][10])
			set, err := Run(ds, []*portfolio.Portfolio{pfLong("bad")}, cfg)
			if err != nil {
				t.Fatalf("run failed: %v", err)
			}
			r := findReport(set, "bad", "historical")
			logIssues(t, []*Report{r})
			if len(r.Issues) != 1 || r.Issues[0].Status != StatusFailed || r.Issues[0].Reason != ReasonInvalidData {
				t.Fatalf("%s: want FAILED/%s, got %+v", tc.name, ReasonInvalidData, r.Issues)
			}
			if len(r.Series) != 0 || r.Healthy {
				t.Fatalf("%s: invalid data must not produce risk output", tc.name)
			}
		})
	}
}

// 场景 6：多空完全相抵（净市值为 0 且损益无离散）不得给出风险为零的假象；
// 而方向接近中性但收益存在离散的组合，风险必须为正。
func TestNeutralPortfolioNoFakeZero(t *testing.T) {
	neutral := &portfolio.Portfolio{Name: "neutral", Positions: []portfolio.Position{
		{Symbol: "AAA", Quantity: 1}, {Symbol: "BBB", Quantity: -2}, // 1*100 - 2*50 = 0 净市值
	}}
	win := map[string][]float64{
		"AAA": variedWindow(0.001),
		"BBB": variedWindow(0.001), // 暴露相反、收益完全同步 -> 损益恒为 0
	}
	ds := &Dataset{Steps: []Step{{
		AsOf: day(1), Quotes: tickRow(1, true), Returns: win,
		NextReturns: map[string]float64{"AAA": 0.001, "BBB": -0.001},
	}}}
	cfg := Config{Methods: []string{"historical", "parametric"}, Confidence: 0.95, MinSamples: 30, MaxStaleness: time.Hour}
	t.Logf("输入快照: 净市值=0 的完全对冲组合, 两腿收益完全同步（损益恒零）")
	set, err := Run(ds, []*portfolio.Portfolio{neutral}, cfg)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	for _, m := range []string{"historical", "parametric"} {
		r := findReport(set, "neutral", m)
		logIssues(t, []*Report{r})
		if len(r.Issues) != 1 || r.Issues[0].Reason != ReasonDegenerate {
			t.Fatalf("method=%s degenerate offset not detected: %+v", m, r.Issues)
		}
	}

	// 接近中性但两腿收益不完全同步：损益有离散，风险必须严格为正（不制造零风险假象）。
	win2 := map[string][]float64{
		"AAA": variedWindow(0.001),
		"BBB": variedWindow(0.002),
	}
	ds2 := &Dataset{Steps: []Step{{
		AsOf: day(1), Quotes: tickRow(1, true), Returns: win2,
		NextReturns: map[string]float64{"AAA": 0.001, "BBB": -0.001},
	}}}
	t.Logf("输入快照: 同样净市值=0, 但两腿波动不同（损益有离散）")
	set2, err := Run(ds2, []*portfolio.Portfolio{neutral}, cfg)
	if err != nil {
		t.Fatalf("run2 failed: %v", err)
	}
	for _, m := range []string{"historical", "parametric"} {
		r := findReport(set2, "neutral", m)
		logIssues(t, []*Report{r})
		if len(r.Series) != 1 {
			t.Fatalf("method=%s should evaluate near-neutral portfolio, series=%d", m, len(r.Series))
		}
		if !(r.Series[0].Result.VaR > 0) {
			t.Fatalf("method=%s near-neutral VaR must be > 0, got %.6f", m, r.Series[0].Result.VaR)
		}
	}
}

// 场景 7：多组合并发回放/回测必须彼此隔离、结果不串，且与执行顺序、机器无关。
// 通过两种方式验证：
//
//	a) 与"逐组合顺序运行"的结果逐字段一致（执行顺序无关）；
//	b) 同一份数据再跑一遍完全一致（可复现）；并跑 -race。
func TestMultiPortfolioConcurrentIsolation(t *testing.T) {
	win := controlledWindow()
	steps := make([]Step, 6)
	next := []float64{-0.001, 0.001, -0.001, 0.001, -0.001, 0.001}
	for i := range steps {
		steps[i] = Step{
			AsOf:        day(i + 1),
			Quotes:      tickRow(uint64(i+2), false),
			Returns:     win,
			NextReturns: map[string]float64{"AAA": next[i]},
		}
	}
	ds := &Dataset{Steps: steps}
	// 三个持仓数量不同的组合；AAA 价格相同，收益相同，故风险随数量线性放大、互不相同。
	pfs := []*portfolio.Portfolio{
		{Name: "p-x1", Positions: []portfolio.Position{{Symbol: "AAA", Quantity: 1}}},
		{Name: "p-x2", Positions: []portfolio.Position{{Symbol: "AAA", Quantity: 2}}},
		{Name: "p-x5", Positions: []portfolio.Position{{Symbol: "AAA", Quantity: 5}}},
	}
	cfg := Config{Methods: []string{"historical", "parametric"}, Confidence: 0.95, MinSamples: 30, MaxStaleness: 7 * 24 * time.Hour}
	t.Logf("输入快照: 3 个组合（数量 1/2/5）共享同一份时间轴, 6 个时点, 两种口径并发")

	concurrent, err := Run(ds, pfs, cfg)
	if err != nil {
		t.Fatalf("concurrent run failed: %v", err)
	}
	// 顺序逐个运行：结果须与并发一致（与执行顺序无关）。
	sequential := &ReportSet{}
	for _, pf := range pfs {
		set, err := Run(ds, []*portfolio.Portfolio{pf}, cfg)
		if err != nil {
			t.Fatalf("sequential run failed: %v", err)
		}
		sequential.Reports = append(sequential.Reports, set.Reports...)
	}
	for _, pf := range pfs {
		for _, m := range cfg.Methods {
			rc := findReport(concurrent, pf.Name, m)
			rs := findReport(sequential, pf.Name, m)
			if !reportsEqual(rc, rs) {
				t.Fatalf("portfolio=%s method=%s concurrent vs sequential differ", pf.Name, m)
			}
			t.Logf("判定依据: %s/%s evaluated=%d breaches=%d VaR序列首值=%.4f（并发=顺序）",
				pf.Name, m, rc.Evaluated, rc.Breaches, rc.Series[0].Result.VaR)
		}
	}
	// 再跑一遍：逐位可复现。
	again, err := Run(ds, pfs, cfg)
	if err != nil {
		t.Fatalf("rerun failed: %v", err)
	}
	for i := range again.Reports {
		if !reportsEqual(again.Reports[i], concurrent.Reports[i]) {
			t.Fatalf("report[%d] not reproducible across runs", i)
		}
	}
	// 不同组合风险必须不同（数量线性放大），无串扰导致雷同。
	v1 := findReport(concurrent, "p-x1", "historical").Series[0].Result.VaR
	v2 := findReport(concurrent, "p-x2", "historical").Series[0].Result.VaR
	v5 := findReport(concurrent, "p-x5", "historical").Series[0].Result.VaR
	t.Logf("判定依据: VaR(x1)=%.4f VaR(x2)=%.4f VaR(x5)=%.4f, 应满足 x2=2*x1, x5=5*x1", v1, v2, v5)
	if v2 != 2*v1 || v5 != 5*v1 || v1 == v2 {
		t.Fatalf("portfolio crosstalk or wrong scaling: %v %v %v", v1, v2, v5)
	}
}

// reportsEqual 逐字段比较两份报告（不含 NaN 直接 != 的坑）。
func reportsEqual(a, b *Report) bool {
	if a.Portfolio != b.Portfolio || a.Method != b.Method ||
		a.Evaluated != b.Evaluated || a.Breaches != b.Breaches ||
		a.Healthy != b.Healthy || len(a.Series) != len(b.Series) ||
		len(a.Outcomes) != len(b.Outcomes) || len(a.Issues) != len(b.Issues) {
		return false
	}
	if !sameFloat(a.BreachRate, b.BreachRate) || !sameFloat(a.WorstLoss, b.WorstLoss) {
		return false
	}
	for i := range a.Series {
		if a.Series[i] != b.Series[i] {
			return false
		}
	}
	for i := range a.Outcomes {
		if a.Outcomes[i] != b.Outcomes[i] {
			return false
		}
	}
	for i := range a.Issues {
		if a.Issues[i] != b.Issues[i] {
			return false
		}
	}
	return true
}

func sameFloat(x, y float64) bool {
	if math.IsNaN(x) {
		return math.IsNaN(y)
	}
	return x == y
}

// 场景 8：严禁偷看未来 —— 风险阈值只能取决于该时点的 Returns，
// 修改"下一期真实收益"乃至删除后续时点，已算出的风险序列必须完全不变。
func TestNoLookahead(t *testing.T) {
	win := controlledWindow()
	mkStep := func(i int, next float64) Step {
		return Step{AsOf: day(i + 1), Quotes: tickRow(uint64(i+2), false),
			Returns: win, NextReturns: map[string]float64{"AAA": next}}
	}
	base := &Dataset{Steps: []Step{mkStep(0, 0.001), mkStep(1, -0.001), mkStep(2, 0.001)}}
	// 把未来下一期收益改成极端值、并删掉后两个时点。
	modified := &Dataset{Steps: []Step{{
		AsOf: day(1), Quotes: tickRow(2, false), Returns: win,
		NextReturns: map[string]float64{"AAA": 0.99}, // 未来暴涨，绝不允许影响时点 0 的阈值
	}}}
	cfg := Config{Methods: []string{"historical"}, Confidence: 0.95, MinSamples: 30, MaxStaleness: 7 * 24 * time.Hour}
	t.Logf("输入快照: 基线 3 时点 vs 篡改版（时点0 的下一期收益 0.001->0.99 且后续时点删除）")
	r1, err := Run(base, []*portfolio.Portfolio{pfLong("look")}, cfg)
	if err != nil {
		t.Fatalf("base run failed: %v", err)
	}
	r2, err := Run(modified, []*portfolio.Portfolio{pfLong("look")}, cfg)
	if err != nil {
		t.Fatalf("modified run failed: %v", err)
	}
	pr1 := findReport(r1, "look", "historical").Series[0]
	pr2 := findReport(r2, "look", "historical").Series[0]
	t.Logf("判定依据: 时点0 VaR 基线=%.6f ES=%.6f; 篡改未来后 VaR=%.6f ES=%.6f",
		pr1.Result.VaR, pr1.Result.ES, pr2.Result.VaR, pr2.Result.ES)
	if pr1 != pr2 {
		t.Fatalf("risk at t0 changed after future data was altered: %+v vs %+v", pr1, pr2)
	}
}

// 场景 9：纯回放（Replay）不产生真实损益对照；口径不同结论必须可按口径区分。
func TestReplayOnlyAndMethodsDistinct(t *testing.T) {
	win := controlledWindow()
	ds := &Dataset{Steps: []Step{
		{AsOf: day(1), Quotes: tickRow(2, false), Returns: win, NextReturns: map[string]float64{"AAA": -0.001}},
	}}
	cfg := Config{Methods: []string{"historical", "parametric"}, Confidence: 0.99, MinSamples: 30, MaxStaleness: time.Hour}
	t.Logf("输入快照: 单时点两口径, Replay 不应产生对照样本")
	set, err := Replay(ds, []*portfolio.Portfolio{pfLong("rep")}, cfg)
	if err != nil {
		t.Fatalf("replay failed: %v", err)
	}
	h := findReport(set, "rep", "historical")
	p := findReport(set, "rep", "parametric")
	if h == nil || p == nil {
		t.Fatalf("missing method reports")
	}
	if h.Evaluated != 0 || p.Evaluated != 0 || len(h.Outcomes) != 0 {
		t.Fatalf("replay must not evaluate realized pnl: h=%d p=%d outcomes=%d", h.Evaluated, p.Evaluated, len(h.Outcomes))
	}
	if len(h.Series) != 1 || len(p.Series) != 1 || !h.Healthy || !p.Healthy {
		t.Fatalf("replay should produce one risk point per method: %+v %+v", h, p)
	}
	t.Logf("判定依据: historical VaR=%.6f, parametric VaR=%.6f（口径不同，分别可查）",
		h.Series[0].Result.VaR, p.Series[0].Result.VaR)
	if h.Series[0].Result == p.Series[0].Result {
		t.Fatalf("two methods produced identical result, methods not distinguished")
	}
}

// 场景 10：序号倒退必须给出 OUT_OF_ORDER，重复序号幂等忽略且不影响结论。
func TestSeqOutOfOrderAndDuplicate(t *testing.T) {
	win := controlledWindow()
	ds := &Dataset{Steps: []Step{
		{AsOf: day(1), Quotes: tickRow(5, false), Returns: win, NextReturns: map[string]float64{"AAA": 0.001}},
		{AsOf: day(2), Quotes: map[string]QuoteTick{"AAA": {Price: 101, Seq: 2}}, // seq 倒退
			Returns: win, NextReturns: map[string]float64{"AAA": 0.001}},
	}}
	cfg := Config{Methods: []string{"historical"}, Confidence: 0.95, MinSamples: 30, MaxStaleness: 7 * 24 * time.Hour}
	t.Logf("输入快照: seq 序列 5 -> 2（倒退）, 期望时点 2 以 OUT_OF_ORDER 失败")
	set, err := Run(ds, []*portfolio.Portfolio{pfLong("seq")}, cfg)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	r := findReport(set, "seq", "historical")
	logIssues(t, []*Report{r})
	if len(r.Issues) != 1 || r.Issues[0].Reason != ReasonOutOfOrder {
		t.Fatalf("out-of-order seq not surfaced: %+v", r.Issues)
	}

	// 重复序号：幂等忽略，沿用旧价，时点仍可计量（两个时点结论一致）。
	dsDup := &Dataset{Steps: []Step{
		{AsOf: day(1), Quotes: tickRow(5, false), Returns: win, NextReturns: map[string]float64{"AAA": 0.001}},
		{AsOf: day(2), Quotes: tickRow(5, false) /* seq 相同 */, Returns: win,
			NextReturns: map[string]float64{"AAA": 0.001}},
	}}
	set2, err := Run(dsDup, []*portfolio.Portfolio{pfLong("seq")}, cfg)
	if err != nil {
		t.Fatalf("dup run failed: %v", err)
	}
	r2 := findReport(set2, "seq", "historical")
	t.Logf("判定依据: 重复 seq=5 幂等忽略; evaluated=%d issues=%d（期望 2/0）", r2.Evaluated, len(r2.Issues))
	if r2.Evaluated != 2 || len(r2.Issues) != 0 {
		t.Fatalf("duplicate seq should be idempotent: evaluated=%d issues=%+v", r2.Evaluated, r2.Issues)
	}
	// 幂等忽略后沿用旧价、同窗口，两个时点风险必须逐位一致（确定性）。
	if r2.Series[0].Result != r2.Series[1].Result {
		t.Fatalf("duplicate seq must not change price/risk: %+v vs %+v", r2.Series[0], r2.Series[1])
	}
}

// 场景 11：时间轴非法 / 配置非法必须在启动阶段拒绝，而不是产出貌似正常的结论。
func TestConfigAndDatasetValidation(t *testing.T) {
	win := controlledWindow()
	good := []*portfolio.Portfolio{pfLong("ok")}
	cfg := Config{Methods: []string{"historical"}, Confidence: 0.95, MinSamples: 30}

	if _, err := Run(&Dataset{}, good, cfg); err == nil {
		t.Fatalf("empty dataset must be rejected")
	} else {
		t.Logf("判定依据[空数据集] -> %v", err)
	}
	badOrder := &Dataset{Steps: []Step{
		{AsOf: day(2), Quotes: tickRow(1, false), Returns: win},
		{AsOf: day(2), Quotes: tickRow(2, false), Returns: win}, // 时间未严格递增
	}}
	if _, err := Run(badOrder, good, cfg); err == nil {
		t.Fatalf("non-monotonic timeline must be rejected")
	} else {
		t.Logf("判定依据[时间轴未严格递增] -> %v", err)
	}
	if _, err := Run(&Dataset{Steps: []Step{{AsOf: day(1), Returns: win}}}, good,
		Config{Methods: []string{"bogus"}, Confidence: 0.95, MinSamples: 30}); err == nil {
		t.Fatalf("unknown method must be rejected")
	} else {
		t.Logf("判定依据[未知口径] -> %v", err)
	}
	if _, err := Run(&Dataset{Steps: []Step{{AsOf: day(1), Returns: win}}}, good,
		Config{Methods: []string{"historical"}, Confidence: 1.5, MinSamples: 30}); err == nil {
		t.Fatalf("bad confidence must be rejected")
	} else {
		t.Logf("判定依据[置信度越界] -> %v", err)
	}
}
