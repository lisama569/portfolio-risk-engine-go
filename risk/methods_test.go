package risk

import (
	"math"
	"testing"
	"time"
)

func almostEq(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestHistoricalVaRHandComputed(t *testing.T) {
	// Input snapshot: 10 P&L observations, ascending variety.
	pl := []float64{-0.05, -0.03, -0.01, 0.01, 0.02, 0.03, 0.04, 0.05, 0.06, 0.07}
	conf := ConfLevel(0.90)
	t.Logf("input snapshot: pl=%v confidence=%v", pl, float64(conf))

	varr, es := HistoricalVaR(pl, conf)
	// Judgment basis: R-7 quantile at alpha=0.1 -> h=(10-1)*0.1=0.9 ->
	// q = -0.05 + 0.9*(0.02) = -0.032 -> VaR = 0.032.
	// ES = -(mean of pl <= -0.032) = -(-0.05) = 0.05.
	t.Logf("judgment basis: type-7 quantile h=0.9 -> q=-0.032; ES tail mean = -0.05")
	if !almostEq(varr, 0.032, 1e-12) {
		t.Fatalf("VaR = %v, want 0.032", varr)
	}
	if !almostEq(es, 0.05, 1e-12) {
		t.Fatalf("ES = %v, want 0.05", es)
	}
}

func TestNormalQuantileKnownValues(t *testing.T) {
	cases := []struct{ p, want float64 }{
		{0.5, 0},
		{0.975, 1.959964},
		{0.99, 2.326348},
		{0.999, 3.090232},
	}
	for _, c := range cases {
		got := normalQuantile(c.p)
		t.Logf("input snapshot: p=%v -> z=%v (reference %v)", c.p, got, c.want)
		if !almostEq(got, c.want, 1e-5) {
			t.Fatalf("normalQuantile(%v) = %v, want %v", c.p, got, c.want)
		}
	}
}

func TestParametricVaRSingleAsset(t *testing.T) {
	// Input snapshot: one asset, symmetric returns with zero mean.
	days := []time.Time{}
	vals := []float64{0.01, -0.01, 0.02, -0.02, 0.0}
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i := range vals {
		days = append(days, base.AddDate(0, 0, i))
	}
	hist := map[AssetID]Returns{"A": {Asset: "A", Values: vals, AsOf: days}}
	weights := map[AssetID]float64{"A": 1.0}
	conf := ConfLevel(0.99)
	t.Logf("input snapshot: returns=%v weight=1.0 confidence=%v", vals, float64(conf))

	varr, es, issues := ParametricVaR([]AssetID{"A"}, weights, hist, conf, 2)
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %v", issues)
	}
	// Judgment basis: mean=0, sample var = (1e-4+1e-4+4e-4+4e-4+0)/4 = 2.5e-4,
	// sigma = 0.0158114; z(0.99)=2.326348 -> VaR = z*sigma = 0.0367821.
	// ES = sigma*phi(z)/0.01, phi(2.326348)=0.0266521 -> ES = 0.0421473.
	sigma := math.Sqrt(2.5e-4)
	wantVaR := 2.326348 * sigma
	wantES := sigma * 0.0266521 / 0.01
	t.Logf("judgment basis: sigma=%v z=2.326348 -> VaR=%v ES=%v", sigma, wantVaR, wantES)
	if !almostEq(varr, wantVaR, 1e-4) {
		t.Fatalf("VaR = %v, want ~%v", varr, wantVaR)
	}
	if !almostEq(es, wantES, 1e-4) {
		t.Fatalf("ES = %v, want ~%v", es, wantES)
	}
	if es < varr {
		t.Fatalf("ES (%v) must be >= VaR (%v)", es, varr)
	}
}

func TestAlignHistoriesIntersection(t *testing.T) {
	// Input snapshot: asset B is missing the middle date of asset A.
	d1 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	d3 := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	hist := map[AssetID]Returns{
		"A": {Asset: "A", Values: []float64{0.01, 0.02, 0.03}, AsOf: []time.Time{d1, d2, d3}},
		"B": {Asset: "B", Values: []float64{-0.01, -0.03}, AsOf: []time.Time{d1, d3}},
	}
	t.Logf("input snapshot: A dates=[%s %s %s], B dates=[%s %s]",
		d1.Format("01-02"), d2.Format("01-02"), d3.Format("01-02"),
		d1.Format("01-02"), d3.Format("01-02"))

	matrix, kept, issues := alignHistories([]AssetID{"A", "B"}, hist, 2)
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %v", issues)
	}
	// Judgment basis: common dates are d1 and d3 only.
	t.Logf("judgment basis: common dates = {d1,d3}; aligned A=[0.01 0.03] B=[-0.01 -0.03]")
	if len(kept) != 2 || len(matrix[0]) != 2 {
		t.Fatalf("alignment shape wrong: kept=%v matrix=%v", kept, matrix)
	}
	if matrix[0][1] != 0.03 || matrix[1][1] != -0.03 {
		t.Fatalf("aligned values wrong: %v", matrix)
	}
}

func TestHistoricalVaRReproducible(t *testing.T) {
	pl := []float64{0.03, -0.02, 0.01, -0.05, 0.02, -0.01, 0.04, -0.03}
	v1, e1 := HistoricalVaR(pl, 0.95)
	v2, e2 := HistoricalVaR(pl, 0.95)
	t.Logf("input snapshot: pl=%v; run1=(%v,%v) run2=(%v,%v)", pl, v1, e1, v2, e2)
	if v1 != v2 || e1 != e2 {
		t.Fatalf("non-reproducible: (%v,%v) vs (%v,%v)", v1, e1, v2, e2)
	}
}
