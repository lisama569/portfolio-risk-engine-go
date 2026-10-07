package backtest

import (
	"github.com/lisama569/portfolio-risk-engine-go/portfolio"
)

// Replay 仅做按时间推进的历史回放（不做真实损益对照）：
// 每个时点只使用该时点及之前已经拿到的行情与收益，返回各组合各口径的风险数值序列。
// 同一份数据重跑结果完全一致；预热/失败时点记录在 Report.Issues 中，绝不顶替。
func Replay(ds *Dataset, pfs []*portfolio.Portfolio, cfg Config) (*ReportSet, error) {
	return runAll(ds, pfs, cfg, false)
}
