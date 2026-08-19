package run

import (
	"os"
	"strings"
)

// The output speaks one visual language, the one a package manager taught
// everyone's eyes (yay/pacman):
//
//	::  bold cyan — a phase: a plan header, a flow step, a question
//	==> bold cyan — one project inside the batch
//	WARNING: / ERROR: — yellow and red, with the colon, always at the start
//	dim — hints, side notes and defaults; plain indented text for details
//
// One meaning per shape, the same shape in every command.

// ANSI codes used for the little colour this tool needs.
const (
	codeBold   = "1"
	codeDim    = "2"
	codeRed    = "31"
	codeGreen  = "32"
	codeYellow = "33"
	codeCyan   = "36"
)

// colorEnabled is process-wide: it is decided once from the flags, the config
// and the terminal, and every printer in the batch obeys the same answer.
var colorEnabled bool //nolint:gochecknoglobals // one terminal per process

// SetColor fixes whether output is coloured.
func SetColor(on bool) { colorEnabled = on }

// ColorEnabled reports the decision, for callers that pass it on to git.
func ColorEnabled() bool { return colorEnabled }

// AutoColor is the default: colour when a terminal is attached and nobody
// opted out through the usual environment variables.
func AutoColor() bool {
	if os.Getenv("NO_COLOR") != "" || strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return false
	}

	fi, err := os.Stdout.Stat()

	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func paint(code, text string) string {
	if !colorEnabled || text == "" {
		return text
	}

	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

// Warn is the warning label: always yellow, always with the colon.
func Warn() string { return Yellow("WARNING:") }

// Fail is the error label, the same rule in red.
func Fail() string { return Red("ERROR:") }

// Marker is the phase mark every header and question starts with.
func Marker() string { return Bold(Cyan("::")) }

func Bold(text string) string   { return paint(codeBold, text) }
func Dim(text string) string    { return paint(codeDim, text) }
func Red(text string) string    { return paint(codeRed, text) }
func Green(text string) string  { return paint(codeGreen, text) }
func Yellow(text string) string { return paint(codeYellow, text) }
func Cyan(text string) string   { return paint(codeCyan, text) }
