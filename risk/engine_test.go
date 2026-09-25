package risk

import (
	"errors"
	"math"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/lisama569/portfolio-risk-engine-go/market"
	"github.com/lisama569/portfolio-risk-engine-go/portfolio"
)

var base = time.Date(2026, 9, 25, 16, 0, 0, 0, time.UTC)

// makeReturns 生成确定性收益序列（固定种子，可复现）。
func makeReturns(symbols []string, n int, seed int64) map[string][]float64 {
	rng := rand.New(rand.NewSource(seed))
	out := make(map[string][]float64, len(symbols))
	for _, s := range symbols {
		r := make([]float64, n)
		for i := range r {
			r[i] = rng.NormFloat64() * 0.01 // 日波动 1%
		}
		out[s] = r
	}
	return out
}

func testPortfolio() *portfolio.Portfolio {
	return &portfolio.Portfolio{Name: "pf", Positions: []portfolio.Position{
		{Symbol: "AAA", Quantity: 10},
		{Symbol: "BBB", Quantity: 20},
	}}
}

func quote(sym string, price float64, seq uint64, asOf time.Time) market.Quote {
	return market.Quote{Symbol: sym, Price: price, AsOf: asOf, Seq: seq}
}

func logResult(t *testing.T, label string, r Result) {
	t.Helper()
	t.Logf("判定依据[%s]: method=%s confidence=%.2f VaR=%.6f ES=%.6f",
		label, r.Method, r.Confidence, r.VaR, r.ES)
}

func TestHistoricalReproducible(t *testing.T) {
	returns := makeReturns([]string{"AAA"}, 250, 7)
	pnl := returns["AAA"]
	r1 := Historical(pnl, 0.99)
	r2 := Historical(pnl, 0.99)
	t.Logf("输入快照: n=%d pnl[0:3]=%v confidence=0.99", len(pnl), pnl[:3])
	logResult(t, "run1", r1)
	logResult(t, "run2", r2)
	if r1 != r2 {
		t.Fatalf("same input, different results: %+v vs %+v", r1, r2)
	}
	if r1.ES < r1.VaR {
		t.Fatalf("ES < VaR violates coherence: %+v", r1)
	}
}

func TestParametricSanity(t *testing.T) {
	r := Parametric(0, 1, 0.99)
	t.Logf("输入快照: mean=0 stddev=1 confidence=0.99")
	logResult(t, "parametric", r)
	// 标准正态 99% 分位 ≈ 2.3263
	if math.Abs(r.VaR-2.3263) > 1e-3 {
		t.Fatalf("VaR off: %v", r.VaR)
	}
	if r.ES <= r.VaR {
		t.Fatalf("ES must exceed VaR: %+v", r)
	}
}

// 增量逐条更新与一次性全量装载，结论须在容差内一致；
// 重复与乱序更新不得造成结论漂移。
func TestIncrementalVsFullConsistency(t *testing.T) {
	returns := makeReturns([]string{"AAA", "BBB"}, 250, 42)
	pf := testPortfolio()

	inc := NewEngine(market.NewStore(time.Hour), pf, returns)
	full := NewEngine(market.NewStore(time.Hour), pf, returns)

	quotes := []market.Quote{
		quote("AAA", 100, 1, base),
		quote("BBB", 50, 1, base),
		quote("AAA", 101, 2, base.Add(time.Minute)),
		quote("BBB", 51, 2, base.Add(time.Minute)),
	}
	// 增量：逐条应用，并混入重复与乱序（应被拒绝且不影响结论）
	for _, q := range quotes {
		if errs := inc.ApplyQuotes(q); errs[0] != nil {
			t.Fatalf("incremental apply failed: %v", errs[0])
		}
	}
	t.Logf("输入快照: 增量序列=%d 条, 另混入重复 seq=2 与乱序 seq=1", len(quotes))
	inc.ApplyQuotes(quote("AAA", 101, 2, base.Add(time.Minute)))  // 重复
	inc.ApplyQuotes(quote("AAA", 99, 1, base))                    // 乱序
	// 全量：一次性装载
	full.ApplyQuotes(quotes...)

	ri, err := inc.Compute("historical", 0.99)
	if err != nil {
		t.Fatalf("incremental compute failed: %v", err)
	}
	rf, err := full.Compute("historical", 0.99)
	if err != nil {
		t.Fatalf("full compute failed: %v", err)
	}
	logResult(t, "incremental", ri)
	logResult(t, "full", rf)
	const tol = 1e-9
	t.Logf("判定依据: |dVaR|=%.3e |dES|=%.3e 容差=%.0e",
		math.Abs(ri.VaR-rf.VaR), math.Abs(ri.ES-rf.ES), tol)
	if math.Abs(ri.VaR-rf.VaR) > tol || math.Abs(ri.ES-rf.ES) > tol {
		t.Fatalf("incremental vs full mismatch beyond tolerance")
	}
}

