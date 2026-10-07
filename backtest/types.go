// Package backtest 在历史时间轴上回放风险计量并对模型做回测：
// 逐时点重算 VaR/ES（只用该时点及之前的数据，杜绝未来函数），
// 再用下一期真实损益检验阈值突破情况，并区分各种预热不足与数据缺陷。
package backtest

import (
	"time"

	"github.com/lisama569/portfolio-risk-engine-go/market"
)

// SkipReason 区分某个时点被跳过（不产生风险结论）的原因。
type SkipReason string

const (
	ReasonWarmup         SkipReason = "WARMUP_INSUFFICIENT" // 可用历史收益样本不足预热窗口
	ReasonMissingQuote   SkipReason = "MISSING_QUOTE"       // 某持仓标的在该时点无行情
	ReasonStaleQuote     SkipReason = "STALE_QUOTE"         // 某持仓行情相对该时点已过期
	ReasonInvalidQuote   SkipReason = "INVALID_QUOTE"       // 该时点行情非法（价格 <=0/NaN/Inf）
	ReasonMissingReturn  SkipReason = "MISSING_RETURN"      // 某持仓标的在该时点缺收益数据
	ReasonInvalidReturn  SkipReason = "INVALID_RETURN"      // 收益为 NaN/Inf，禁止以零顶替
	ReasonDuplicateQuote SkipReason = "DUPLICATE_QUOTE"     // 该时点某标的行情序号重复（重复装载）
	ReasonOutOfOrder     SkipReason = "OUT_OF_ORDER"        // 该时点某标的行情序号倒退
	ReasonDegenerate     SkipReason = "DEGENERATE_RISK"     // 风险阈值退化（VaR<=0），不制造"零风险"假象
)

// Point 时间轴上的一个时点：该时点已知的行情（不含未来）与各标的当期实现收益。
// RealizedReturn 为该时点所覆盖区间内实际发生的收益；最后一个时点没有后续区间，
// 其 RealizedReturn 不会用于任何突破统计。
type Point struct {
	AsOf           time.Time
	Quotes         []market.Quote // 截至该时点新增/生效的行情
	RealizedReturn map[string]float64
}

// Config 回放与回测配置。
type Config struct {
	Methods      []string      // 计量口径，如 "historical"、"parametric"
	Confidence   float64       // VaR/ES 置信水平
	MinHistory   int           // 预热所需最少历史收益样本数；<=0 时按 1 处理
	MaxStaleness time.Duration // 行情最大时效；<=0 表示不做时效校验
}

// MethodRisk 单个口径在单个时点的计量结果。
type MethodRisk struct {
	Method string
	VaR    float64 // 损失为正
	ES     float64
}

// PointResult 某组合在某时点的结果：要么得到各口径风险，要么记录可区分的跳过原因。
type PointResult struct {
	Index       int
	AsOf        time.Time
	OK          bool
	Risks       []MethodRisk
	SkipReason  SkipReason // !OK 时有效
	SkipDetail  string
	RealizedPnL float64         // 该时点之后一期的真实组合损益（货币单位）
	HasNext     bool            // 是否存在可对照的后续区间
	NextClean   bool            // 后续区间收益是否完整合法（否则该点不纳入突破统计）
	Breaches    map[string]bool // method -> 是否突破（真实损失 > VaR）；仅 HasNext&&NextClean 时填充
}

// SkipEvent 一次时点级跳过事件，按组合记录。
type SkipEvent struct {
	Index  int
	AsOf   time.Time
	Reason SkipReason
	Detail string
}

// MethodStats 单个口径的回测统计。
type MethodStats struct {
	Method         string
	EligiblePoints int     // 纳入对照的有效时点数（有风险结论且后续区间数据干净）
	Breaches       int     // 真实损失超过 VaR 的次数
	BreachRate     float64 // 突破比例 = Breaches / EligiblePoints
	WorstLoss      float64 // 最差一期亏损（损失为正；无有效样本时为 0）
}

// PortfolioSeries 单个组合的完整回放序列与回测统计。
type PortfolioSeries struct {
	Portfolio string
	Points    []PointResult
	Skips     []SkipEvent
	Stats     []MethodStats
}

// Report 整段回放/回测报告（可同时包含多个组合，彼此独立）。
type Report struct {
	Portfolios []PortfolioSeries
}

// Engine 沿时间轴回放的回测引擎，对每个组合独立推进，互不影响。
type Engine struct {
	config Config
}
