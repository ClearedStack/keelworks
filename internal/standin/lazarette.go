package standin

import (
	"context"
	"sync"

	"github.com/ClearedStack/keelworks/internal/lazarette"
	"github.com/ClearedStack/keelworks/internal/registry"
)

// RetainedDigests is how many signed digests Lazarette retains per image.
const RetainedDigests = 5

// Lazarette is an in-memory lazarette.Verifier following SPEC.md: only
// digests explicitly recorded as signed pass verification, and at most
// RetainedDigests are kept per image.
type Lazarette struct {
	mu     sync.Mutex
	signed map[string][]string // image -> signed digests, oldest first
}

// NewLazarette returns an empty in-memory Lazarette stand-in.
func NewLazarette() *Lazarette {
	return &Lazarette{signed: map[string][]string{}}
}

// RecordSigned records that image@digest was pushed and signed with cosign.
func (l *Lazarette) RecordSigned(image, digest string) {
	if image == "" || !registry.ValidDigest(digest) {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, existing := range l.signed[image] {
		if existing == digest {
			return
		}
	}
	list := append(l.signed[image], digest)
	if len(list) > RetainedDigests {
		list = list[len(list)-RetainedDigests:]
	}
	l.signed[image] = list
}

// Verify reports Verified when digest is among the retained signed digests for image.
func (l *Lazarette) Verify(ctx context.Context, image, digest string) lazarette.Status {
	if ctx.Err() != nil {
		return lazarette.Unavailable
	}
	if image == "" || !registry.ValidDigest(digest) {
		return lazarette.Unsigned
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, d := range l.signed[image] {
		if d == digest {
			return lazarette.Verified
		}
	}
	return lazarette.Unsigned
}
