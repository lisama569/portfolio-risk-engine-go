package risk

import (
	"math"
	"strings"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

func testDates(n int) []time.Time {
	out := make([]time.Time, n)
	base := fixedNow.AddDate(0, 0, -n)
	for i := range out {
		out[i] = base.AddDate(0, 0, i+1)
	}
	return out
}

func buildStore(t *testing.T, staleWindow time.Duration, minHist int, quotes []Quote, hist map[AssetID]Returns) *MarketStore {
	t.Helper()
	s := NewMarketStore(staleWindow, minHist)
	if iss := s.LoadQuotes(quotes); len(iss) > 0 {
		t.Logf("LoadQuotes rejected (part of input snapshot handling): %v", iss)
	}
	rs := make([]Returns, 0, len(hist))
	for _, r := range hist {
		rs = append(rs, r)
	}
	if iss := s.LoadHistory(rs); len(iss) > 0 {
		t.Logf("LoadHistory rejected: %v", iss)
	}
	return s
}

func TestNormalizeBasicWeights(t *testing.T) {
	// Input snapshot: A,B quoted fresh, equal gross weights.
	days := testDates(5)
	store := buildStore(t, 7*24*time.Hour, 3,
		[]Quote{
			{Asset: "A", Price: 100, AsOf: fixedNow.Add(-time.Hour)},
			{Asset: "B", Price: 50, AsOf: fixedNow.Add(-2 * time.Hour)},
		},
		map[AssetID]Returns{
			"A": {Asset: "A", Values: []float64{.01, -.01, .02, -.02, .0}, AsOf: days},
			"B": {Asset: "B", Values: []float64{.02, -.02, .01, -.01, .03}, AsOf: days},
		})
	pf, err := Normalize("p1", []Position{{Asset: "A", Weight: 0.5}, {Asset: "B", Weight: 0.5}}, store.View(), fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("input snapshot: weights A=0.5 B=0.5 -> normalized %v coverage=%v", pf.Weights, pf.Coverage)
	// Judgment basis: both assets covered, retained gross = 1.0 ->
	// weights unchanged and sum exactly to 1.
	if !almostEq(pf.Weights["A"], 0.5, 1e-12) || !almostEq(pf.Weights["B"], 0.5, 1e-12) {
		t.Fatalf("weights = %v", pf.Weights)
	}
	if !almostEq(pf.Coverage, 1, 1e-12) || len(pf.Issues) != 0 {
		t.Fatalf("coverage=%v issues=%v", pf.Coverage, pf.Issues)
	}
}

func TestNormalizeMissingQuoteDegrades(t *testing.T) {
	// Input snapshot: A fresh+history, C has no quote at all.
	days := testDates(4)
	store := buildStore(t, 7*24*time.Hour, 3,
		[]Quote{{Asset: "A", Price: 100, AsOf: fixedNow.Add(-time.Hour)}},
		map[AssetID]Returns{
			"A": {Asset: "A", Values: []float64{.01, -.01, .02, .0}, AsOf: days},
			"C": {Asset: "C", Values: []float64{.01, .02, .03, .0}, AsOf: days},
		})
	pf, err := Normalize("p", []Position{{Asset: "A", Weight: 0.5}, {Asset: "C", Weight: 0.5}}, store.View(), fixedNow)
	if err != nil {
		t.Fatalf("missing quote should degrade, not hard-reject: %v", err)
	}
	t.Logf("input snapshot: A quoted/fresh, C unquoted; issues=%v coverage=%v", pf.Issues, pf.Coverage)
	// Judgment basis: C -> missing_quote (distinguishable kind), excluded,
	// A renormalized to weight 1, coverage 0.5.
	if len(pf.Issues) != 1 || pf.Issues[0].Kind != IssueMissingQuote || pf.Issues[0].Asset != "C" {
		t.Fatalf("want single missing_quote[C], got %v", pf.Issues)
	}
	if !almostEq(pf.Weights["A"], 1, 1e-12) {
		t.Fatalf("A weight should renormalize to 1, got %v", pf.Weights)
	}
	if !almostEq(pf.Coverage, 0.5, 1e-12) {
		t.Fatalf("coverage want 0.5, got %v", pf.Coverage)
	}
}

func TestNormalizeStaleQuoteDegrades(t *testing.T) {
	// Input snapshot: A fresh, B quoted 10 days ago while window is 7 days.
	days := testDates(4)
	staleAt := fixedNow.Add(-10 * 24 * time.Hour)
	store := buildStore(t, 7*24*time.Hour, 3,
		[]Quote{
			{Asset: "A", Price: 100, AsOf: fixedNow.Add(-time.Hour)},
			{Asset: "B", Price: 80, AsOf: staleAt},
		},
		map[AssetID]Returns{
			"A": {Asset: "A", Values: []float64{.01, -.01, .02, .0}, AsOf: days},
			"B": {Asset: "B", Values: []float64{.01, -.02, .03, .0}, AsOf: days},
		})
	pf, err := Normalize("p", []Position{{Asset: "A", Weight: 0.6}, {Asset: "B", Weight: 0.4}}, store.View(), fixedNow)
	if err != nil {
		t.Fatalf("stale quote should degrade: %v", err)
	}
	t.Logf("input snapshot: B quote AsOf=%s (window 7d); issues=%v", staleAt.Format("2006-01-02"), pf.Issues)
	// Judgment basis: stale_quote must be distinguishable from missing_quote.
	if len(pf.Issues) != 1 || pf.Issues[0].Kind != IssueStaleQuote || pf.Issues[0].Asset != "B" {
		t.Fatalf("want single stale_quote[B], got %v", pf.Issues)
	}
	if !strings.Contains(pf.Issues[0].Detail, "exceeds window") {
		t.Fatalf("stale detail should explain window: %v", pf.Issues[0])
	}
	if !almostEq(pf.Weights["A"], 1, 1e-12) {
		t.Fatalf("renormalized A weight want 1, got %v", pf.Weights)
	}
}

func TestLoadInvalidPriceRejected(t *testing.T) {
	// Input snapshot: one bad price (0), one non-finite price, one good.
	s := NewMarketStore(7*24*time.Hour, 3)
	iss := s.LoadQuotes([]Quote{
		{Asset: "P0", Price: 0, AsOf: fixedNow},
		{Asset: "PN", Price: math.NaN(), AsOf: fixedNow},
		{Asset: "G", Price: 42, AsOf: fixedNow},
	})
	t.Logf("input snapshot: prices {P0:0, PN:NaN, G:42}; rejected=%v", iss)
	// Judgment basis: bad prices produce invalid_price issues and are NOT
	// stored (never silently treated as zero).
	kinds := map[AssetID]IssueKind{}
	for _, i := range iss {
		kinds[i.Asset] = i.Kind
	}
	if kinds["P0"] != IssueInvalidPrice || kinds["PN"] != IssueInvalidPrice {
		t.Fatalf("want invalid_price for both bad prices, got %v", iss)
	}
	if _, ok := s.quotes["P0"]; ok {
		t.Fatal("rejected zero-price quote must not be stored")
	}
	if _, ok := s.quotes["G"]; !ok {
		t.Fatal("good quote must be stored")
	}
}

func TestNormalizeShortHistoryDegrades(t *testing.T) {
	// Input snapshot: A has 4 observations, B only 2 while minHist=3.
	days4 := testDates(4)
	days2 := testDates(2)
	store := buildStore(t, 7*24*time.Hour, 3,
		[]Quote{
			{Asset: "A", Price: 100, AsOf: fixedNow},
			{Asset: "B", Price: 100, AsOf: fixedNow},
		},
		map[AssetID]Returns{
			"A": {Asset: "A", Values: []float64{.01, -.01, .02, .0}, AsOf: days4},
			"B": {Asset: "B", Values: []float64{.01, -.01}, AsOf: days2},
		})
	pf, err := Normalize("p", []Position{{Asset: "A", Weight: 0.5}, {Asset: "B", Weight: 0.5}}, store.View(), fixedNow)
	if err != nil {
		t.Fatalf("short history should degrade: %v", err)
	}
	t.Logf("input snapshot: A hist=4 obs, B hist=2 obs (min 3); issues=%v", pf.Issues)
	// Judgment basis: B -> short_history, distinct from no_history.
	if len(pf.Issues) != 1 || pf.Issues[0].Kind != IssueShortHistory || pf.Issues[0].Asset != "B" {
		t.Fatalf("want single short_history[B], got %v", pf.Issues)
	}
}

func TestNormalizeHardRejections(t *testing.T) {
	days := testDates(3)
	store := buildStore(t, 7*24*time.Hour, 3,
		[]Quote{{Asset: "A", Price: 100, AsOf: fixedNow}},
		map[AssetID]Returns{"A": {Asset: "A", Values: []float64{.01, -.01, .02}, AsOf: days}})

	// Duplicate asset.
	_, err := Normalize("dup",
		[]Position{{Asset: "A", Weight: 0.5}, {Asset: "A", Weight: 0.5}}, store.View(), fixedNow)
	if err == nil || !strings.Contains(err.Error(), "duplicate position") {
		t.Fatalf("want duplicate rejection, got %v", err)
	}
	t.Logf("input snapshot: duplicate A positions -> %v", err)

	// Both weight and exposure supplied.
	_, err = Normalize("both", []Position{{Asset: "A", Weight: 0.5, Exposure: 1000}}, store.View(), fixedNow)
	if err == nil || !strings.Contains(err.Error(), "exactly one of") {
		t.Fatalf("want mutually-exclusive rejection, got %v", err)
	}
	t.Logf("input snapshot: weight and exposure both set -> %v", err)
}

func TestNormalizeExposureBased(t *testing.T) {
	// Input snapshot: exposures 1000@100 (A) and 500@50 (B) -> equal
	// notional weights 0.5/0.5.
	days := testDates(3)
	store := buildStore(t, 7*24*time.Hour, 3,
		[]Quote{
			{Asset: "A", Price: 100, AsOf: fixedNow},
			{Asset: "B", Price: 50, AsOf: fixedNow},
		},
		map[AssetID]Returns{
			"A": {Asset: "A", Values: []float64{.01, -.01, .02}, AsOf: days},
			"B": {Asset: "B", Values: []float64{.02, -.02, .01}, AsOf: days},
		})
	pf, err := Normalize("exp", []Position{{Asset: "A", Exposure: 1000}, {Asset: "B", Exposure: 500}}, store.View(), fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("input snapshot: exposures A=1000@100 B=500@50 -> weights %v", pf.Weights)
	// Judgment basis: units = 10 and 10 -> 0.5 / 0.5.
	if !almostEq(pf.Weights["A"], 0.5, 1e-12) || !almostEq(pf.Weights["B"], 0.5, 1e-12) {
		t.Fatalf("weights = %v", pf.Weights)
	}
}
