package risk

import (
	"fmt"
	"math"
	"sort"
)

// alignHistories intersects per-asset return series on common dates and
// returns a per-asset aligned matrix (assets x commonDates), all in
// chronological order. Assets whose dates do not overlap the common
// calendar enough produce a distinguishable issue and are dropped from
// the matrix (never silently zero-filled).
func alignHistories(assets []AssetID, hist map[AssetID]Returns, minHist int) (matrix [][]float64, kept []AssetID, issues []Issue) {
	// commonDates = intersection of all assets' date sets.
	var common map[int64]struct{}
	for _, a := range assets {
		r, ok := hist[a]
		if !ok || len(r.Values) == 0 {
			issues = append(issues, Issue{Asset: a, Kind: IssueNoHistory,
				Detail: "no history available for alignment"})
			continue
		}
		dates := make(map[int64]struct{}, len(r.AsOf))
		for _, t := range r.AsOf {
			dates[t.Unix()] = struct{}{}
		}
		if common == nil {
			common = dates
		} else {
			for d := range common {
				if _, ok := dates[d]; !ok {
					delete(common, d)
				}
			}
		}
	}
	if len(common) < minHist {
		issues = append(issues, Issue{Kind: IssueShortHistory,
			Detail: fmt.Sprintf("only %d common return dates across assets (< %d)", len(common), minHist)})
		return nil, nil, issues
	}
	sortedDates := make([]int64, 0, len(common))
	for d := range common {
		sortedDates = append(sortedDates, d)
	}
	sort.Slice(sortedDates, func(i, j int) bool { return sortedDates[i] < sortedDates[j] })

	for _, a := range assets {
		r, ok := hist[a]
		if !ok || len(r.Values) == 0 {
			continue // issue already recorded above
		}
		byDate := make(map[int64]float64, len(r.Values))
		for i, t := range r.AsOf {
			byDate[t.Unix()] = r.Values[i]
		}
		row := make([]float64, len(sortedDates))
		for j, d := range sortedDates {
			row[j] = byDate[d]
		}
		matrix = append(matrix, row)
		kept = append(kept, a)
	}
	return matrix, kept, issues
}

// PortfolioPnL computes the historical portfolio P&L series (fraction of
// NAV, negative = loss) as the weight-aligned sum of asset returns.
func PortfolioPnL(assets []AssetID, weights map[AssetID]float64, hist map[AssetID]Returns, minHist int) ([]float64, []Issue) {
	matrix, kept, issues := alignHistories(assets, hist, minHist)
	if len(kept) == 0 {
		return nil, issues
	}
	n := len(matrix[0])
	pl := make([]float64, n)
	for i, a := range kept {
		w := weights[a]
		for j := 0; j < n; j++ {
			pl[j] += w * matrix[i][j]
		}
	}
	return pl, issues
}

// HistoricalVaR computes VaR and expected shortfall from a P&L series by
// empirical quantile (linear interpolation, R type-7). Losses are
// returned as positive fractions of NAV. Deterministic for a given input.
func HistoricalVaR(pl []float64, c ConfLevel) (var_, es float64) {
	if len(pl) == 0 {
		return 0, 0
	}
	sorted := make([]float64, len(pl))
	copy(sorted, pl)
	sort.Float64s(sorted)
	alpha := 1 - float64(c) // tail probability, e.g. 0.01
	q := quantileType7(sorted, alpha)
	// ES: mean of P&L at or below the alpha-quantile.
	sum, cnt := 0.0, 0
	for _, v := range sorted {
		if v <= q {
			sum += v
			cnt++
		}
	}
	if cnt == 0 {
		sum, cnt = sorted[0], 1
	}
	return -q, -(sum / float64(cnt))
}

// quantileType7 implements the R-7 / numpy-linear empirical quantile:
// h = (n-1)*p; result = x[floor(h)] + frac*(x[floor(h)+1]-x[floor(h)]).
func quantileType7(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 1 {
		return sorted[0]
	}
	h := float64(n-1) * p
	f := math.Floor(h)
	i := int(f)
	if i >= n-1 {
		return sorted[n-1]
	}
	frac := h - f
	return sorted[i] + frac*(sorted[i+1]-sorted[i])
}

