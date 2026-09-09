package main

import (
	"testing"
)

// Numbers are compared by value, so a page may spell a rate any way it
// likes as long as it means the same amount.
func TestRatesAreComparedByValue(t *testing.T) {
	cases := []struct {
		page, rate string
		want       bool
	}{
		{"input $12.50 per million", "12.5", true},
		{"input $12.5 per million", "12.50", true},
		{"output $75.00 per million", "75", true},
		{"output $75 per million", "75.00", true},
		{"cache read $0.075", "0.075", true},
		{"cache read $0.0750", "0.075", true},
		// Different values must not match, however they are spelled.
		{"input $12.51 per million", "12.5", false},
		{"input $1.25 per million", "12.5", false},
	}
	for _, c := range cases {
		t.Run(c.page+"/"+c.rate, func(t *testing.T) {
			if got := containsRate(c.page, c.rate); got != c.want {
				t.Errorf("containsRate(%q, %q) = %t, want %t", c.page, c.rate, got, c.want)
			}
		})
	}
}

// The regression this closes: a page that no longer mentions $10 must not
// match merely because it says $1 somewhere.
func TestWholeNumberRateIsNotMatchedByItsPrefix(t *testing.T) {
	page := "Our plans start at $1 per seat and $100 for the team tier."
	if containsRate(page, "10") {
		t.Error("a page without $10 must not match the rate 10")
	}
	if !containsRate(page, "100") {
		t.Error("a page showing $100 must match the rate 100")
	}
}

func TestDecimalRateMatchesEitherSpelling(t *testing.T) {
	if !containsRate("input costs $12.50 per million", "12.5") {
		t.Error("a table storing 12.5 must match a page printing $12.50")
	}
	if !containsRate("input costs 12.5 per million", "12.50") {
		t.Error("a table storing 12.50 must match a page printing 12.5")
	}
}

// Substring search finds "10" inside "$100", so the match must respect
// number boundaries. This is the case that made the whole tool unreliable.
func TestRateIsNotMatchedInsideALongerNumber(t *testing.T) {
	cases := []struct {
		page, rate string
		want       bool
	}{
		{"plans from $100 per month", "10", false},
		{"plans from $100 per month", "100", true},
		{"input is $10 per million", "10", true},
		{"input is $10.50 per million", "10", false},
		{"grouped as 1,050,000 tokens", "1050000", true},
		{"tokens: 1050000", "10", false},
		{"output $75.00 per million", "75", true},
		{"context 272000 tokens", "272000", true},
		{"context 2720000 tokens", "272000", false},
	}
	for _, c := range cases {
		t.Run(c.page+"/"+c.rate, func(t *testing.T) {
			if got := containsRate(c.page, c.rate); got != c.want {
				t.Errorf("containsRate(%q, %q) = %t, want %t", c.page, c.rate, got, c.want)
			}
		})
	}
}
