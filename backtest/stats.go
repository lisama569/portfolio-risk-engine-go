package backtest

// computeStats 汇总各口径的回测统计。
// 有效样本（EligiblePoints）仅包括：该时点成功算出风险、存在后续区间、
// 且后续区间收益完整合法（NextClean）的点。被跳过或后续数据脏的点绝不计入，
// 也不会以零损益顶替，避免把缺陷包装成"看着正常"的突破率。
func computeStats(s *PortfolioSeries, methods []string) []MethodStats {
	out := make([]MethodStats, 0, len(methods))
	for _, m := range methods {
		st := MethodStats{Method: m}
		for _, p := range s.Points {
			if !p.OK || !p.HasNext || !p.NextClean {
				continue
			}
			breached, tracked := p.Breaches[m]
			if !tracked {
				continue // 该口径在此点未产出（防御：不应发生）
			}
			st.EligiblePoints++
			loss := -p.RealizedPnL // 损失为正
			if loss > st.WorstLoss {
				st.WorstLoss = loss
			}
			if breached {
				st.Breaches++
			}
		}
		if st.EligiblePoints > 0 {
			st.BreachRate = float64(st.Breaches) / float64(st.EligiblePoints)
		}
		out = append(out, st)
	}
	return out
}

// FindStats 按口径名取统计，未找到返回 false。
func (s *PortfolioSeries) FindStats(method string) (MethodStats, bool) {
	for _, st := range s.Stats {
		if st.Method == method {
			return st, true
		}
	}
	return MethodStats{}, false
}

// AllClean 报告整段回放是否没有任何跳过事件。
// 注意：没有跳过不等于回测可信，仍需结合样本数与突破率判断。
func (s *PortfolioSeries) AllClean() bool { return len(s.Skips) == 0 }

// SkipsByReason 按原因分组统计跳过次数，便于审计。
func (s *PortfolioSeries) SkipsByReason() map[SkipReason]int {
	m := make(map[SkipReason]int)
	for _, sk := range s.Skips {
		m[sk.Reason]++
	}
	return m
}
