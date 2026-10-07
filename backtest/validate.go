package backtest

import (
	"fmt"
	"math"
	"time"

	"github.com/lisama569/portfolio-risk-engine-go/portfolio"
)

// ConfigError 回放/回测配置或数据集本身不可用时返回（整段运行拒绝启动，
// 而不是包装成看似正常的结论）。
type ConfigError struct{ Detail string }

func (e *ConfigError) Error() string { return "backtest config error: " + e.Detail }

// 各配置项默认值。
const (
	defaultConfidence = 0.99
	defaultMinSamples = 30
)

var validMethods = map[string]bool{
	"historical": true,
	"parametric": true,
}

// validateConfig 校验并归一化配置；零值字段填默认。返回新对象，不改入参。
func validateConfig(cfg Config) (*Config, error) {
	c := cfg
	if len(c.Methods) == 0 {
		c.Methods = []string{"historical"}
	}
	for _, m := range c.Methods {
		if !validMethods[m] {
			return nil, &ConfigError{Detail: fmt.Sprintf("unknown method %q", m)}
		}
	}
	if c.Confidence == 0 {
		c.Confidence = defaultConfidence
	}
	if c.Confidence <= 0 || c.Confidence >= 1 {
		return nil, &ConfigError{Detail: fmt.Sprintf("confidence=%v must be within (0,1)", c.Confidence)}
	}
	if c.MinSamples == 0 {
		c.MinSamples = defaultMinSamples
	}
	if c.MinSamples < 1 {
		return nil, &ConfigError{Detail: fmt.Sprintf("minSamples=%d must be >= 1", c.MinSamples)}
	}
	if c.MaxStaleness < 0 {
		return nil, &ConfigError{Detail: "maxStaleness must be >= 0 (0 disables staleness check)"}
	}
	return &c, nil
}

// validateDataset 校验时间轴非空且 AsOf 严格递增。
// 逐时点的数据非法（缺行情、非法数值等）不在这里报错，而在回放时按时点给出
// 可区分的失败原因，避免个别时点问题把整段回测在启动阶段一刀切。
func validateDataset(ds *Dataset) error {
	if ds == nil || len(ds.Steps) == 0 {
		return &ConfigError{Detail: "dataset has no steps"}
	}
	var prev time.Time
	for i, st := range ds.Steps {
		if i > 0 && !st.AsOf.After(prev) {
			return &ConfigError{Detail: fmt.Sprintf(
				"step[%d] asOf=%s not strictly after previous %s",
				i, st.AsOf.Format(time.RFC3339), prev.Format(time.RFC3339))}
		}
		prev = st.AsOf
	}
	return nil
}

// validatePortfolios 校验组合列表非空、每个组合名称与持仓非空、名称不重复。
func validatePortfolios(pfs []*portfolio.Portfolio) error {
	if len(pfs) == 0 {
		return &ConfigError{Detail: "no portfolios"}
	}
	names := make(map[string]bool, len(pfs))
	for i, pf := range pfs {
		if pf == nil {
			return &ConfigError{Detail: fmt.Sprintf("portfolio[%d] is nil", i)}
		}
		if pf.Name == "" {
			return &ConfigError{Detail: fmt.Sprintf("portfolio[%d] has empty name", i)}
		}
		if names[pf.Name] {
			return &ConfigError{Detail: fmt.Sprintf("duplicate portfolio name %q", pf.Name)}
		}
		names[pf.Name] = true
		if len(pf.Positions) == 0 {
			return &ConfigError{Detail: fmt.Sprintf("portfolio %q has no positions", pf.Name)}
		}
		for _, pos := range pf.Positions {
			if pos.Symbol == "" {
				return &ConfigError{Detail: fmt.Sprintf("portfolio %q has position with empty symbol", pf.Name)}
			}
		}
	}
	return nil
}

// finite 报告 f 是否为有限实数（非 NaN、非 ±Inf）。
func finite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}
