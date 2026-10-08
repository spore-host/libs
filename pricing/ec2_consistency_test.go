package pricing

import (
	"math"
	"regexp"
	"strings"
	"testing"
)

var sizeMultiple = map[string]float64{
	"large": 1, "xlarge": 2, "2xlarge": 4, "3xlarge": 6, "4xlarge": 8,
	"6xlarge": 12, "8xlarge": 16, "9xlarge": 18, "12xlarge": 24,
	"16xlarge": 32, "18xlarge": 36, "24xlarge": 48, "32xlarge": 64,
	"48xlarge": 96,
}

// TestPriceConsistencyWithinFamily is the gate that matters for truffle#175.
//
// These rates back --cost-limit when the Price List API is unavailable, so a
// MISTYPED price is far worse than an old one: it silently under- or
// over-enforces a spend cap and nothing downstream can tell. Staleness at least
// moves in a predictable direction; a transposed digit does not.
//
// EC2 On-Demand scales linearly with size inside a family — a 2xlarge is exactly
// 4x a large — so linearity is a property a typo cannot survive. All 405
// Graviton rates added in #175 satisfied it on the first run, which is what made
// them trustworthy enough to commit.
func TestPriceConsistencyWithinFamily(t *testing.T) {
	checked := 0
	for region, prices := range EC2Pricing {
		// family -> size -> price
		byFamily := map[string]map[string]float64{}
		for typ, price := range prices {
			dot := strings.Index(typ, ".")
			if dot < 0 {
				t.Errorf("%s: %q is not a <family>.<size> instance type", region, typ)
				continue
			}
			fam, size := typ[:dot], typ[dot+1:]
			if _, known := sizeMultiple[size]; !known {
				continue // metal, flex and odd sizes are not linear; skip rather than guess
			}
			if byFamily[fam] == nil {
				byFamily[fam] = map[string]float64{}
			}
			byFamily[fam][size] = price
		}

		for fam, sizes := range byFamily {
			base, ok := sizes["large"]
			if !ok || base <= 0 {
				continue // no anchor in this region; nothing to compare against
			}
			for size, got := range sizes {
				want := base * sizeMultiple[size]
				checked++
				// 2% tolerance: the rates carry four to six decimals and the
				// table trims trailing zeros, so exact equality is too strict.
				if math.Abs(got-want)/want > 0.02 {
					t.Errorf("%s %s.%s = %g, but %s.large is %g so it should be ~%g "+
						"(%.1f%% off). EC2 On-Demand is linear in size, so this is a "+
						"typo rather than a real rate — and these back --cost-limit.",
						region, fam, size, got, fam, base, want,
						100*math.Abs(got-want)/want)
				}
			}
		}
	}
	if checked < 300 {
		t.Fatalf("only %d size relationships checked; the matcher is probably broken and "+
			"this gate would pass vacuously", checked)
	}
}

// TestGravitonCoverageIsNoLongerEmpty is the regression guard for #175 itself.
//
// Before the fix the table had NO Graviton compute, general-purpose or memory
// family in any region, so a throttled Price List call plus --cost-limit refused
// any Graviton launch. The reporter hit it on c7g.2xlarge in us-west-2 with
// --cost-limit 0.15.
func TestGravitonCoverageIsNoLongerEmpty(t *testing.T) {
	// The exact case from the report must resolve.
	if p := EC2Pricing["us-west-2"]["c7g.2xlarge"]; p <= 0 {
		t.Errorf("c7g.2xlarge in us-west-2 has no static price — this is literally the "+
			"launch #175 reported being refused (got %g)", p)
	}

	grav := regexp.MustCompile(`^(c|m|r)[6-9]g`)
	for region, prices := range EC2Pricing {
		n := 0
		for typ := range prices {
			if grav.MatchString(typ) {
				n++
			}
		}
		if n == 0 {
			t.Errorf("%s has zero Graviton compute/general-purpose/memory entries, so a "+
				"throttled Price List call there still refuses every Graviton launch "+
				"with a cost limit (#175)", region)
		}
	}
}

// TestPricesAsOfIsADate keeps the table's age machine-readable rather than prose.
func TestPricesAsOfIsADate(t *testing.T) {
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`).MatchString(PricesAsOf) {
		t.Errorf("PricesAsOf = %q, want YYYY-MM-DD", PricesAsOf)
	}
}
