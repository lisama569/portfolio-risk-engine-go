// Package risk 提供风险计量口径：历史模拟法与参数法。
package risk

import (
	"math"
	"sort"
)

// Result 一次风险计量输出。
type Result struct {
	Method     string  // "historical" 或 "parametric"
	Confidence float64 // 置信水平，如 0.99
	VaR        float64 // 风险价值（损失为正）
	ES         float64 // 期望损失（超过 VaR 的平均损失）
}

// Historical 历史模拟法：对组合历史损益序列排序取分位，确定性可复现。
// pnl 为组合历史损益（正为盈利）。VaR 取损失分布的 confidence 分位，
// ES 为超过 VaR 的损失均值。分位点采用最近秩法（向上取整），无插值歧义。
func Historical(pnl []float64, confidence float64) Result {
	r := Result{Method: "historical", Confidence: confidence}
	if len(pnl) == 0 {
		return r
	}
	losses := make([]float64, len(pnl))
	for i, p := range pnl {
		losses[i] = -p
	}
	sort.Float64s(losses) // 升序：尾部为大额损失
	// 最近秩：k = ceil(c * n)，索引 k-1
	k := int(math.Ceil(confidence * float64(len(losses))))
	if k < 1 {
		k = 1
	}
	if k > len(losses) {
		k = len(losses)
	}
	r.VaR = losses[k-1]
	var tailSum float64
	for _, l := range losses[k-1:] {
		tailSum += l
	}
	r.ES = tailSum / float64(len(losses)-k+1)
	return r
}

// Parametric 参数法（方差-协方差）：假设组合收益正态 N(mean, stddev^2)，
// VaR = -(mean - z*stddev)，ES = -(mean - stddev*phi(z)/alpha)，alpha = 1-confidence。
func Parametric(mean, stddev, confidence float64) Result {
	r := Result{Method: "parametric", Confidence: confidence}
	alpha := 1 - confidence
	z := normInv(confidence)
	r.VaR = -(mean - z*stddev)
	r.ES = -(mean - stddev*normPDF(z)/alpha)
	return r
}

// normPDF 标准正态密度。
func normPDF(x float64) float64 {
	return math.Exp(-0.5*x*x) / math.Sqrt(2*math.Pi)
}

// normInv 标准正态逆 CDF（Acklam 有理逼近，确定性实现，最大误差 ~1.15e-9）。
func normInv(p float64) float64 {
	if p <= 0 {
		return math.Inf(-1)
	}
	if p >= 1 {
		return math.Inf(1)
	}
	a := [...]float64{-3.969683028665376e+01, 2.209460984245205e+02,
		-2.759285104469687e+02, 1.383577518672690e+02,
		-3.066479806614716e+01, 2.506628277459239e+00}
	b := [...]float64{-5.447609879822406e+01, 1.615858368580409e+02,
		-1.556989798598866e+02, 6.680131188771972e+01,
		-1.328068155288572e+01}
	c := [...]float64{-7.784894002430293e-03, -3.223964580411365e-01,
		-2.400758277161838e+00, -2.549732539343734e+00,
		4.374664141464968e+00, 2.938163982698783e+00}
	d := [...]float64{7.784695709041462e-03, 3.224671290700398e-01,
		2.445134137142996e+00, 3.754408661907416e+00}
	const plow, phigh = 0.02425, 1 - 0.02425
	var q, x float64
	switch {
	case p < plow:
		q = math.Sqrt(-2 * math.Log(p))
		x = (((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) /
			((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	case p <= phigh:
		q = p - 0.5
		r2 := q * q
		x = (((((a[0]*r2+a[1])*r2+a[2])*r2+a[3])*r2+a[4])*r2 + a[5]) * q /
			(((((b[0]*r2+b[1])*r2+b[2])*r2+b[3])*r2+b[4])*r2 + 1)
	default:
		q = math.Sqrt(-2 * math.Log(1-p))
		x = -(((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) /
			((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	}
	// 一步 Halley 精化，提高精度且保持确定性
	e := 0.5*math.Erfc(-x/math.Sqrt2) - p
	u := e * math.Sqrt(2*math.Pi) * math.Exp(x*x/2)
	x = x - u/(1+x*u/2)
	return x
}
