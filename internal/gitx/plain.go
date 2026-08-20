package gitx

import (
	"io"
	"os"
)

// Some child programs colour their output even into a pipe — direnv's
// "loading .envrc" line ignores DIRENV_LOG_FORMAT outright. When rt itself
// runs without colour, everything it streams from children is scrubbed of
// escape sequences too, so piped output stays clean end to end.

//nolint:gochecknoglobals // process-wide output mode, set once at startup
var plainOutput bool

// SetPlainOutput makes every streamed child command's output pass through an
// ANSI escape filter. The CLI sets it from the same decision that turns rt's
// own colours off.
func SetPlainOutput(plain bool) { plainOutput = plain }

func childStdout() io.Writer { return childWriter(os.Stdout) }

func childStderr() io.Writer { return childWriter(os.Stderr) }

func childWriter(w io.Writer) io.Writer {
	if !plainOutput {
		return w
	}

	return &ansiStripper{w: w}
}

// ansiStripper removes ANSI escape sequences from a byte stream. It keeps the
// tail of an unfinished sequence between writes, so a code split across two
// chunks is still removed whole.
type ansiStripper struct {
	w       io.Writer
	pending []byte // an escape sequence started but not yet terminated
}

// escape sequence bytes: ESC starts one, '[' extends it into a CSI sequence,
// and a CSI sequence runs until a byte in @..~.
const (
	escByte      = 0x1b
	csiFinalLow  = 0x40
	csiFinalHigh = 0x7e
)

func (s *ansiStripper) Write(p []byte) (int, error) {
	out := make([]byte, 0, len(p))

	for _, next := range p {
		switch {
		case len(s.pending) == 0 && next != escByte:
			out = append(out, next)
		case len(s.pending) == 0: // ESC opens a sequence
			s.pending = append(s.pending, next)
		case len(s.pending) == 1:
			if next == '[' {
				s.pending = append(s.pending, next) // CSI, runs until a final byte
			} else {
				s.pending = s.pending[:0] // a two-byte escape ends here
			}
		default:
			if next >= csiFinalLow && next <= csiFinalHigh {
				s.pending = s.pending[:0] // the final byte closes the sequence
			} else {
				s.pending = append(s.pending, next)
			}
		}
	}

	if _, err := s.w.Write(out); err != nil {
		return 0, err //nolint:wrapcheck // a writer passes its sink's error through
	}

	return len(p), nil
}
