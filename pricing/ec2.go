package pricing

import (
	"fmt"
	"strings"
)

// EC2Pricing holds approximate hourly On-Demand rates for a small, deliberately
// incomplete set of EC2 instance types, by region (rates as of 2026-01).
//
// This table exists for OFFLINE use — a rough estimate when no credentials or no
// network are available. It is **not** a pricing database and must not be grown
// into one: a hand-maintained table of AWS prices is stale the day it is written,
// and the staleness is invisible at the call site.
//
// truffle is the suite's pricing authority for real rates. It queries the AWS
// Price List API and falls back to this table by *exact* (region, instanceType)
// lookup, erroring when the table has no such entry — see
// truffle/pkg/aws/pricing.go's staticOnDemandPricer. Use that, not this, whenever
// a price will be shown to a user or enforced against a spend cap.
//
// Coverage is skewed towards older general-purpose/compute families. In
// particular there is **no GPU family newer than p4d**, so any current-generation
// accelerator (g6, g6e, g7e, p5, p5e, p6-*, trn*, inf*) is absent by design and
// will not resolve here.
var EC2Pricing = map[string]map[string]float64{
	"us-east-1": {
		// General Purpose
		"t3.micro":    0.0104,
		"t3.small":    0.0208,
		"t3.medium":   0.0416,
		"t3.large":    0.0832,
		"t3.xlarge":   0.1664,
		"t3.2xlarge":  0.3328,
		"t4g.micro":   0.0084,
		"t4g.small":   0.0168,
		"t4g.medium":  0.0336,
		"t4g.large":   0.0672,
		"t4g.xlarge":  0.1344,
		"t4g.2xlarge": 0.2688,
		"m5.large":    0.096,
		"m5.xlarge":   0.192,
		"m5.2xlarge":  0.384,
		"m5.4xlarge":  0.768,
		"m5.8xlarge":  1.536,
		"m6i.large":   0.096,
		"m6i.xlarge":  0.192,
		"m6i.2xlarge": 0.384,
		"m6i.4xlarge": 0.768,
		"m7i.large":   0.1008,
		"m7i.xlarge":  0.2016,
		"m7i.2xlarge": 0.4032,
		// Compute Optimized
		"c5.large":    0.085,
		"c5.xlarge":   0.17,
		"c5.2xlarge":  0.34,
		"c5.4xlarge":  0.68,
		"c5.9xlarge":  1.53,
		"c6i.large":   0.085,
		"c6i.xlarge":  0.17,
		"c6i.2xlarge": 0.34,
		"c6i.4xlarge": 0.68,
		"c7i.large":   0.0893,
		"c7i.xlarge":  0.1785,
		"c7i.2xlarge": 0.357,
		"c7i.4xlarge": 0.714,
		// Memory Optimized
		"r5.large":    0.126,
		"r5.xlarge":   0.252,
		"r5.2xlarge":  0.504,
		"r5.4xlarge":  1.008,
		"r6i.large":   0.126,
		"r6i.xlarge":  0.252,
		"r6i.2xlarge": 0.504,
		// GPU
		"g4dn.xlarge":  0.526,
		"g4dn.2xlarge": 0.752,
		"g5.xlarge":    1.006,
		"g5.2xlarge":   1.212,
		"p3.2xlarge":   3.06,
		"p4d.24xlarge": 32.77,
	},
	"us-east-2": {
		"t3.micro":    0.0104,
		"t3.small":    0.0208,
		"t3.medium":   0.0416,
		"t3.large":    0.0832,
		"m5.large":    0.096,
		"m5.xlarge":   0.192,
		"c5.large":    0.085,
		"c5.xlarge":   0.17,
		"c7i.xlarge":  0.1785,
		"c7i.4xlarge": 0.714,
		"r5.large":    0.126,
		"r5.xlarge":   0.252,
	},
	"us-west-1": {
		"t3.micro":    0.0116,
		"t3.small":    0.0232,
		"t3.medium":   0.0464,
		"t3.large":    0.0928,
		"m5.large":    0.107,
		"m5.xlarge":   0.214,
		"c5.large":    0.094,
		"c5.xlarge":   0.188,
		"c7i.xlarge":  0.199,
		"c7i.4xlarge": 0.796,
	},
	"us-west-2": {
		"t3.micro":    0.0104,
		"t3.small":    0.0208,
		"t3.medium":   0.0416,
		"t3.large":    0.0832,
		"m5.large":    0.096,
		"m5.xlarge":   0.192,
		"m6i.xlarge":  0.192,
		"c5.large":    0.085,
		"c5.xlarge":   0.17,
		"c7i.xlarge":  0.1785,
		"c7i.4xlarge": 0.714,
		"r5.large":    0.126,
		"r5.xlarge":   0.252,
		"g4dn.xlarge": 0.526,
		"g5.xlarge":   1.006,
	},
	"eu-west-1": {
		"t3.micro":   0.0114,
		"t3.small":   0.0228,
		"t3.medium":  0.0456,
		"t3.large":   0.0912,
		"m5.large":   0.107,
		"m5.xlarge":  0.214,
		"c5.large":   0.094,
		"c5.xlarge":  0.188,
		"c7i.xlarge": 0.199,
		"r5.large":   0.14,
		"r5.xlarge":  0.28,
	},
	"eu-central-1": {
		"t3.micro":   0.012,
		"t3.small":   0.024,
		"t3.medium":  0.048,
		"t3.large":   0.096,
		"m5.large":   0.113,
		"m5.xlarge":  0.226,
		"c5.large":   0.099,
		"c5.xlarge":  0.198,
		"c7i.xlarge": 0.21,
		"r5.large":   0.148,
	},
	"ap-southeast-1": {
		"t3.micro":  0.0116,
		"t3.small":  0.0232,
		"t3.medium": 0.0464,
		"t3.large":  0.0928,
		"m5.large":  0.107,
		"m5.xlarge": 0.214,
		"c5.large":  0.094,
		"c5.xlarge": 0.188,
	},
	"ap-northeast-1": {
		"t3.micro":  0.0128,
		"t3.small":  0.0256,
		"t3.medium": 0.0512,
		"t3.large":  0.1024,
		"m5.large":  0.118,
		"m5.xlarge": 0.236,
		"c5.large":  0.103,
		"c5.xlarge": 0.206,
	},
}

