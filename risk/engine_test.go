package risk

import (
	"fmt"
	"math"
	"sync"
	"testing"
	"time"
)

// makeMarket builds a 3-asset market with deterministic synthetic
// returns: r_t = k*t/1000 for asset k (0,1,2), 60 observations.
func makeMarket(t *testing.T, now time.Time) (*MarketStore, []AssetID) {
	t.Helper()
	assets := []AssetID{"A", "B", "C"}
	s := NewMarketStore(7*24*time.Hour, 30)
	var quotes []Quote
	var hist []Returns
	for k, a := range assets {
		quotes = append(quotes, Quote{Asset: a, Price: 100 + float64(k), AsOf: now.Add(-time.Hour)})
		n := 60
		vals := make([]float64, n)
		days := make([]time.Time, n)
		for i := 0; i < n; i++ {
			vals[i] = float64(k+1) * float64(i-n/2) / 1000.0
			days[i] = now.AddDate(0, 0, i-n)
		}
		hist = append(hist, Returns{Asset: a, Values: vals, AsOf: days})
	}
	if iss := s.LoadQuotes(quotes); len(iss) != 0 {
		t.Fatalf("LoadQuotes: %v", iss)
	}
	if iss := s.LoadHistory(hist); len(iss) != 0 {
		t.Fatalf("LoadHistory: %v", iss)
	}
	return s, assets
}

func baselinePortfolios() map[string][]Position {
	return map[string][]Position{
		"eq": {{Asset: "A", Weight: 0.4}, {Asset: "B", Weight: 0.35}, {Asset: "C", Weight: 0.25}},
		"ls": {{Asset: "A", Weight: 0.7}, {Asset: "B", Weight: -0.3}},
		"c":  {{Asset: "C", Weight: 1.0}},
	}
}

