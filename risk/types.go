package risk

import (
	"fmt"
	"time"
)

// AssetID is the unique identifier for a tradable asset.
type AssetID string

// Position describes a holding in the portfolio.
// Exactly one of Weight (fraction of gross NAV) or Exposure (currency
// amount) may be supplied; the loader normalizes to both forms.
type Position struct {
	Asset    AssetID
	Weight   float64 // fraction of gross exposure; 0 means "use Exposure"
	Exposure float64 // signed currency exposure; 0 means "use Weight"
}

// Quote is a single market observation for an asset.
type Quote struct {
	Asset  AssetID
	Price  float64
	AsOf   time.Time
	Source string
}

// Returns is a date-ordered (oldest -> newest) series of simple returns.
type Returns struct {
	Asset  AssetID
	Values []float64
	AsOf   []time.Time
}

// IssueKind enumerates the distinguishable failure / degradation reasons
// attached to an asset during loading and calculation.
type IssueKind string

const (
	IssueNone           IssueKind = ""
	IssueMissingQuote   IssueKind = "missing_quote"
	IssueStaleQuote     IssueKind = "stale_quote"
	IssueInvalidPrice   IssueKind = "invalid_price"
	IssueInvalidHolding IssueKind = "invalid_holding"
	IssueNoHistory      IssueKind = "no_history"
	IssueShortHistory   IssueKind = "short_history"
	IssueInvalidReturns IssueKind = "invalid_returns"
	IssueDupUpdate      IssueKind = "duplicate_update"
	IssueOutOfOrder     IssueKind = "out_of_order_update"
	IssueUnknownAsset   IssueKind = "unknown_asset"
)

// Issue records a rejection or degradation reason for one asset.
type Issue struct {
	Asset  AssetID
	Kind   IssueKind
	Detail string
}

func (i Issue) Error() string {
	if i.Asset == "" {
		return string(i.Kind) + ": " + i.Detail
	}
	return string(i.Kind) + " [" + string(i.Asset) + "]: " + i.Detail
}

// Degraded is true when the issue only reduces coverage instead of
// rejecting the whole request.
func (k IssueKind) Degraded() bool {
	switch k {
	case IssueMissingQuote, IssueStaleQuote, IssueNoHistory, IssueShortHistory:
		return true
	default:
		return false
	}
}

// Portfolio is a normalized set of holdings. Weights sum to 1 over the
// retained (covered) assets; Coverage is retained gross weight / total
// gross weight of the input.
type Portfolio struct {
	Name      string
	Assets    []AssetID
	Weights   map[AssetID]float64
	Exposures map[AssetID]float64
	Coverage  float64
	Issues    []Issue
}

// ConfLevel is a risk confidence level, e.g. 0.99 for 99%.
type ConfLevel float64

// Method selects the risk calculation methodology.
type Method string

const (
	MethodHistorical Method = "historical"
	MethodParametric Method = "parametric"
)

// Result is the risk output for one portfolio / method / confidence level.
type Result struct {
	Portfolio  string
	Method     Method
	Confidence ConfLevel
	VaR        float64 // loss, expressed as a positive number (fraction of NAV)
	ES         float64 // expected shortfall, positive fraction of NAV
	Degraded   bool
	Issues     []Issue
	InputsHash string // snapshot identifier for reproducibility checks
}

// StaleWindow is the max acceptable quote age relative to a reference time.
type StaleWindow = time.Duration

// MinHistory is the minimum number of return observations required;
// below it the asset is degraded out (short_history).
type MinHistory int

// LoadError aggregates hard rejection issues (as opposed to degradations,
// which are carried on the Portfolio / Result themselves).
type LoadError struct {
	Issues []Issue
}

func (e *LoadError) Error() string {
	if e == nil || len(e.Issues) == 0 {
		return "risk: validation failed"
	}
	msg := fmt.Sprintf("risk: %d rejection issue(s): ", len(e.Issues))
	for i, is := range e.Issues {
		if i > 0 {
			msg += "; "
		}
		msg += is.Error()
	}
	return msg
}
