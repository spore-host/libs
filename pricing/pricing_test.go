package pricing

import (
	"fmt"
	"strings"
	"testing"
)

// floatEqual checks if two floats are approximately equal (within epsilon)
func floatEqual(a, b, epsilon float64) bool {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff < epsilon
}

func TestGetEC2HourlyRate(t *testing.T) {
	tests := []struct {
		name         string
		region       string
		instanceType string
		expected     float64
	}{
		{
			name:         "us-east-1 t3.micro (exact match)",
			region:       "us-east-1",
			instanceType: "t3.micro",
			expected:     0.0104,
		},
		{
			name:         "us-east-1 c5.xlarge (exact match)",
			region:       "us-east-1",
			instanceType: "c5.xlarge",
			expected:     0.17,
		},
		{
			name:         "eu-west-1 m5.large (exact match)",
			region:       "eu-west-1",
			instanceType: "m5.large",
			expected:     0.107,
		},
		{
			name:         "Case insensitive region",
			region:       "US-EAST-1",
			instanceType: "t3.micro",
			expected:     0.0104,
		},
		{
			name:         "Case insensitive instance type",
			region:       "us-east-1",
			instanceType: "T3.MICRO",
			expected:     0.0104,
		},
		{
			name:         "Whitespace trimming",
			region:       "  us-east-1  ",
			instanceType: "  t3.micro  ",
			expected:     0.0104,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := GetEC2HourlyRate(tt.region, tt.instanceType)
			if err != nil {
				t.Fatalf("expected a price for %s in %s, got error: %v", tt.instanceType, tt.region, err)
			}
			if !floatEqual(result, tt.expected, 0.00001) {
				t.Errorf("expected %.4f, got %.4f", tt.expected, result)
			}
		})
	}
}

// TestGetEC2HourlyRateErrorsRatherThanGuessing is the regression guard for #29:
// a miss must be an error, never a substituted or estimated price.
//
// The want* values are what the old implementation actually returned (measured
// against libs v0.49.0), so each case fails loudly if the guessing ever returns.
func TestGetEC2HourlyRateErrorsRatherThanGuessing(t *testing.T) {
	tests := []struct {
		name          string
		region        string
		instanceType  string
		oldFabricated float64 // what the pre-#29 code returned
		why           string
	}{
		// Modern accelerators: absent from the table, so the old code fell through
		// to unknown-family 0.10 x size-multiplier. Real rates from the AWS Price
		// List (us-east-1, 2026-07) are in the comments.
		{"g7e.4xlarge", "us-east-1", "g7e.4xlarge", 0.80, "real $3.9982 — was 5.0x low"},
		{"p5.4xlarge", "us-east-1", "p5.4xlarge", 0.80, "real $6.88 — was 8.6x low"},
		{"g6e.12xlarge", "us-east-1", "g6e.12xlarge", 2.40, "real $10.49 — was 4.4x low"},
		{"p6-b200.48xlarge", "us-east-1", "p6-b200.48xlarge", 9.60, "real $113.93 — was 11.9x low"},
		{"p5e.48xlarge", "us-east-1", "p5e.48xlarge", 9.60, "AWS publishes NO on-demand price — was pure fabrication"},

		// Unknown family / size / region: the three substitution paths.
		{"unknown family", "us-east-1", "bogus.xlarge", 0.20, "unknown family 0.10 x xlarge 2.0"},
		{"unknown size", "us-east-1", "t3.unknown", 0.1664, "unknown size defaulted to the xlarge multiplier"},
		{"region absent from table", "sa-east-1", "c5.xlarge", 0.17, "silently borrowed us-east-1's price"},
		{"region present, type absent", "us-east-1", "m7i.12xlarge", 2.4192, "estimated 0.1008 x 24.0"},
		{"no dot in type", "us-east-1", "invalid", 0.10, "bare 0.10 default"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GetEC2HourlyRate(tt.region, tt.instanceType)
			if err == nil {
				t.Fatalf("GetEC2HourlyRate(%q, %q) = %.4f with nil error — a miss must error, not guess (%s)",
					tt.region, tt.instanceType, got, tt.why)
			}
			if got != 0 {
				t.Errorf("on error the price must be 0, got %.4f", got)
			}
			if floatEqual(got, tt.oldFabricated, 0.00001) {
				t.Errorf("returned the old fabricated value %.4f — the #29 guessing is back", tt.oldFabricated)
			}
		})
	}
}