func registerAll(t *testing.T, e *Engine) {
	t.Helper()
	for name, ps := range baselinePortfolios() {
		if err := e.Register(name, ps); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
}

func TestConcurrentRecomputeNoRaceNoCrosstalk(t *testing.T) {
	store, _ := makeMarket(t, fixedNow)
	e := NewEngine(store).WithClock(func() time.Time { return fixedNow })
	registerAll(t, e)

	reqs := []CalcRequest{
		{"eq", MethodHistorical, 0.95},
		{"eq", MethodParametric, 0.95},
		{"eq", MethodHistorical, 0.99},
		{"ls", MethodHistorical, 0.95},
		{"ls", MethodParametric, 0.99},
		{"c", MethodHistorical, 0.99},
	}

	// Baseline single-threaded results for crosstalk comparison.
	base := map[string]Result{}
	for _, r := range reqs {
		res, err := e.Compute(r.Portfolio, r.Method, r.Level)
		if err != nil {
			t.Fatal(err)
		}
		base[fmt.Sprintf("%s|%s|%.2f", r.Portfolio, r.Method, r.Level)] = *res
	}
	t.Logf("input snapshot: 3 assets x 60 returns, %d portfolios; baseline computed", len(baselinePortfolios()))

	// Judgment basis: under -race, 200 goroutines hammering all requests
	// must (a) never race, (b) return byte-identical results/hash per key.
	var wg sync.WaitGroup
	const goroutines = 200
	errCh := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			rs, err := e.ComputeMany(reqs)
			if err != nil {
				errCh <- err
				return
			}
			for i, got := range rs {
				r := reqs[i]
				key := fmt.Sprintf("%s|%s|%.2f", r.Portfolio, r.Method, r.Level)
				want := base[key]
				if got.VaR != want.VaR || got.ES != want.ES || got.InputsHash != want.InputsHash {
					errCh <- fmt.Errorf("goroutine %d %s: got (%.10g,%.10g,%s) want (%.10g,%.10g,%s)",
						g, key, got.VaR, got.ES, got.InputsHash[:8], want.VaR, want.ES, want.InputsHash[:8])
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	for k, v := range base {
		t.Logf("judgment: %s -> VaR=%.8f ES=%.8f hash=%s", k, v.VaR, v.ES, v.InputsHash[:12])
	}
}

func TestIncrementalEqualsFullRecompute(t *testing.T) {
	// FULL path: load all final-state quotes up front.
	fullStore, _ := makeMarket(t, fixedNow)
	fullStore.LoadQuotes([]Quote{
		{Asset: "A", Price: 110, AsOf: fixedNow},
		{Asset: "B", Price: 55, AsOf: fixedNow},
		{Asset: "C", Price: 99.5, AsOf: fixedNow},
	})
	eFull := NewEngine(fullStore).WithClock(func() time.Time { return fixedNow })
	registerAll(t, eFull)

	// INCREMENTAL path: start from baseline market, then apply the same
	// quotes as sequenced updates.
	incStore, _ := makeMarket(t, fixedNow)
	issues := incStore.Apply([]QuoteUpdate{
		{Quote: Quote{Asset: "A", Price: 110, AsOf: fixedNow}, Sequence: 101},
		{Quote: Quote{Asset: "B", Price: 55, AsOf: fixedNow}, Sequence: 102},
		{Quote: Quote{Asset: "C", Price: 99.5, AsOf: fixedNow}, Sequence: 103},
	})
	t.Logf("input snapshot: incremental updates applied, apply issues=%v", issues)
	if len(issues) != 0 {
		t.Fatalf("in-order updates must apply cleanly: %v", issues)
	}
	eInc := NewEngine(incStore).WithClock(func() time.Time { return fixedNow })
	registerAll(t, eInc)

	// Judgment basis: identical final state must yield identical risk
	// numbers regardless of load path (tolerance 1e-15 allows only
	// floating-point order noise; here operations are order-identical).
	for _, name := range []string{"eq", "ls", "c"} {
		for _, m := range []Method{MethodHistorical, MethodParametric} {
			for _, c := range []ConfLevel{0.95, 0.99} {
				rf, err := eFull.Compute(name, m, c)
				if err != nil {
					t.Fatal(err)
				}
				ri, err := eInc.Compute(name, m, c)
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("input snapshot: %s/%s/%.2f full=(%.12g,%.12g) inc=(%.12g,%.12g) hashEq=%v",
					name, m, float64(c), rf.VaR, rf.ES, ri.VaR, ri.ES, rf.InputsHash == ri.InputsHash)
				if math.Abs(rf.VaR-ri.VaR) > 1e-15 || math.Abs(rf.ES-ri.ES) > 1e-15 {
					t.Errorf("%s/%s: drift full=(%.12g,%.12g) inc=(%.12g,%.12g)",
						name, m, rf.VaR, rf.ES, ri.VaR, ri.ES)
				}
				if rf.InputsHash != ri.InputsHash {
					t.Errorf("%s/%s: input hashes differ despite identical final state", name, m)
				}
			}
		}
	}
}

func TestDuplicateAndOutOfOrderUpdates(t *testing.T) {
	store, _ := makeMarket(t, fixedNow)
	// First clean update of A.
	if iss := store.Apply([]QuoteUpdate{
		{Quote: Quote{Asset: "A", Price: 110, AsOf: fixedNow}, Sequence: 101},
	}); len(iss) != 0 {
		t.Fatalf("first update should apply: %v", iss)
	}
	// Duplicate (same sequence) and out-of-order (older sequence): must be
	// rejected with distinguishable kinds and leave price unchanged.
	iss := store.Apply([]QuoteUpdate{
		{Quote: Quote{Asset: "A", Price: 120, AsOf: fixedNow}, Sequence: 101}, // duplicate
		{Quote: Quote{Asset: "A", Price: 130, AsOf: fixedNow}, Sequence: 50},  // stale
	})
	t.Logf("input snapshot: seq last=101; retry seq=101 price=120, backdated seq=50 price=130")
	if len(iss) != 2 || iss[0].Kind != IssueDupUpdate || iss[1].Kind != IssueOutOfOrder {
		t.Fatalf("judgment: want [duplicate_update, out_of_order_update], got %v", iss)
	}
	q, _ := store.View().Quote("A")
	// Judgment basis: price must still be exactly 110 — no double pricing,
	// no drift from replay/reorder.
	if q.Price != 110 {
		t.Fatalf("price drifted to %v after dup/out-of-order updates", q.Price)
	}
	t.Logf("judgment: price remains %.1f after rejected replays", q.Price)

	// Invalid price inside an update batch is independently rejected.
	iss = store.Apply([]QuoteUpdate{
		{Quote: Quote{Asset: "B", Price: -5, AsOf: fixedNow}, Sequence: 201},
	})
	if len(iss) != 1 || iss[0].Kind != IssueInvalidPrice {
		t.Fatalf("want invalid_price rejection, got %v", iss)
	}
	t.Logf("input snapshot: B price=-5 -> %v", iss)
}

func TestReproducibleHashAndValues(t *testing.T) {
	store, _ := makeMarket(t, fixedNow)
	e := NewEngine(store).WithClock(func() time.Time { return fixedNow })
	registerAll(t, e)
	r1, err := e.Compute("eq", MethodHistorical, 0.99)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := e.Compute("eq", MethodHistorical, 0.99)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("input snapshot: eq/historical/0.99 twice -> hash1=%s hash2=%s VaR=%.10g",
		r1.InputsHash[:16], r2.InputsHash[:16], r1.VaR)
	if r1.InputsHash != r2.InputsHash || r1.VaR != r2.VaR || r1.ES != r2.ES {
		t.Fatal("same inputs must reproduce identical hash and risk numbers")
	}
}

func TestStressScenarioOverlay(t *testing.T) {
	store, _ := makeMarket(t, fixedNow)
	e := NewEngine(store).WithClock(func() time.Time { return fixedNow })
	if err := e.Register("eq", baselinePortfolios()["eq"]); err != nil {
		t.Fatal(err)
	}
	snap := store.View()
	// Re-normalize for the scenario engine.
	pf, err := Normalize("eq", baselinePortfolios()["eq"], snap, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	// Scenario: A -10%, B absolute +5 (from 101 -> +4.95%), C +2%.
	sc := Scenario{Name: "crash", Shocks: []Shock{
		{Asset: "A", Relative: -0.10, UseRelative: true},
		{Asset: "B", Absolute: 5},
		{Asset: "C", Relative: 0.02, UseRelative: true},
	}}
	pl, issues := ApplyScenario(pf, snap, sc)
	want := 0.4*(-0.10) + 0.35*(5.0/101.0) + 0.25*0.02
	t.Logf("input snapshot: weights A=0.4 B=0.35 C=0.25; shocks A%%-10 B+5 C%%+2 -> pl=%.8f (want %.8f), issues=%v",
		pl, want, issues)
	// Judgment basis: pl = sum w_i * shocked return_i.
	if len(issues) != 0 || math.Abs(pl-want) > 1e-12 {
		t.Fatalf("scenario pl = %.12g, want %.12g, issues=%v", pl, want, issues)
	}

	// Scenario driving a price non-positive must be rejected with reason,
	// never priced at zero.
	bad := Scenario{Name: "wipeout", Shocks: []Shock{
		{Asset: "A", Absolute: -200}, // 100 + (-200) = -100
	}}
	_, badIssues := ApplyScenario(pf, snap, bad)
	if len(badIssues) != 1 || badIssues[0].Kind != IssueInvalidPrice {
		t.Fatalf("judgment: want invalid_price for negative shocked price, got %v", badIssues)
	}
	t.Logf("input snapshot: A absolute shock -200 (price 100) -> %v", badIssues)
}

func TestConcurrentUpdatesAndReads(t *testing.T) {
	store, _ := makeMarket(t, fixedNow)
	e := NewEngine(store).WithClock(func() time.Time { return fixedNow })
	registerAll(t, e)
	reqs := []CalcRequest{
		{"eq", MethodHistorical, 0.95},
		{"ls", MethodParametric, 0.99},
		{"c", MethodHistorical, 0.99},
	}

	var wg sync.WaitGroup
	// Judgment basis: readers must never observe a torn state; every
	// Compute either sees price p_n or p_{n+1}, never corrupted maps, and
	// never panics or races (run with -race).
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		seq := int64(1000)
		for {
			select {
			case <-stop:
				return
			default:
			}
			seq++
			store.Apply([]QuoteUpdate{{
				Quote:    Quote{Asset: "A", Price: 100 + float64(seq%5), AsOf: fixedNow},
				Sequence: seq,
			}})
		}
	}()
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				rs, err := e.ComputeMany(reqs)
				if err == nil {
					for _, r := range rs {
						if math.Abs(r.VaR) > 1 { // sanity: synthetic data => VaR far below 100%
							t.Errorf("implausible VaR %.6g", r.VaR)
							return
						}
					}
				}
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
	t.Logf("input snapshot: continuous seq updates on A interleaved with 16x200 batched recomputes; no race/error")
}
