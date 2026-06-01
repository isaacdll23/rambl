// Package doctor is the engine for the `rambl doctor` preflight-check command.
//
// It defines a small, stable contract: a Check is a named diagnostic, running
// it produces a Result with a Status severity, and helpers render the results
// for humans and decide the process exit code.
package doctor

import "strings"

// Status is the outcome severity of a single check.
type Status int

const (
	OK Status = iota
	Warn
	Fail
)

// String returns a lowercase label: "ok", "warn", or "fail".
func (s Status) String() string {
	switch s {
	case OK:
		return "ok"
	case Warn:
		return "warn"
	case Fail:
		return "fail"
	default:
		return "unknown"
	}
}

// Result is the outcome of running one Check.
type Result struct {
	Name   string // human label for the check
	Status Status
	Detail string // short explanation (e.g. resolved path, or why it failed)
}

// Check is a single named diagnostic.
type Check struct {
	Name string
	Run  func() Result
}

// Run executes every check in order and returns their results in the same
// order. If a check's returned Result.Name is empty, Run fills it in from
// Check.Name so callers always get a labeled result.
func Run(checks []Check) []Result {
	results := make([]Result, 0, len(checks))
	for _, c := range checks {
		r := c.Run()
		if r.Name == "" {
			r.Name = c.Name
		}
		results = append(results, r)
	}
	return results
}

// labelWidth is the column width of the bracketed status label, sized to the
// widest label ("[warn]"/"[fail]") so detail columns line up across rows.
const labelWidth = len("[warn]")

// RenderText renders results as deterministic, human-readable lines, one per
// result, each ending in a newline. Each line is formatted as:
//
//	"[ok]   <Name> — <Detail>\n"
//
// using the Status.String() label inside brackets, padded so columns align.
// If Detail is empty, the " — <Detail>" suffix is omitted.
func RenderText(results []Result) string {
	var b strings.Builder
	for _, r := range results {
		// Pad the bracketed label to a fixed width for column alignment.
		label := "[" + r.Status.String() + "]"
		b.WriteString(label)
		for i := len(label); i < labelWidth; i++ {
			b.WriteByte(' ')
		}
		// A single space separates the label column from the name.
		b.WriteByte(' ')
		b.WriteString(r.Name)
		if r.Detail != "" {
			b.WriteString(" — ")
			b.WriteString(r.Detail)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// HasFailure reports whether any result has Status Fail (used by the CLI to
// choose a non-zero exit code). Warn does NOT count as a failure.
func HasFailure(results []Result) bool {
	for _, r := range results {
		if r.Status == Fail {
			return true
		}
	}
	return false
}
