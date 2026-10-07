package stepif

import "testing"

func TestEvaluate(t *testing.T) {
	cases := []struct {
		name           string
		ifVal          string
		priorSucceeded bool
		priorFailed    bool
		want           bool
	}{
		{"empty no priors runs", "", true, false, true},
		{"empty all prior succeeded runs", "", true, false, true},
		{"empty prior failed skips", "", false, true, false},
		{"success mirror of empty", "success", true, false, true},
		{"success after failure skips", "success", false, true, false},
		{"failure after nothing fails skips", "failure", true, false, false},
		{"failure after prior failure runs", "failure", false, true, true},
		{"failure after skip runs", "failure", false, true, true},
		{"always runs after failure", "always", false, true, true},
		{"always runs after success", "always", true, false, true},
		{"invalid conservative skip", "sometimes", true, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Evaluate(c.ifVal, c.priorSucceeded, c.priorFailed); got != c.want {
				t.Errorf("Evaluate(%q, %v, %v) = %v, want %v",
					c.ifVal, c.priorSucceeded, c.priorFailed, got, c.want)
			}
		})
	}
}
