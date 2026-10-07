package backtest

import (
	"errors"
	"fmt"
	"sync"

	"github.com/lisama569/portfolio-risk-engine-go/portfolio"
)

// Replay 同时对多个组合执行回放+回测。
//
// 隔离性：每个组合使用独立的行情库、收益历史缓冲与结果容器，goroutine 间不共享可变状态，
// 因此多组合并发与逐个串行结果逐位一致、无串扰；结果按输入顺序回填，调度顺序不影响输出。
//
// 错误边界：时间轴/配置类问题（NewEngine、空时间轴）直接返回 error；
// 单个组合的时点级数据缺陷只体现在该组合自己的 Skips/Stats 中，不影响其他组合，
// 也不会把整段回测包装成正常结论（跳过事件始终保留在报告里）。
func (e *Engine) Replay(portfolios []*portfolio.Portfolio, timeline []Point) (Report, error) {
	if len(portfolios) == 0 {
		return Report{}, errors.New("backtest: no portfolios")
	}
	if err := validateTimeline(timeline); err != nil {
		return Report{}, err
	}
	series := make([]PortfolioSeries, len(portfolios))
	errs := make([]error, len(portfolios))
	var wg sync.WaitGroup
	for i, pf := range portfolios {
		wg.Add(1)
		go func(i int, pf *portfolio.Portfolio) {
			defer wg.Done()
			s, err := e.ReplayOne(pf, timeline)
			series[i] = s
			errs[i] = err
		}(i, pf)
	}
	wg.Wait()
	// 仅配置/结构类错误才上抛；先收集再按序检查，保证行为确定。
	for i, err := range errs {
		if err != nil {
			return Report{}, fmt.Errorf("backtest: portfolio %q: %w", safeName(portfolios[i]), err)
		}
	}
	return Report{Portfolios: series}, nil
}

func safeName(pf *portfolio.Portfolio) string {
	if pf == nil {
		return "<nil>"
	}
	return pf.Name
}
