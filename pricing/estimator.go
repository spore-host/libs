package pricing

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// CostEstimate represents the estimated cost breakdown for a parameter sweep.
//
// When RateResolver could not price every row, TotalCost covers only the rows it
// could price and [CostEstimate.Partial] reports true. A partial total is a floor,
// not an estimate, and every surface that prints one must say so — quoting a
// confident number over a partial sum is how #29's fabricated rates misled
// callers in the first place.
type CostEstimate struct {
	ComputeCost float64 // Total EC2 compute cost (priced rows only)
	LambdaCost  float64 // Lambda orchestration cost
	StorageCost float64 // S3 parameter storage cost
	TotalCost   float64 // Sum of all costs (priced rows only)

	PricedRows int // rows whose instance type/region resolved to a rate

	// UnpricedRows names the rows that could not be priced, as "type@region",
	// deduplicated and sorted. These contribute $0 to the totals above.
	UnpricedRows []string
}

// Partial reports whether at least one row could not be priced, making TotalCost
// a lower bound rather than an estimate of the whole sweep.
func (e *CostEstimate) Partial() bool { return len(e.UnpricedRows) > 0 }

// RateResolver returns the hourly On-Demand rate for an instance type in a
// region, or an error when it cannot determine one.
//
// It exists so the caller supplies the pricing authority: libs cannot import
// truffle (truffle imports libs), so spawn passes a resolver backed by truffle's
// live Price List lookup, while an offline caller can pass nil and get
// [StaticRateResolver].
//
// A resolver must return an error rather than a substituted or estimated rate —
// see [GetEC2HourlyRate] and #29.
type RateResolver func(region, instanceType string) (float64, error)

// StaticRateResolver prices rows from the embedded static table by exact match,
// erroring on a miss. It makes no network calls and needs no credentials, so it
// is the default when EstimateSweepCost is given a nil resolver — at the cost of
// not being able to price anything outside [EC2Pricing], notably any GPU family
// newer than p4d.
func StaticRateResolver(region, instanceType string) (float64, error) {
	return GetEC2HourlyRate(region, instanceType)
}

// ParamFileFormat matches the sweep parameter file structure
// Duplicated here to avoid circular dependency
type ParamFileFormat struct {
	Defaults map[string]interface{}   `json:"defaults"`
	Params   []map[string]interface{} `json:"params"`
}

