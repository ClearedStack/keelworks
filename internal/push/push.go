// Package push reads a push notification from Plainforge and decides what
// Keelworks does with it. It is pure: the same bytes always give the same
// decision. The format is written down in contract/v1/push.md.
package push

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"

	"github.com/ClearedStack/keelworks/internal/fault"
	"github.com/ClearedStack/keelworks/internal/registry"
)

// MaxBody is the largest notification body Keelworks reads, in bytes.
const MaxBody = 64 << 10

// ReasonBranchNotMain is why a valid push to a branch other than main is
// ignored. It is the only reason Keelworks ignores a notification: whether a
// push is announced at all, the [skip-ci] rule included, is decided by
// Plainforge before it sends.
const ReasonBranchNotMain = "branch-not-main"

// Event is a valid push notification.
type Event struct {
	Repository string
	Ref        string
	Before     string // the commit the ref pointed at before the push
	After      string // the commit the ref points at now; the one that is built
}

// Decision says what to do with a valid notification. Ignore is empty when
// the push must be built, and holds the reason otherwise.
type Decision struct {
	Event  Event
	Target registry.Target
	Ignore string
}

// RunID names the run for this event. It uses the component's DNS-safe slug
// as the prefix and depends only on the repository, the ref and the commit
// that is built, so a notification delivered twice names one run.
func (e Event) RunID() string {
	prefix := e.Repository
	if t, ok := registry.ByRepository(e.Repository); ok {
		prefix = t.Slug
	}
	sum := sha256.Sum256([]byte(e.Repository + "\x00" + e.Ref + "\x00" + e.After))
	return prefix + "-" + hex.EncodeToString(sum[:6])
}

// fields are the notification's field names, in the order they are checked.
var fields = []string{"after", "before", "ref", "repository"}

// Decide parses a notification body and applies the intake rules in a fixed
// order. Every refusal is a *fault.Fault.
func Decide(body []byte) (Decision, error) {
	if len(body) > MaxBody {
		return Decision{}, fault.New(fault.Parse, "body is too large")
	}
	raw, err := object(body)
	if err != nil {
		return Decision{}, err
	}

	// Broken data first: a required field that is missing or has the wrong
	// shape. Fields are checked in alphabetical order so the answer does not
	// depend on the order they were sent in.
	var ev Event
	for _, name := range fields {
		value, ok := raw[name]
		if !ok {
			return Decision{}, fault.New(fault.Parse, "field "+name+" is missing")
		}
		var s string
		if json.Unmarshal(value, &s) != nil || bytes.TrimSpace(value)[0] != '"' {
			return Decision{}, fault.New(fault.Parse, "field "+name+" is not a string")
		}
		switch name {
		case "after":
			ev.After = s
		case "before":
			ev.Before = s
		case "ref":
			ev.Ref = s
		case "repository":
			ev.Repository = s
		}
	}
	switch {
	case !validCommit(ev.After):
		return Decision{}, fault.New(fault.Parse, "field after is malformed")
	case !validCommit(ev.Before):
		return Decision{}, fault.New(fault.Parse, "field before is malformed")
	case !validRef(ev.Ref):
		return Decision{}, fault.New(fault.Parse, "field ref is malformed")
	case !validRepository(ev.Repository):
		return Decision{}, fault.New(fault.Parse, "field repository is malformed")
	}

	// Valid and unfamiliar next.
	if len(raw) > len(fields) {
		return Decision{}, fault.New(fault.NeedsSubtool, "notification has a field no rule covers")
	}
	if strings.Trim(ev.After, "0") == "" {
		return Decision{}, fault.New(fault.NeedsSubtool, "no rule for a deleted ref")
	}
	branch, isBranch := strings.CutPrefix(ev.Ref, "refs/heads/")
	_, isTag := strings.CutPrefix(ev.Ref, "refs/tags/")
	if !isBranch && !isTag {
		return Decision{}, fault.New(fault.NeedsSubtool, "no rule for this kind of ref")
	}
	target, ok := registry.ByRepository(ev.Repository)
	if !ok {
		return Decision{}, fault.New(fault.NeedsSubtool, "no rule for this repository")
	}

	// Valid and familiar: build it or ignore it.
	if isBranch && branch != "main" {
		return Decision{Event: ev, Target: target, Ignore: ReasonBranchNotMain}, nil
	}
	return Decision{Event: ev, Target: target}, nil
}

// object decodes body as exactly one JSON object.
func object(body []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	var value json.RawMessage
	if err := dec.Decode(&value); err != nil {
		return nil, fault.New(fault.Parse, "body is not valid JSON")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fault.New(fault.Parse, "body is not valid JSON")
	}
	if len(value) == 0 || value[0] != '{' {
		return nil, fault.New(fault.Parse, "body is not a JSON object")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(value, &raw); err != nil {
		return nil, fault.New(fault.Parse, "body is not valid JSON")
	}
	return raw, nil
}

func isLowerAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

// validRepository accepts a repository name: starts with a lowercase letter,
// ends with a lowercase letter or digit, up to 32 characters, made of
// lowercase letters, digits, hyphens and single dots between letters or digits.
func validRepository(s string) bool {
	if len(s) == 0 || len(s) > 32 || s[0] < 'a' || s[0] > 'z' || !isLowerAlnum(s[len(s)-1]) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case isLowerAlnum(c) || c == '-':
		case c == '.':
			if !isLowerAlnum(s[i-1]) || !isLowerAlnum(s[i+1]) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// validCommit accepts exactly 40 lowercase hexadecimal characters.
func validCommit(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// validRef accepts "refs/<kind>/<name>" in printable ASCII without spaces,
// 200 characters at most, with a non-empty kind and name.
func validRef(s string) bool {
	if len(s) > 200 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] <= ' ' || s[i] >= 0x7f {
			return false
		}
	}
	parts := strings.SplitN(s, "/", 3)
	return len(parts) == 3 && parts[0] == "refs" && parts[1] != "" && parts[2] != "" &&
		!strings.HasSuffix(s, "/") && !strings.Contains(s, "//")
}