// ParametricVaR assumes jointly normal returns. It aligns histories on
// common dates, estimates the sample mean vector and covariance matrix,
// and returns VaR / ES as positive loss fractions of NAV.
func ParametricVaR(assets []AssetID, weights map[AssetID]float64, hist map[AssetID]Returns, c ConfLevel, minHist int) (var_, es float64, issues []Issue) {
	matrix, kept, issues := alignHistories(assets, hist, minHist)
	if len(kept) == 0 {
		return 0, 0, issues
	}
	n := len(matrix[0])
	k := len(kept)
	means := make([]float64, k)
	for i := range matrix {
		for _, v := range matrix[i] {
			means[i] += v
		}
		means[i] /= float64(n)
	}
	// Portfolio mean and variance: mu = w'm, var = w'Cw.
	mu := 0.0
	wv := make([]float64, k)
	for i, a := range kept {
		wv[i] = weights[a]
		mu += wv[i] * means[i]
	}
	var portVar float64
	if n > 1 {
		denom := float64(n - 1)
		for i := 0; i < k; i++ {
			for j := 0; j < k; j++ {
				cov := 0.0
				for t := 0; t < n; t++ {
					cov += (matrix[i][t] - means[i]) * (matrix[j][t] - means[j])
				}
				cov /= denom
				portVar += wv[i] * cov * wv[j]
			}
		}
	}
	if portVar < 0 {
		portVar = 0 // numerical guard
	}
	sigma := math.Sqrt(portVar)
	alpha := 1 - float64(c)
	z := normalQuantile(float64(c)) // Phi^{-1}(c), e.g. 2.326 for 0.99
	var_ = z*sigma - mu
	// ES for normal: sigma * phi(z_alpha)/alpha - mu, with z_alpha the
	// lower-tail quantile; phi symmetric so phi(z_alpha) = phi(z).
	es = sigma*normalPDF(z)/alpha - mu
	return var_, es, issues
}

// normalPDF is the standard normal density.
func normalPDF(x float64) float64 {
	return math.Exp(-0.5*x*x) / math.Sqrt(2*math.Pi)
}

// normalQuantile returns the inverse standard normal CDF using Acklam's
// rational approximation (max abs error ~1.15e-9). Deterministic.
func normalQuantile(p float64) float64 {
	if p <= 0 {
		return math.Inf(-1)
	}
	if p >= 1 {
		return math.Inf(1)
	}
	const (
		a1 = -3.969683028665376e+01
		a2 = 2.209460984245205e+02
		a3 = -2.759285104469687e+02
		a4 = 1.383577518672690e+02
		a5 = -3.066479806614716e+01
		a6 = 2.506628277459239e+00
		b1 = -5.447609879822406e+01
		b2 = 1.615858368580409e+02
		b3 = -1.556989798598866e+02
		b4 = 6.680131188771972e+01
		b5 = -1.328068155288572e+01
		c1 = -7.784894002430293e-03
		c2 = -3.223964580411365e-01
		c3 = -2.400758277161838e+00
		c4 = -2.549732539343734e+00
		c5 = 4.374664141464968e+00
		c6 = 2.938163982698783e+00
		d1 = 7.784695709041462e-03
		d2 = 3.224671290700398e-01
		d3 = 2.445134137142996e+00
		d4 = 3.754408661907416e+00
	)
	const plow = 0.02425
	const phigh = 1 - plow
	var q, r float64
	switch {
	case p < plow:
		q = math.Sqrt(-2 * math.Log(p))
		return (((((c1*q+c2)*q+c3)*q+c4)*q+c5)*q + c6) /
			((((d1*q+d2)*q+d3)*q+d4)*q + 1)
	case p > phigh:
		q = math.Sqrt(-2 * math.Log(1-p))
		return -(((((c1*q+c2)*q+c3)*q+c4)*q+c5)*q + c6) /
			((((d1*q+d2)*q+d3)*q+d4)*q + 1)
	default:
		q = p - 0.5
		r = q * q
		return (((((a1*r+a2)*r+a3)*r+a4)*r+a5)*r + a6) * q /
			(((((b1*r+b2)*r+b3)*r+b4)*r+b5)*r + 1)
	}
}
