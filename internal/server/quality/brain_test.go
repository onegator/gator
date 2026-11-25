package quality

import "testing"

func TestMedianAndCompare(t *testing.T) {
	if Median(nil) != 0 || Median([]int{5}) != 5 || Median([]int{9, 1, 5}) != 5 || Median([]int{4, 1, 3, 2}) != 2 {
		t.Fatal("median")
	}
	before := NewSample([]int{1000, 1200, 1100}, 6, 4)
	cases := []struct {
		name         string
		after        Sample
		held, fell   bool
		growthAtMost float64
	}{
		{"a little more context and fewer rollbacks", NewSample([]int{1300, 1250, 1200}, 8, 2), true, true, 0.2},
		{"far more context", NewSample([]int{2000, 2100}, 8, 2), false, true, 1},
		{"more rollbacks", NewSample([]int{1100}, 5, 5), true, false, 0},
		{"no work after", NewSample(nil, 0, 0), false, false, -1},
	}
	for _, c := range cases {
		got := Compare(before, c.after)
		if got.ContextHeld != c.held || got.RollbacksFell != c.fell || got.ContextGrowth > c.growthAtMost {
			t.Errorf("%s: %+v", c.name, got)
		}
	}
	if before.RollbackRate != 0.4 || before.MedianContextBytes != 1100 {
		t.Fatalf("sample: %+v", before)
	}
}
