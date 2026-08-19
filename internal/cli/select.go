package cli

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// selectAll is the picker answer that takes every candidate.
const selectAll = "all"

// errNothingSelected means the operator answered the picker with nothing.
var errNothingSelected = errors.New("nothing selected")

// parseSelection turns a picker answer into 0-based indexes into a list of
// count items, sorted ascending and deduplicated. It accepts numbers, ranges
// and "all": "1 3", "1,3", "2-4", "1 4-6", "all". An empty answer, "q" or
// "none" selects nothing.
func parseSelection(answer string, count int) ([]int, error) {
	answer = strings.TrimSpace(answer)

	switch strings.ToLower(answer) {
	case "", "q", "quit", "none":
		return nil, errNothingSelected
	case "a", selectAll:
		all := make([]int, count)
		for i := range all {
			all[i] = i
		}

		return all, nil
	}

	chosen := map[int]bool{}

	for _, field := range strings.FieldsFunc(answer, func(r rune) bool { return r == ' ' || r == ',' }) {
		from, to, err := parseRange(field, count)
		if err != nil {
			return nil, err
		}

		for i := from; i <= to; i++ {
			chosen[i-1] = true
		}
	}

	if len(chosen) == 0 {
		return nil, errNothingSelected
	}

	out := make([]int, 0, len(chosen))
	for i := range chosen {
		out = append(out, i)
	}

	sort.Ints(out)

	return out, nil
}

// parseRange reads "n" or "n-m" as an inclusive 1-based range.
func parseRange(field string, count int) (int, int, error) {
	head, tail, isRange := strings.Cut(field, "-")

	from, err := parseIndex(head, count)
	if err != nil {
		return 0, 0, err
	}

	if !isRange {
		return from, from, nil
	}

	upto, err := parseIndex(tail, count)
	if err != nil {
		return 0, 0, err
	}

	if upto < from {
		return 0, 0, fmt.Errorf("range %s runs backwards", field)
	}

	return from, upto, nil
}

func parseIndex(s string, count int) (int, error) {
	index, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", s)
	}

	if index < 1 || index > count {
		return 0, fmt.Errorf("%d is out of range 1-%d", index, count)
	}

	return index, nil
}
