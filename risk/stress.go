package risk

import "fmt"

// Shock is a price scenario for one asset. Exactly one of Relative
// (multiplicative return, e.g. -0.10 for -10%) or Absolute (price shift
// in quote currency) is applied, selected by UseRelative.
type Shock struct {
	Asset       AssetID
	Relative    float64
	Absolute    float64
	UseRelative bool
}

// Scenario is a named bundle of simultaneous shocks.
type Scenario struct {
	Name   string
	Shocks []Shock
}

// ApplyScenario returns the hypothetical portfolio P&L (fraction of NAV,
// negative = loss) under the scenario. Shocks on assets without a usable
// quote produce distinguishable issues and are excluded from the total —
// never silently priced at zero.
func ApplyScenario(p *Portfolio, snap *Snapshot, sc Scenario) (pl float64, issues []Issue) {
	if p == nil || snap == nil {
		return 0, []Issue{{Kind: IssueUnknownAsset, Detail: "nil portfolio or snapshot"}}
	}
	// Aggregate shocks per asset so repeated entries compose
	// multiplicatively instead of double-pricing independently.
	type agg struct {
		rel   float64
		abs   float64
		hasR  bool
		hasA  bool
		order int
	}
	combined := map[AssetID]*agg{}
	var order []AssetID
	for i, sh := range sc.Shocks {
		a := sh.Asset
		c, ok := combined[a]
		if !ok {
			c = &agg{rel: 1, order: i}
			combined[a] = c
			order = append(order, a)
		}
		if sh.UseRelative {
			c.rel *= 1 + sh.Relative
			c.hasR = true
		} else {
			c.abs += sh.Absolute
			c.hasA = true
		}
	}

	for _, a := range order {
		c := combined[a]
		w, held := p.Weights[a]
		if !held {
			continue // shock on unheld asset has no P&L effect
		}
		q, ok := snap.Quote(a)
		if !ok {
			issues = append(issues, Issue{Asset: a, Kind: IssueMissingQuote,
				Detail: fmt.Sprintf("scenario %q: no quote to shock; contribution excluded", sc.Name)})
			continue
		}
		if !isFinite(q.Price) || q.Price <= 0 {
			issues = append(issues, Issue{Asset: a, Kind: IssueInvalidPrice,
				Detail: fmt.Sprintf("scenario %q: stored price not finite-positive", sc.Name)})
			continue
		}
		ret := 0.0
		if c.hasR {
			ret += c.rel - 1
		}
		if c.hasA {
			newPrice := q.Price*(1+ret) + c.abs
			if newPrice <= 0 || !isFinite(newPrice) {
				issues = append(issues, Issue{Asset: a, Kind: IssueInvalidPrice,
					Detail: fmt.Sprintf("scenario %q: shocked price %v not positive-finite", sc.Name, newPrice)})
				continue
			}
			ret = newPrice/q.Price - 1
		}
		pl += w * ret
	}
	return pl, issues
}