// 并发重算多个组合：静态数据下所有并发结果必须一致，无数据竞争与串扰。
func TestConcurrentComputeConsistent(t *testing.T) {
	symbols := []string{"AAA", "BBB", "CCC"}
	returns := makeReturns(symbols, 250, 99)
	store := market.NewStore(time.Hour)
	store.Apply(quote("AAA", 100, 1, base))
	store.Apply(quote("BBB", 50, 1, base))
	store.Apply(quote("CCC", 25, 1, base))

	engines := []*Engine{
		NewEngine(store, &portfolio.Portfolio{Name: "p1", Positions: []portfolio.Position{
			{Symbol: "AAA", Quantity: 10}, {Symbol: "BBB", Quantity: 5}}}, returns),
		NewEngine(store, &portfolio.Portfolio{Name: "p2", Positions: []portfolio.Position{
			{Symbol: "BBB", Quantity: 8}, {Symbol: "CCC", Quantity: 4}}}, returns),
		NewEngine(store, &portfolio.Portfolio{Name: "p3", Positions: []portfolio.Position{
			{Symbol: "AAA", Quantity: 3}, {Symbol: "CCC", Quantity: 9}}}, returns),
	}
	t.Logf("输入快照: 3 个组合共享行情库, 每组合 32 个 goroutine 并发 Compute")

	const goroutines = 32
	results := make([][]Result, len(engines))
	var wg sync.WaitGroup
	for i, e := range engines {
		results[i] = make([]Result, goroutines)
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func(i, g int, e *Engine) {
				defer wg.Done()
				r, err := e.Compute("historical", 0.95)
				if err != nil {
					t.Errorf("compute failed: %v", err)
					return
				}
				results[i][g] = r
			}(i, g, e)
		}
	}
	wg.Wait()
	for i, rs := range results {
		for g := 1; g < goroutines; g++ {
			if rs[g] != rs[0] {
				t.Fatalf("engine %d: concurrent results diverged: %+v vs %+v", i, rs[g], rs[0])
			}
		}
		t.Logf("判定依据: engine[%d] %d 次并发结果完全一致 VaR=%.6f ES=%.6f",
			i, goroutines, rs[0].VaR, rs[0].ES)
	}
	// 不同组合结论应不同（无串扰导致雷同）
	if results[0][0] == results[1][0] {
		t.Fatalf("distinct portfolios produced identical results, possible crosstalk")
	}
}

// 行情缺失时 Compute 须以可区分原因失败，不得静默按零计价。
func TestComputeMissingQuoteDistinct(t *testing.T) {
	returns := makeReturns([]string{"AAA", "BBB"}, 100, 5)
	store := market.NewStore(time.Hour)
	store.Apply(quote("AAA", 100, 1, base)) // BBB 缺失
	e := NewEngine(store, testPortfolio(), returns)
	t.Logf("输入快照: 持仓 AAA+BBB, 行情仅 AAA")

	_, err := e.Compute("historical", 0.99)
	var re *market.RejectError
	t.Logf("判定依据: 期望原因=%s, 实际 err=%v", market.ReasonMissing, err)
	if !errors.As(err, &re) || re.Reason != market.ReasonMissing || re.Symbol != "BBB" {
		t.Fatalf("missing quote not surfaced distinctly: %v", err)
	}
}

// 行情过期时 Compute 须以 STALE 原因失败。
func TestComputeStaleQuoteDistinct(t *testing.T) {
	returns := makeReturns([]string{"AAA", "BBB"}, 100, 6)
	store := market.NewStore(30 * time.Minute)
	store.Apply(quote("AAA", 100, 1, base.Add(time.Hour))) // 新
	store.Apply(quote("BBB", 50, 1, base))                 // 旧，相对估值时刻过期
	e := NewEngine(store, testPortfolio(), returns)
	t.Logf("输入快照: AAA asOf=%s, BBB asOf=%s, maxStaleness=30m",
		base.Add(time.Hour).Format(time.RFC3339), base.Format(time.RFC3339))

	_, err := e.Compute("parametric", 0.99)
	var re *market.RejectError
	t.Logf("判定依据: 期望原因=%s, 实际 err=%v", market.ReasonStale, err)
	if !errors.As(err, &re) || re.Reason != market.ReasonStale || re.Symbol != "BBB" {
		t.Fatalf("stale quote not surfaced distinctly: %v", err)
	}
}

// 压力情景叠加：对重仓多头施加下跌冲击，受压 VaR 应显著上升，且基础行情不被修改。
func TestStressOverlay(t *testing.T) {
	returns := makeReturns([]string{"AAA", "BBB"}, 250, 11)
	store := market.NewStore(time.Hour)
	store.Apply(quote("AAA", 100, 1, base))
	store.Apply(quote("BBB", 50, 1, base))
	e := NewEngine(store, testPortfolio(), returns)

	baseRes, err := e.Compute("historical", 0.99)
	if err != nil {
		t.Fatalf("base compute failed: %v", err)
	}
	sc := StressScenario{"AAA": -0.20, "BBB": -0.10}
	t.Logf("输入快照: 压力情景=%v", sc)
	stressed, err := e.Stress(sc, 0.99)
	if err != nil {
		t.Fatalf("stress failed: %v", err)
	}
	logResult(t, "base", baseRes)
	logResult(t, "stressed", stressed)
	t.Logf("判定依据: 受压组合市值下降、历史损益分布整体左移, VaR 应上升")
	if stressed.VaR <= baseRes.VaR {
		t.Fatalf("stress did not increase VaR: base=%v stressed=%v", baseRes.VaR, stressed.VaR)
	}
	// 基础行情不被压力测试污染
	after, err := e.Compute("historical", 0.99)
	if err != nil || after != baseRes {
		t.Fatalf("stress polluted base state: %+v vs %+v err=%v", after, baseRes, err)
	}
	t.Logf("判定依据: 压力后基础重算结果不变, 情景未写入行情库")
}
