package pipeline

import "testing"

func TestRequestAnswered(t *testing.T) {
	cases := []struct {
		name         string
		wasRequested bool
		now          int
		want         bool
	}{
		{"request answered on GitHub", true, 0, true},
		{"still requested", true, 1, false},
		{"never requested, follow-up draft must survive syncs", false, 0, false},
		{"newly requested", false, 1, false},
		{"repo tracks all PRs, request state unknown", false, -1, false},
	}
	for _, c := range cases {
		if got := requestAnswered(c.wasRequested, c.now); got != c.want {
			t.Errorf("%s: requestAnswered(%v, %d) = %v, want %v", c.name, c.wasRequested, c.now, got, c.want)
		}
	}
}