func TestFormatCost(t *testing.T) {
	tests := []struct {
		name     string
		cost     float64
		expected string
	}{
		{
			name:     "Zero cost",
			cost:     0.0,
			expected: "$0.00",
		},
		{
			name:     "Small cost",
			cost:     0.05,
			expected: "$0.05",
		},
		{
			name:     "Typical cost",
			cost:     1.25,
			expected: "$1.25",
		},
		{
			name:     "Large cost",
			cost:     123.45,
			expected: "$123.45",
		},
		{
			name:     "Very precise cost (rounded)",
			cost:     1.23456789,
			expected: "$1.23",
		},
		{
			name:     "Negative cost (edge case)",
			cost:     -5.50,
			expected: "$-5.50",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FormatCost(tt.cost)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestFormatCostDetailed(t *testing.T) {
	tests := []struct {
		name     string
		cost     float64
		expected string
	}{
		{
			name:     "Zero cost",
			cost:     0.0,
			expected: "$0.0000",
		},
		{
			name:     "Very small cost (4 decimals)",
			cost:     0.0005,
			expected: "$0.0005",
		},
		{
			name:     "Small cost under $0.01 (4 decimals)",
			cost:     0.0099,
			expected: "$0.0099",
		},
		{
			name:     "Cost at $0.01 boundary (2 decimals)",
			cost:     0.01,
			expected: "$0.01",
		},
		{
			name:     "Typical cost over $0.01 (2 decimals)",
			cost:     1.25,
			expected: "$1.25",
		},
		{
			name:     "Large cost (2 decimals)",
			cost:     123.45,
			expected: "$123.45",
		},
		{
			name:     "Very precise small cost",
			cost:     0.00123456,
			expected: "$0.0012",
		},
		{
			name:     "Very precise large cost",
			cost:     1.23456789,
			expected: "$1.23",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FormatCostDetailed(tt.cost)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestEstimateSweepCost(t *testing.T) {
	tests := []struct {
		name        string
		params      *ParamFileFormat
		wantErr     bool
		checkResult func(*testing.T, *CostEstimate)
	}{
		{
			name: "Single instance, 1 hour",
			params: &ParamFileFormat{
				Defaults: map[string]interface{}{
					"instance_type": "t3.micro",
					"region":        "us-east-1",
					"ttl":           "1h",
				},
				Params: []map[string]interface{}{
					{"name": "job1"},
				},
			},
			wantErr: false,
			checkResult: func(t *testing.T, e *CostEstimate) {
				expectedCompute := 0.0104 // t3.micro for 1 hour
				if e.ComputeCost != expectedCompute {
					t.Errorf("expected compute cost %.4f, got %.4f", expectedCompute, e.ComputeCost)
				}
				if e.TotalCost <= e.ComputeCost {
					t.Error("total cost should include Lambda and storage")
				}
			},
		},
		{
			name: "Multiple instances, same config",
			params: &ParamFileFormat{
				Defaults: map[string]interface{}{
					"instance_type": "c5.xlarge",
					"region":        "us-east-1",
					"ttl":           "2h",
				},
				Params: []map[string]interface{}{
					{"name": "job1"},
					{"name": "job2"},
					{"name": "job3"},
				},
			},
			wantErr: false,
			checkResult: func(t *testing.T, e *CostEstimate) {
				expectedCompute := 0.17 * 2.0 * 3.0 // 3 instances, 2 hours each
				if e.ComputeCost != expectedCompute {
					t.Errorf("expected compute cost %.4f, got %.4f", expectedCompute, e.ComputeCost)
				}
			},
		},
		{
			name: "Mixed regions",
			params: &ParamFileFormat{
				Defaults: map[string]interface{}{
					"instance_type": "t3.micro",
					"ttl":           "1h",
				},
				Params: []map[string]interface{}{
					{"name": "job1", "region": "us-east-1"},
					{"name": "job2", "region": "eu-west-1"},
				},
			},
			wantErr: false,
			checkResult: func(t *testing.T, e *CostEstimate) {
				expectedCompute := 0.0104 + 0.0114 // Different prices per region
				if e.ComputeCost != expectedCompute {
					t.Errorf("expected compute cost %.4f, got %.4f", expectedCompute, e.ComputeCost)
				}
			},
		},
		{
			name: "Mixed instance types",
			params: &ParamFileFormat{
				Defaults: map[string]interface{}{
					"region": "us-east-1",
					"ttl":    "1h",
				},
				Params: []map[string]interface{}{
					{"name": "job1", "instance_type": "t3.micro"},
					{"name": "job2", "instance_type": "c5.xlarge"},
				},
			},
			wantErr: false,
			checkResult: func(t *testing.T, e *CostEstimate) {
				expectedCompute := 0.0104 + 0.17
				if e.ComputeCost != expectedCompute {
					t.Errorf("expected compute cost %.4f, got %.4f", expectedCompute, e.ComputeCost)
				}
			},
		},
		{
			name: "No instance type specified",
			params: &ParamFileFormat{
				Defaults: map[string]interface{}{
					"region": "us-east-1",
				},
				Params: []map[string]interface{}{
					{"name": "job1"},
				},
			},
			wantErr: true,
		},
		{
			name: "Invalid TTL format",
			params: &ParamFileFormat{
				Defaults: map[string]interface{}{
					"instance_type": "t3.micro",
					"region":        "us-east-1",
					"ttl":           "invalid",
				},
				Params: []map[string]interface{}{
					{"name": "job1"},
				},
			},
			wantErr: true,
		},
		{
			name: "Default 1 hour TTL when not specified",
			params: &ParamFileFormat{
				Defaults: map[string]interface{}{
					"instance_type": "t3.micro",
					"region":        "us-east-1",
				},
				Params: []map[string]interface{}{
					{"name": "job1"},
				},
			},
			wantErr: false,
			checkResult: func(t *testing.T, e *CostEstimate) {
				expectedCompute := 0.0104 // 1 hour default
				if e.ComputeCost != expectedCompute {
					t.Errorf("expected compute cost %.4f, got %.4f", expectedCompute, e.ComputeCost)
				}
			},
		},
		{
			name: "Default us-east-1 region when not specified",
			params: &ParamFileFormat{
				Defaults: map[string]interface{}{
					"instance_type": "t3.micro",
					"ttl":           "1h",
				},
				Params: []map[string]interface{}{
					{"name": "job1"},
				},
			},
			wantErr: false,
			checkResult: func(t *testing.T, e *CostEstimate) {
				expectedCompute := 0.0104 // us-east-1 price
				if e.ComputeCost != expectedCompute {
					t.Errorf("expected compute cost %.4f, got %.4f", expectedCompute, e.ComputeCost)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// nil resolver => StaticRateResolver; every type above is in the
			// static table, so these assert the offline-but-honest default.
			result, err := EstimateSweepCost(tt.params, nil)
			if (err != nil) != tt.wantErr {
				t.Errorf("expected error: %v, got: %v", tt.wantErr, err)
				return
			}

			if err == nil && tt.checkResult != nil {
				tt.checkResult(t, result)
				if result.Partial() {
					t.Errorf("every row here is priceable, so the estimate must not be partial; unpriced=%v", result.UnpricedRows)
				}
			}
		})
	}
}

// TestEstimateSweepCostUsesInjectedResolver proves the resolver seam is actually
// used, rather than the static table being consulted behind the caller's back.
func TestEstimateSweepCostUsesInjectedResolver(t *testing.T) {
	var asked [][2]string
	resolver := func(region, instanceType string) (float64, error) {
		asked = append(asked, [2]string{region, instanceType})
		return 10.0, nil // deliberately unlike any static-table value
	}

	est, err := EstimateSweepCost(&ParamFileFormat{
		Defaults: map[string]interface{}{"instance_type": "t3.micro", "region": "us-east-1", "ttl": "2h"},
		Params:   []map[string]interface{}{{"name": "job1"}},
	}, resolver)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(asked) != 1 || asked[0] != [2]string{"us-east-1", "t3.micro"} {
		t.Errorf("resolver should have been asked once for (us-east-1, t3.micro), got %v", asked)
	}
	// 10.0/hr x 2h = 20.0. The static table's t3.micro is 0.0104, so a wrong
	// number here means the injected resolver was bypassed.
	if !floatEqual(est.ComputeCost, 20.0, 0.00001) {
		t.Errorf("expected compute 20.00 from the injected resolver, got %.4f", est.ComputeCost)
	}
	if est.PricedRows != 1 || est.Partial() {
		t.Errorf("expected 1 priced row and a complete estimate, got priced=%d unpriced=%v", est.PricedRows, est.UnpricedRows)
	}
}

// TestEstimateSweepCostPartialWhenRowsUnpriceable is the #29 guard at the sweep
// level: an unpriceable row must be named and excluded, never valued at a guess
// and never silently at $0 inside a total that looks complete.
func TestEstimateSweepCostPartialWhenUnpriceable(t *testing.T) {
	resolver := func(region, instanceType string) (float64, error) {
		if instanceType == "g6e.12xlarge" {
			return 10.49, nil
		}
		return 0, fmt.Errorf("no price for %s in %s", instanceType, region)
	}

	est, err := EstimateSweepCost(&ParamFileFormat{
		Defaults: map[string]interface{}{"region": "us-east-1", "ttl": "1h"},
		Params: []map[string]interface{}{
			{"name": "priceable", "instance_type": "g6e.12xlarge"},
			{"name": "unpriceable", "instance_type": "p5e.48xlarge"},
			{"name": "dup-unpriceable", "instance_type": "p5e.48xlarge"},
		},
	}, resolver)
	if err != nil {
		t.Fatalf("a mixed sweep should still estimate, got error: %v", err)
	}

	if est.PricedRows != 1 {
		t.Errorf("expected 1 priced row, got %d", est.PricedRows)
	}
	if !floatEqual(est.ComputeCost, 10.49, 0.00001) {
		t.Errorf("compute must count only the priced row (10.49), got %.4f", est.ComputeCost)
	}
	if !est.Partial() {
		t.Fatal("estimate must report Partial() when a row could not be priced")
	}
	if want := []string{"p5e.48xlarge@us-east-1"}; len(est.UnpricedRows) != 1 || est.UnpricedRows[0] != want[0] {
		t.Errorf("expected deduplicated unpriced rows %v, got %v", want, est.UnpricedRows)
	}

	// Both display forms must disclose the shortfall — a total that reads as
	// complete over a partial sum is the original defect restated.
	for name, out := range map[string]string{"Display": est.Display(), "DisplayCompact": est.DisplayCompact()} {
		if !strings.Contains(out, "p5e.48xlarge@us-east-1") {
			t.Errorf("%s must name the unpriced row, got:\n%s", name, out)
		}
		if !strings.Contains(strings.ToUpper(out), "FLOOR") {
			t.Errorf("%s must mark the total as a floor, got:\n%s", name, out)
		}
	}
}

func TestCostEstimateDisplay(t *testing.T) {
	estimate := &CostEstimate{
		ComputeCost: 10.50,
		LambdaCost:  0.0005,
		StorageCost: 0.00001,
		TotalCost:   10.50051,
	}

	t.Run("Display format", func(t *testing.T) {
		result := estimate.Display()

		// Check that output contains expected components
		expectedStrings := []string{
			"$10.50",  // Compute cost
			"$0.0005", // Lambda cost (detailed)
			"$0.0000", // Storage cost (detailed, very small)
			"$10.50",  // Total cost
			"Compute",
			"Lambda",
			"Storage",
			"Total",
		}

		for _, expected := range expectedStrings {
			if !strings.Contains(result, expected) {
				t.Errorf("expected output to contain %q, got: %s", expected, result)
			}
		}
	})

	t.Run("DisplayCompact format", func(t *testing.T) {
		result := estimate.DisplayCompact()

		expectedStrings := []string{
			"Total estimated cost:",
			"$10.50",
			"EC2:",
			"Lambda:",
			"S3:",
		}

		for _, expected := range expectedStrings {
			if !strings.Contains(result, expected) {
				t.Errorf("expected output to contain %q, got: %s", expected, result)
			}
		}
	})
}

func TestGetStringValuePricing(t *testing.T) {
	// Same tests as sweep package, but using pricing package's version
	tests := []struct {
		name         string
		m            map[string]interface{}
		key          string
		defaultValue string
		expected     string
	}{
		{
			name: "Key exists with string value",
			m: map[string]interface{}{
				"instance_type": "t3.micro",
			},
			key:          "instance_type",
			defaultValue: "default",
			expected:     "t3.micro",
		},
		{
			name: "Key does not exist",
			m: map[string]interface{}{
				"instance_type": "t3.micro",
			},
			key:          "region",
			defaultValue: "us-east-1",
			expected:     "us-east-1",
		},
		{
			name: "Key exists with non-string value",
			m: map[string]interface{}{
				"count": 42,
			},
			key:          "count",
			defaultValue: "default",
			expected:     "default",
		},
		{
			name: "Empty string value",
			m: map[string]interface{}{
				"region": "",
			},
			key:          "region",
			defaultValue: "us-east-1",
			expected:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getStringValue(tt.m, tt.key, tt.defaultValue)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}
