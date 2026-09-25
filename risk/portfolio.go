package risk

import (
	"fmt"
	"sort"
	"time"
)

// Normalize validates raw positions against an immutable market snapshot.
//
// Hard rejections (invalid holding, duplicate asset, bad price present in
// the request path) are returned aggregated in *LoadError and never
// silently dropped. Missing/stale quotes and missing/short history are
// degradations: the offending asset is excluded, its issue is carried on
// the returned Portfolio, weights of covered assets are renormalized to
// sum to 1, and Coverage reports retained gross weight (1.0 = full).
func Normalize(name string, positions []Position, snap *Snapshot, now time.Time) (*Portfolio, error) {
	pf := &Portfolio{
		Name:      name,
		Weights:   map[AssetID]float64{},
		Exposures: map[AssetID]float64{},
		Coverage:  1,
	}
	if snap == nil {
		return nil, &LoadError{Issues: []Issue{{Kind: IssueUnknownAsset, Detail: "nil market snapshot"}}}
	}
	if now.IsZero() {
		now = time.Now()
	}

	type rawHolding struct {
		asset    AssetID
		weight   float64
		exposure float64
	}
	holdings := make(map[AssetID]rawHolding, len(positions))
	var order []AssetID
	var hard []Issue

	for _, pos := range positions {
		if pos.Asset == "" {
			hard = append(hard, Issue{Kind: IssueInvalidHolding, Detail: "position with empty asset id"})
			continue
		}
		if _, dup := holdings[pos.Asset]; dup {
			hard = append(hard, Issue{Asset: pos.Asset, Kind: IssueInvalidHolding,
				Detail: "duplicate position for asset"})
			continue
		}
		hasW := pos.Weight != 0
		hasE := pos.Exposure != 0
		if hasW == hasE {
			hard = append(hard, Issue{Asset: pos.Asset, Kind: IssueInvalidHolding,
				Detail: "exactly one of Weight or Exposure must be non-zero"})
			continue
		}
		var w float64
		if hasW {
			w = pos.Weight
			if !isFinite(w) {
				hard = append(hard, Issue{Asset: pos.Asset, Kind: IssueInvalidHolding,
					Detail: "weight is non-finite"})
				continue
			}
		} else {
			if !isFinite(pos.Exposure) {
				hard = append(hard, Issue{Asset: pos.Asset, Kind: IssueInvalidHolding,
					Detail: "exposure is non-finite"})
				continue
			}
		}
		holdings[pos.Asset] = rawHolding{pos.Asset, pos.Weight, pos.Exposure}
		order = append(order, pos.Asset)
	}

	if len(hard) > 0 {
		return nil, &LoadError{Issues: hard}
	}
	if len(holdings) == 0 {
		return nil, &LoadError{Issues: []Issue{{Kind: IssueInvalidHolding, Detail: "portfolio has no valid positions"}}}
	}

	// Coverage pass: distinguish missing / stale / invalid quote and
	// missing / short history. Raw weight contribution uses |weight| or
	// |exposure/price| so both input styles share one denominator.
	grossAll := 0.0
	rawW := make(map[AssetID]float64, len(order))
	for _, a := range order {
		h := holdings[a]
		q, hasQ := snap.Quote(a)
		switch {
		case h.weight != 0:
			rawW[a] = h.weight
		case hasQ && isFinite(q.Price) && q.Price > 0:
			rawW[a] = h.exposure / q.Price
		default:
			rawW[a] = h.exposure // unpriced: nominal fallback, never covered
		}
		grossAll += absF(rawW[a])
	}
	grossCovered := 0.0
	var covered []AssetID
	for _, a := range order {
		q, ok := snap.Quote(a)
		if !ok {
			pf.Issues = append(pf.Issues, Issue{Asset: a, Kind: IssueMissingQuote,
				Detail: "no quote available; asset excluded (degraded)"})
			continue
		}
		if !isFinite(q.Price) || q.Price <= 0 {
			// Defensive: LoadQuotes/Apply should already reject this.
			pf.Issues = append(pf.Issues, Issue{Asset: a, Kind: IssueInvalidPrice,
				Detail: "stored price is not finite-positive (degraded)"})
			continue
		}
		if age := now.Sub(q.AsOf); age > snap.staleWin {
			pf.Issues = append(pf.Issues, Issue{Asset: a, Kind: IssueStaleQuote,
				Detail: fmt.Sprintf("quote age %s exceeds window %s; asset excluded (degraded)",
					age.Truncate(time.Second), snap.staleWin)})
			continue
		}
		r, hasHist := snap.History(a)
		if !hasHist {
			pf.Issues = append(pf.Issues, Issue{Asset: a, Kind: IssueNoHistory,
				Detail: "no return history; asset excluded (degraded)"})
			continue
		}
		if len(r.Values) < snap.minHist {
			pf.Issues = append(pf.Issues, Issue{Asset: a, Kind: IssueShortHistory,
				Detail: fmt.Sprintf("only %d observations (< %d); asset excluded (degraded)",
					len(r.Values), snap.minHist)})
			continue
		}
		covered = append(covered, a)
		grossCovered += absF(rawW[a])
	}

	if len(covered) == 0 {
		// Degradation consumed everything; surface as hard error so the
		// caller cannot mistake it for a fully-invested result.
		return nil, &LoadError{Issues: appendIssueCopy(pf.Issues, Issue{
			Kind:   IssueInvalidHolding,
			Detail: "every position was degraded away; no covered assets remain",
		})}
	}

	// Weight normalization: scale supplied signed weights so retained
	// gross = 1.0, preserving long/short signs.
	scale := 0.0
	if grossCovered > 0 {
		scale = 1 / grossCovered
	}
	for _, a := range covered {
		w := rawW[a] * scale
		pf.Weights[a] = w
		if holdings[a].weight != 0 {
			pf.Exposures[a] = w // fraction-of-NAV exposure proxy
		} else {
			pf.Exposures[a] = holdings[a].exposure
		}
		pf.Assets = append(pf.Assets, a)
	}
	if grossAll > 0 {
		pf.Coverage = grossCovered / grossAll
	} else {
		pf.Coverage = 1
	}
	sort.Slice(pf.Assets, func(i, j int) bool { return pf.Assets[i] < pf.Assets[j] })
	return pf, nil
}

func appendIssueCopy(in []Issue, extra Issue) []Issue {
	out := make([]Issue, 0, len(in)+1)
	out = append(out, in...)
	out = append(out, extra)
	return out
}

func absF(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
