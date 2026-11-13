// Package fault holds the three public error codes of Keelworks.
//
// A Fault carries a code and a detail. The detail is always one of a small
// set of fixed phrases written in this repository. It never repeats text
// that came from outside, so it cannot name a machine, an address or a
// credential.
package fault

// Code is one of the three public error codes.
type Code string

const (
	// Timeout means something outside Keelworks took too long.
	Timeout Code = "E_TIMEOUT"
	// Parse means the data was broken.
	Parse Code = "E_PARSE"
	// NeedsSubtool means the data was valid and unfamiliar: no rule covers it.
	NeedsSubtool Code = "E_NEEDS_SUBTOOL"
)

// Fault is an error with a public code and a fixed detail phrase.
type Fault struct {
	Code   Code
	Detail string
}

func (f *Fault) Error() string { return string(f.Code) + ": " + f.Detail }

// New returns a Fault. The detail must be a fixed phrase, never outside text.
func New(code Code, detail string) *Fault { return &Fault{Code: code, Detail: detail} }
