package cli

import (
	"errors"
	"strings"
	"testing"
)

func TestParseSelection(t *testing.T) {
	t.Parallel()

	const count = 5

	cases := []struct {
		name, answer string
		want         []int // 0-based
	}{
		{"single", "3", []int{2}},
		{"space separated", "1 3", []int{0, 2}},
		{"comma separated", "1,3", []int{0, 2}},
		{"mixed separators", "1, 3 5", []int{0, 2, 4}},
		{"range", "2-4", []int{1, 2, 3}},
		{"range plus single", "1 4-5", []int{0, 3, 4}},
		{"all", "all", []int{0, 1, 2, 3, 4}},
		{"all shorthand", "a", []int{0, 1, 2, 3, 4}},
		{"sorted and deduplicated", "5 1 5 2-3", []int{0, 1, 2, 4}},
		{"one element range", "2-2", []int{1}},
	}

	for _, c := range cases {
		got, err := parseSelection(c.answer, count)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)

			continue
		}

		if len(got) != len(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)

			continue
		}

		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: got %v, want %v", c.name, got, c.want)

				break
			}
		}
	}
}

func TestParseSelectionNothing(t *testing.T) {
	t.Parallel()

	for _, answer := range []string{"", "   ", "q", "quit", "none", "NONE"} {
		_, err := parseSelection(answer, 5)
		if !errors.Is(err, errNothingSelected) {
			t.Errorf("%q: got %v, want errNothingSelected", answer, err)
		}
	}
}

func TestParseSelectionRejects(t *testing.T) {
	t.Parallel()

	cases := []struct {
		answer, wantErr string
	}{
		{"0", "out of range"},
		{"6", "out of range"},
		{"2-9", "out of range"},
		{"4-2", "runs backwards"},
		{"x", "not a number"},
		{"1 x", "not a number"},
	}

	for _, c := range cases {
		_, err := parseSelection(c.answer, 5)
		if err == nil {
			t.Errorf("%q: expected an error", c.answer)

			continue
		}

		if !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%q: error %q does not mention %q", c.answer, err, c.wantErr)
		}
	}
}
