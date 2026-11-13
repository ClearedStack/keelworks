// Package caller decides whether a call to Keelworks comes from Plainforge.
//
// Plainforge presents a bearer token of its ServiceAccount. Keelworks hands
// the token to the cluster in a TokenReview and admits the call only when the
// cluster says the token is valid, was issued for Keelworks, and belongs to
// Plainforge. The decision is pure given the cluster's answer.
package caller

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Names from the registry: Plainforge runs in the clearedstack namespace as
// the ServiceAccount plainforge, and its token must be issued for keelworks.
const (
	// Audience is the audience the token must have been issued for.
	Audience = "keelworks"
	// Plainforge is the username the cluster reports for Plainforge's
	// ServiceAccount.
	Plainforge = "system:serviceaccount:clearedstack:plainforge"
)

// MaxToken is the longest bearer token Keelworks hands to the cluster, in
// bytes.
const MaxToken = 4096

// Identity is the cluster's answer about one token.
type Identity struct {
	Authenticated bool
	Username      string
	Audiences     []string // the audiences the cluster confirmed
}

// Reviewer asks the cluster about a token. An error means the cluster gave
// no usable answer. Keelworks never shows a Reviewer's error text to anyone.
type Reviewer interface {
	Review(ctx context.Context, token, audience string) (Identity, error)
}

// Verdict is the decision about one call.
type Verdict int

const (
	// Allowed: the call comes from Plainforge.
	Allowed Verdict = iota + 1
	// NoToken: the call carries no well-formed bearer token. The cluster
	// was not asked.
	NoToken
	// BadToken: the cluster does not vouch for the token, or not for this
	// audience.
	BadToken
	// NotPlainforge: the token is valid and belongs to someone else.
	NotPlainforge
	// TimedOut: the cluster took too long to answer.
	TimedOut
	// Unavailable: the cluster gave no usable answer.
	Unavailable
)

func (v Verdict) String() string {
	switch v {
	case Allowed:
		return "allowed"
	case NoToken:
		return "no-token"
	case BadToken:
		return "bad-token"
	case NotPlainforge:
		return "not-plainforge"
	case TimedOut:
		return "timed-out"
	case Unavailable:
		return "unavailable"
	}
	return "unknown"
}

// Check decides about a call from the values of its Authorization header.
// The cluster is asked at most once, and waited on for timeout at most.
func Check(ctx context.Context, r Reviewer, timeout time.Duration, authorization []string) Verdict {
	token, ok := bearer(authorization)
	if !ok {
		return NoToken
	}
	reviewCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	id, err := r.Review(reviewCtx, token, Audience)
	if err != nil {
		if ctx.Err() == nil && errors.Is(reviewCtx.Err(), context.DeadlineExceeded) {
			return TimedOut
		}
		return Unavailable
	}
	switch {
	case !id.Authenticated:
		return BadToken
	case !contains(id.Audiences, Audience):
		// A cluster that does not echo the audience did not check it.
		return BadToken
	case id.Username != Plainforge:
		return NotPlainforge
	}
	return Allowed
}

// bearer returns the token of exactly one "Bearer <token>" header value.
func bearer(authorization []string) (string, bool) {
	if len(authorization) != 1 {
		return "", false
	}
	token, ok := strings.CutPrefix(authorization[0], "Bearer ")
	if !ok || len(token) > MaxToken {
		return "", false
	}
	// One or more of A-Z a-z 0-9 - . _ ~ + / and then any number of =.
	body := strings.TrimRight(token, "=")
	if body == "" {
		return "", false
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '-' || c == '.' || c == '_' || c == '~' || c == '+' || c == '/') {
			return "", false
		}
	}
	return token, true
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
