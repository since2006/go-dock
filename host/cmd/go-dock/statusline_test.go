package main

import "testing"

func TestFallbackLine(t *testing.T) {
	cases := map[string]string{
		`{"model":{"display_name":"Opus 5.5"},"rate_limits":{"five_hour":{"used_percentage":62.4},"seven_day":{"used_percentage":31}}}`: "Opus 5.5 · 5h 62% · 7d 31%",
		`{"model":{"display_name":"Opus 5.5"}}`: "Opus 5.5",
		`{}`:                                    "go-dock",
		`not json`:                              "go-dock",
	}
	for in, want := range cases {
		if got := fallbackLine([]byte(in)); got != want {
			t.Errorf("fallbackLine(%s) = %q, want %q", in, got, want)
		}
	}
}