// EstimateSweepCost calculates the estimated cost for a parameter sweep, pricing
// each row through rate.
//
// A nil rate uses [StaticRateResolver]. Rows that rate cannot price are recorded
// in [CostEstimate.UnpricedRows] and contribute nothing to the totals, so the
// result is explicitly partial rather than quietly wrong (#29). An unpriceable row
// is not an error: a sweep mixing priceable and unpriceable types should still
// report what it knows, provided it also reports what it doesn't.
func EstimateSweepCost(params *ParamFileFormat, rate RateResolver) (*CostEstimate, error) {
	if rate == nil {
		rate = StaticRateResolver
	}
	estimate := &CostEstimate{}
	unpriced := map[string]struct{}{}

	// Calculate compute cost for each parameter set
	for i, paramSet := range params.Params {
		// Get instance type (from param set or defaults)
		instanceType := getStringValue(paramSet, "instance_type", "")
		if instanceType == "" {
			if defaults, ok := params.Defaults["instance_type"].(string); ok {
				instanceType = defaults
			} else {
				return nil, fmt.Errorf("param set %d: no instance_type specified", i)
			}
		}

		// Get region (from param set or defaults)
		region := getStringValue(paramSet, "region", "")
		if region == "" {
			if defaults, ok := params.Defaults["region"].(string); ok {
				region = defaults
			} else {
				region = "us-east-1" // Default region
			}
		}

		// Get TTL (from param set or defaults)
		ttlStr := getStringValue(paramSet, "ttl", "")
		if ttlStr == "" {
			if defaults, ok := params.Defaults["ttl"].(string); ok {
				ttlStr = defaults
			} else {
				ttlStr = "1h" // Default 1 hour
			}
		}

		// Parse TTL duration
		ttl, err := time.ParseDuration(ttlStr)
		if err != nil {
			return nil, fmt.Errorf("param set %d: invalid ttl format %s: %w", i, ttlStr, err)
		}
		hours := ttl.Hours()

		// Price the row. A resolver that cannot price this (type, region) is
		// recorded and skipped — never valued at a guess, and never at $0
		// silently.
		hourlyRate, err := rate(region, instanceType)
		if err != nil || hourlyRate <= 0 {
			unpriced[fmt.Sprintf("%s@%s", instanceType, region)] = struct{}{}
			continue
		}

		estimate.PricedRows++
		estimate.ComputeCost += hourlyRate * hours
	}

	for row := range unpriced {
		estimate.UnpricedRows = append(estimate.UnpricedRows, row)
	}
	sort.Strings(estimate.UnpricedRows)

	// Lambda cost estimation
	// Lambda: $0.0000166667 per GB-second
	// Assume 512MB memory, estimate 5 minutes runtime per 10 instances
	numParams := len(params.Params)
	estimatedLambdaSeconds := float64(numParams) * 30.0 // 30 seconds per instance estimate
	if estimatedLambdaSeconds < 300 {
		estimatedLambdaSeconds = 300 // Minimum 5 minutes
	}
	memorySizeGB := 512.0 / 1024.0
	estimate.LambdaCost = 0.0000166667 * memorySizeGB * estimatedLambdaSeconds

	// S3 storage cost (very small, usually negligible)
	// $0.023 per GB per month, assume params file < 1MB, prorated for 1 day
	estimate.StorageCost = 0.023 * 0.001 * (1.0 / 30.0)

	// Total cost
	estimate.TotalCost = estimate.ComputeCost + estimate.LambdaCost + estimate.StorageCost

	return estimate, nil
}

// Display formats the cost estimate for a human. When the estimate is partial it
// labels the total as a floor and names the rows that could not be priced, so the
// number is never read as covering the whole sweep.
func (e *CostEstimate) Display() string {
	totalLabel := "Total:         "
	if e.Partial() {
		totalLabel = "Total (FLOOR): "
	}

	b := &strings.Builder{}
	fmt.Fprintf(b, `Estimated cost for this sweep:
  Compute (EC2):  %s
  Lambda:         %s (orchestration)
  Storage (S3):   %s (parameters)
  ────────────────────────────────
  %s %s`,
		FormatCost(e.ComputeCost),
		FormatCostDetailed(e.LambdaCost),
		FormatCostDetailed(e.StorageCost),
		totalLabel,
		FormatCost(e.TotalCost))

	if e.Partial() {
		fmt.Fprintf(b, "\n\n  ⚠️  %d row(s) could not be priced and are NOT included above:\n       %s\n      The real cost is HIGHER than this total — treat it as a lower bound.",
			len(e.UnpricedRows), strings.Join(e.UnpricedRows, ", "))
	}
	return b.String()
}

// DisplayCompact formats the cost estimate in a compact format, marking a partial
// total rather than presenting it as complete.
func (e *CostEstimate) DisplayCompact() string {
	s := fmt.Sprintf("Total estimated cost: %s (EC2: %s, Lambda: %s, S3: %s)",
		FormatCost(e.TotalCost),
		FormatCost(e.ComputeCost),
		FormatCostDetailed(e.LambdaCost),
		FormatCostDetailed(e.StorageCost))
	if e.Partial() {
		s += fmt.Sprintf(" — FLOOR ONLY, %d row(s) unpriced: %s", len(e.UnpricedRows), strings.Join(e.UnpricedRows, ", "))
	}
	return s
}

// Helper function to get string values from param map
func getStringValue(m map[string]interface{}, key, defaultValue string) string {
	if val, ok := m[key]; ok {
		if str, ok := val.(string); ok {
			return str
		}
	}
	return defaultValue
}
