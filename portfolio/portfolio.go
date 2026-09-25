// Package portfolio 定义组合持仓与权重归一化。
package portfolio

import "fmt"

// Position 单个标的持仓。
type Position struct {
	Symbol   string
	Quantity float64 // 正为多头，负为空头
}

// Portfolio 多资产组合。
type Portfolio struct {
	Name      string
	Positions []Position
}

// MissingQuoteError 表示某标的缺少可用行情，计算被拒绝而非静默跳过。
type MissingQuoteError struct{ Symbol string }

func (e *MissingQuoteError) Error() string {
	return "missing price for symbol: " + e.Symbol
}

// exposures 计算各标的市值暴露；任何标的缺价即报错。
func (p *Portfolio) exposures(prices map[string]float64) (map[string]float64, error) {
	expo := make(map[string]float64, len(p.Positions))
	for _, pos := range p.Positions {
		px, ok := prices[pos.Symbol]
		if !ok {
			return nil, &MissingQuoteError{Symbol: pos.Symbol}
		}
		expo[pos.Symbol] += pos.Quantity * px
	}
	return expo, nil
}

// Weights 按总市值归一化权重（净暴露口径，权重和为 1）；
// 同时保证绝对暴露归一：若净市值为 0（完全对冲）则退化为按绝对暴露归一。
// quote 缺失时返回错误（不静默跳过、不按零处理）。
func (p *Portfolio) Weights(prices map[string]float64) (map[string]float64, error) {
	expo, err := p.exposures(prices)
	if err != nil {
		return nil, err
	}
	var net, abs float64
	for _, e := range expo {
		net += e
		if e < 0 {
			abs -= e
		} else {
			abs += e
		}
	}
	denom := net
	if denom == 0 {
		denom = abs // 完全对冲组合：按总暴露归一，避免除零
	}
	if denom == 0 {
		return nil, fmt.Errorf("portfolio %q has zero exposure, cannot normalize", p.Name)
	}
	w := make(map[string]float64, len(expo))
	for s, e := range expo {
		w[s] = e / denom
	}
	return w, nil
}

// Value 组合总市值（净暴露）。
func (p *Portfolio) Value(prices map[string]float64) (float64, error) {
	expo, err := p.exposures(prices)
	if err != nil {
		return 0, err
	}
	var v float64
	for _, e := range expo {
		v += e
	}
	return v, nil
}
