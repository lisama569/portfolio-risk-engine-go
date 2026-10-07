// Package backtest 在现有风险引擎之上提供按时间推进的历史回放（replay）
// 与模型回测（backtest）：每个时点只允许使用该时点及之前的数据，绝不偷看未来。
package backtest

import (
	"time"

	"github.com/lisama569/portfolio-risk-engine-go/risk"
)

// Reason 区分回测时点被跳过 / 失败的原因，彼此可判别。
type Reason string

const (
	ReasonWarmup        Reason = "WARMUP"            // 历史收益样本不足，尚在预热
	ReasonNoRealizedPnL Reason = "NO_REALIZED_PNL"   // 该时点之后没有真实损益，无法对照
	ReasonMissingQuote  Reason = "MISSING_QUOTE"     // 持仓标的在该时点无行情
	ReasonStaleQuote    Reason = "STALE_QUOTE"       // 行情相对该时点已过期
	ReasonInvalidData   Reason = "INVALID_DATA"      // 价格或收益为 NaN/Inf/<=0 等非法值
	ReasonDuplicateSeq  Reason = "DUPLICATE_SEQ"     // 重复序号，幂等忽略
	ReasonOutOfOrder    Reason = "OUT_OF_ORDER"      // 序号倒退，拒绝
	ReasonMissingReturn Reason = "MISSING_RETURN"    // 该时点缺少某标的的历史/真实收益
	ReasonDegenerate    Reason = "DEGENERATE_SERIES" // 损益序列退化（方差为 0），风险为零的假象
	ReasonInternal      Reason = "INTERNAL"          // 其他不应发生的内部错误
)

// Status 单个时点的处理状态。
type Status string

const (
	StatusEvaluated Status = "EVALUATED" // 成功算出风险；回测时还完成了真实损益对照
	StatusWarmup    Status = "WARMUP"    // 预热不足，跳过
	StatusSkipped   Status = "SKIPPED"   // 无真实损益可对照等，风险仍可用但不纳入回测统计
	StatusFailed    Status = "FAILED"    // 该时点失败（携带可区分原因），绝不静默顶替
)

// Step 回放时间轴上的一个时点：
// AsOf 为该时点；Quotes 为在该时点"送达"的行情（不含 AsOf 之后的未来行情）；
// Returns 为截至该时点已知的各标的历史收益（Returns[t] 为第 t 期实现的收益）；
// NextReturns 为该时点之后下一期真实实现的收益（回测对照用，风险计量时严禁使用）。
type Step struct {
	AsOf        time.Time
	Quotes      map[string]QuoteTick // symbol -> 该时点送达的行情
	Returns     map[string][]float64 // symbol -> 截至该时点已知的历史收益
	NextReturns map[string]float64   // symbol -> 下一期真实收益（仅回测对照）
}

// QuoteTick 一个时点送达的单条行情。
type QuoteTick struct {
	Price float64
	Seq   uint64
}

// Dataset 一段历史回放数据：按时间轴排列的时点序列（必须严格递增）。
type Dataset struct {
	Steps []Step
}

// Config 回放 / 回测配置。
type Config struct {
	Methods      []string      // 计量口径："historical" / "parametric"
	Confidence   float64       // 置信水平，如 0.99
	MinSamples   int           // 预热所需最少历史收益样本数
	MaxStaleness time.Duration // 行情最大允许时效
}

// PointRisk 单个时点的风险数值序列元素。
type PointRisk struct {
	AsOf   time.Time
	Result risk.Result
}

// Issue 单个时点的问题记录（跳过或失败），原因可区分、不静默吞没。
type Issue struct {
	AsOf   time.Time
	Status Status
	Reason Reason
	Detail string
}

// PointOutcome 回测中单个时点的风险阈值与真实损益对照。
type PointOutcome struct {
	AsOf     time.Time
	VaR      float64 // 风险阈值（损失为正）
	ES       float64
	Realized float64 // 该时点之后下一期的真实组合损益（正为盈利）
	Loss     float64 // -Realized，损失为正
	Breached bool    // Loss > VaR 记为一次突破
}

// Report 单个 (组合, 口径) 的回测结论。
type Report struct {
	Portfolio  string
	Method     string
	Series     []PointRisk    // 所有时点的风险数值（含预热/失败标记外的完整轨迹）
	Outcomes   []PointOutcome // 成功对照的时点
	Issues     []Issue        // 跳过/失败记录，原因可区分
	Evaluated  int
	Breaches   int
	BreachRate float64 // Breaches / Evaluated
	WorstLoss  float64 // 最差一期损失（损失为正）；无样本时为 NaN
	Healthy    bool    // 仅当无 FAILED 时点且至少有一个成功对照时为 true
}

// ReportSet 一次多组合多口径回测的全部结论。
type ReportSet struct {
	Reports []*Report
}
