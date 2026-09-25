package portfolio

import (
	"errors"
	"math"
	"testing"
)

func TestWeightsNormalized(t *testing.T) {
	pf := &Portfolio{Name: "p1", Positions: []Position{
		{Symbol: "AAA", Quantity: 10}, // 10*100=1000
		{Symbol: "BBB", Quantity: 5},  // 5*50=250
		{Symbol: "CCC", Quantity: -2}, // -2*125=-250
	}}
	prices := map[string]float64{"AAA": 100, "BBB": 50, "CCC": 125}
	t.Logf("输入快照: positions=%+v prices=%v", pf.Positions, prices)

	w, err := pf.Weights(prices)
	if err != nil {
		t.Fatalf("weights failed: %v", err)
	}
	var sum float64
	for s, wi := range w {
		sum += wi
		t.Logf("判定依据: weight[%s]=%.6f", s, wi)
	}
	t.Logf("判定依据: 权重和=%.12f（净暴露口径应归一为 1）", sum)
	if math.Abs(sum-1) > 1e-12 {
		t.Fatalf("weights not normalized: sum=%v", sum)
	}
	// 暴露方向保持：多头为正、空头为负
	if w["CCC"] >= 0 || w["AAA"] <= 0 {
		t.Fatalf("exposure sign lost: %v", w)
	}
}

func TestWeightsMissingPriceRejected(t *testing.T) {
	pf := &Portfolio{Name: "p2", Positions: []Position{
		{Symbol: "AAA", Quantity: 1},
		{Symbol: "ZZZ", Quantity: 1},
	}}
	prices := map[string]float64{"AAA": 100}
	t.Logf("输入快照: positions=%+v prices=%v（ZZZ 缺价）", pf.Positions, prices)
	_, err := pf.Weights(prices)
	var mq *MissingQuoteError
	t.Logf("判定依据: 期望 MissingQuoteError(symbol=ZZZ)，实际 err=%v", err)
	if !errors.As(err, &mq) || mq.Symbol != "ZZZ" {
		t.Fatalf("missing price not surfaced distinctly: %v", err)
	}
}

func TestValue(t *testing.T) {
	pf := &Portfolio{Name: "p3", Positions: []Position{
		{Symbol: "AAA", Quantity: 10},
		{Symbol: "BBB", Quantity: 5},
	}}
	prices := map[string]float64{"AAA": 100, "BBB": 50}
	v, err := pf.Value(prices)
	t.Logf("输入快照: positions=%+v prices=%v", pf.Positions, prices)
	t.Logf("判定依据: 市值=10*100+5*50=1250，实际=%.2f", v)
	if err != nil || v != 1250 {
		t.Fatalf("value=%v err=%v", v, err)
	}
}
