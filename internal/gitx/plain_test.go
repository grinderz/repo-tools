package gitx

import (
	"strings"
	"testing"
)

// The stripper removes whole escape sequences, including one split across two
// writes — which is how they arrive from a streaming child.
func TestAnsiStripper(t *testing.T) {
	var out strings.Builder

	s := &ansiStripper{w: &out}

	if _, err := s.Write([]byte("\x1b[0mdirenv: \x1b[1;32mloading\x1b[0m .envrc\n")); err != nil {
		t.Fatal(err)
	}

	if got := out.String(); got != "direnv: loading .envrc\n" {
		t.Errorf("got %q", got)
	}

	out.Reset()

	// The sequence breaks between the parameter bytes and the final byte.
	for _, chunk := range []string{"a\x1b[1;", "32mb", "\x1b", "[0mc"} {
		if _, err := s.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}

	if got := out.String(); got != "abc" {
		t.Errorf("split sequence survived: %q", got)
	}
}
