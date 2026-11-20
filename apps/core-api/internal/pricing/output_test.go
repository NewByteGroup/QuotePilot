package pricing

import "testing"

// Spec 2026-11-11 S3b: negative margin is a hard block at approve (P5) and
// send (S5); the signal the endpoints enforce lives on Output. Only a sell
// strictly below cost trips it — pricing at cost is a pass-through, not a
// loss.
func TestOutput_NegativeMargin(t *testing.T) {
	cases := []struct {
		name      string
		cost      string
		sell      string
		wantBlock bool
	}{
		{"sell above cost", "1000.00", "1150.00", false},
		{"sell at cost", "1000.00", "1000.00", false},
		{"sell below cost", "1000.00", "999.99", true},
	}
	for _, tc := range cases {
		out := Output{TotalCostBRL: d(tc.cost), FinalPriceBRL: d(tc.sell)}
		if got := out.NegativeMargin(); got != tc.wantBlock {
			t.Fatalf("%s: NegativeMargin() = %v, want %v", tc.name, got, tc.wantBlock)
		}
	}
}