// GetEC2HourlyRate returns the hourly On-Demand rate for instanceType in region
// from the static [EC2Pricing] table, by exact match.
//
// It returns an error when the table has no entry for that exact (region,
// instanceType) pair. It does NOT substitute a price: a caller that cannot be
// told "I don't know" has no way to distinguish a real rate from an invented one,
// and an invented rate is worse than no rate because it looks usable.
//
// This previously returned a bare float64 and guessed on a miss (#29): an unknown
// region silently borrowed us-east-1's prices, and an unknown instance type fell
// through to a per-family-size estimate — unknown family 0.10 × unknown size 2.0.
// For modern accelerators the guess was 4-12x low and carried err == nil, so
// g7e.4xlarge came back as $0.80 against a real $3.9982, and p5e.48xlarge as
// $9.60 for a type AWS publishes no on-demand price for at all. Callers used
// those numbers to quote costs and size budgets.
//
// For a real rate, prefer truffle's pricer (live AWS Price List, with this table
// as an exact-match fallback). See [EC2Pricing] for why this table is small.
func GetEC2HourlyRate(region, instanceType string) (float64, error) {
	regionKey := strings.ToLower(strings.TrimSpace(region))
	typeKey := strings.ToLower(strings.TrimSpace(instanceType))

	regionPricing, ok := EC2Pricing[regionKey]
	if !ok {
		return 0, fmt.Errorf("no static price table for region %q (static table covers %d regions; use truffle for a live price)", region, len(EC2Pricing))
	}

	price, ok := regionPricing[typeKey]
	if !ok || price <= 0 {
		return 0, fmt.Errorf("no static price for %q in %q (not in the static table; use truffle for a live price)", instanceType, region)
	}

	return price, nil
}

// FormatCost formats a cost value as a currency string
func FormatCost(cost float64) string {
	return fmt.Sprintf("$%.2f", cost)
}

// FormatCostDetailed formats cost with more precision for small amounts
func FormatCostDetailed(cost float64) string {
	if cost < 0.01 {
		return fmt.Sprintf("$%.4f", cost)
	}
	return fmt.Sprintf("$%.2f", cost)
}
