// Package market 提供本地行情数据的类型定义与校验规则。
package market

import "time"

// RejectReason 区分行情被拒绝或降级的原因。
type RejectReason string

const (
	ReasonMissing      RejectReason = "MISSING_QUOTE"   // 无该标的行情
	ReasonStale        RejectReason = "STALE_QUOTE"     // 行情超过最大时效
	ReasonInvalidPrice RejectReason = "INVALID_PRICE"   // 价格 <=0 / NaN / Inf
	ReasonDuplicate    RejectReason = "DUPLICATE_SEQ"   // 重复序号，幂等忽略
	ReasonOutOfOrder   RejectReason = "OUT_OF_ORDER"    // 乱序更新，拒绝
)

// RejectError 携带可区分的拒绝原因。
type RejectError struct {
	Symbol string
	Reason RejectReason
	Detail string
}

func (e *RejectError) Error() string {
	return string(e.Reason) + ": " + e.Symbol + ": " + e.Detail
}

// Quote 单条行情。
type Quote struct {
	Symbol string
	Price  float64
	AsOf   time.Time // 行情时间戳
	Seq    uint64    // 单调递增序号，用于去重与乱序检测
}
