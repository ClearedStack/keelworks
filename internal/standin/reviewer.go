package standin

import (
	"context"
	"errors"
	"sync"

	"github.com/ClearedStack/keelworks/internal/caller"
)

// Scripted behaviour of the token check itself, for a token whose check does
// not end in an answer.
const (
	ReviewStall       = "stall"       // the cluster never answers
	ReviewUnavailable = "unavailable" // the cluster gives no usable answer
)

// ScriptedCaller is one made-up bearer token and what the cluster says about
// it. A token that is not in the script is one the cluster does not know.
type ScriptedCaller struct {
	Token     string   `json:"token"`
	Username  string   `json:"username"`
	Audiences []string `json:"audiences"` // what the token was issued for
	Review    string   `json:"review"`    // "", "stall" or "unavailable"
}

// Reviewer is an in-memory caller.Reviewer. It answers the way a cluster's
// TokenReview does: a token is vouched for only when it is known and was
// issued for the audience that is asked about.
type Reviewer struct {
	callers map[string]ScriptedCaller

	mu      sync.Mutex
	reviews int
}

// NewReviewer returns a Reviewer that knows the callers in script.
func NewReviewer(script Script) *Reviewer {
	r := &Reviewer{callers: map[string]ScriptedCaller{}}
	for _, c := range script.Callers {
		r.callers[c.Token] = c
	}
	return r
}

// Review plays back what the script says about token.
func (r *Reviewer) Review(ctx context.Context, token, audience string) (caller.Identity, error) {
	r.mu.Lock()
	r.reviews++
	r.mu.Unlock()

	c, known := r.callers[token]
	if !known {
		return caller.Identity{}, nil
	}
	switch c.Review {
	case ReviewStall:
		<-ctx.Done()
		return caller.Identity{}, ctx.Err()
	case ReviewUnavailable:
		return caller.Identity{}, errors.New("stand-in cluster gave no usable answer")
	}
	for _, a := range c.Audiences {
		if a == audience {
			return caller.Identity{Authenticated: true, Username: c.Username, Audiences: []string{audience}}, nil
		}
	}
	return caller.Identity{}, nil
}

// Reviews returns how many times the cluster was asked about a token.
func (r *Reviewer) Reviews() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reviews
}
