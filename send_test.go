package main

import "testing"

func TestPairsNamed(t *testing.T) {
	records := []pairRecord{{Name: "x5-421221"}, {Name: "x7-264472"}, {Name: "x7-264472-000001"}, {Name: "x50-000001"}}
	for _, c := range []struct {
		name string
		want int
	}{{"x5", 1}, {"x5-421221", 1}, {"x7", 1}, {"x7-264472", 1}, {"x", 0}, {"x50", 1}, {"y", 0}} {
		if got := len(pairsNamed(records, c.name)); got != c.want {
			t.Errorf("pairsNamed(%q) = %d, want %d", c.name, got, c.want)
		}
	}
	if got := pairsNamed(append(records, pairRecord{Name: "x5-111111"}), "x5"); len(got) != 2 {
		t.Errorf("ambiguous base resolved to %d", len(got))
	}
}
