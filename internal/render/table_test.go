package render

import "testing"

// A table is read by a person, so a float is trimmed to the value it means
// rather than to the shortest string that reads back as the same double. The
// machine formats keep the exact value.
func TestTableFloat(t *testing.T) {
	tests := map[float64]string{
		4963.4400000000005:  "4963.44",
		4739.699999999994:   "4739.7",
		200:                 "200",
		0.5:                 "0.5",
		-1.2000000000000002: "-1.2",
		1e20:                "100000000000000000000",
	}

	for in, want := range tests {
		if got := tableFloat(in); got != want {
			t.Errorf("tableFloat(%v) = %q, want %q", in, got, want)
		}
	}
}
