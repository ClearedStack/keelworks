package standin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/ClearedStack/keelworks/internal/plainforge"
	"github.com/ClearedStack/keelworks/internal/registry"
)

// initialDigest is the starting made-up digest in stand-in app.yaml manifests.
var initialDigest = "sha256:" + strings.Repeat("01", 32)

// PinnedCommit is one [skip-ci] manifest commit written to Plainforge.
type PinnedCommit struct {
	Repository string `json:"repository"`
	App        string `json:"app"`
	Digest     string `json:"digest"`
	Message    string `json:"message"`
}

// ScriptedManifest overrides the initial app.yaml or pin behaviour of a
// repository in the stand-in Plainforge store.
type ScriptedManifest struct {
	Repository string `json:"repository"`
	AppYAML    string `json:"appYAML"`
	Pin        string `json:"pin"` // "", "stall" or "unavailable"
}

// ForgeStore is the in-memory Plainforge repository stand-in implementing
// plainforge.Pinner using the real UpdateAppYAML and CommitMessage rules.
type ForgeStore struct {
	mu        sync.Mutex
	manifests map[string][]byte
	pinModes  map[string]string
	commits   []PinnedCommit
}

// NewForgeStore returns a ForgeStore seeded with an app.yaml for every target
// in registry.All(), overridden by any entries in script.Manifests.
func NewForgeStore(script Script) *ForgeStore {
	s := &ForgeStore{
		manifests: map[string][]byte{},
		pinModes:  map[string]string{},
	}
	for _, t := range registry.All() {
		ns := "cs-workers"
		switch t.Slug {
		case "conn", "muster", "plainforge", "lazarette", "keelworks", "shipkeeper":
			ns = "clearedstack"
		}
		yaml := "name: " + t.Slug + "\nnamespace: " + ns + "\nimage: " + t.Image + "\ndigest: " + initialDigest + "\nreplicas: 1\nport: 8080\nmemory: 32Mi\n"
		s.manifests[t.Repository] = []byte(yaml)
	}
	for _, m := range script.Manifests {
		if m.AppYAML != "" {
			if m.AppYAML == "missing" {
				delete(s.manifests, m.Repository)
			} else {
				s.manifests[m.Repository] = []byte(m.AppYAML)
			}
		}
		if m.Pin != "" {
			s.pinModes[m.Repository] = m.Pin
		}
	}
	return s
}

// Pin updates app.yaml for repository with digest and records a [skip-ci] commit.
func (s *ForgeStore) Pin(ctx context.Context, repository, appSlug, digest string) plainforge.Status {
	s.mu.Lock()
	mode := s.pinModes[repository]
	s.mu.Unlock()

	switch mode {
	case "stall":
		<-ctx.Done()
		return plainforge.TimedOut
	case "unavailable":
		return plainforge.Unavailable
	}
	if ctx.Err() != nil {
		return plainforge.Unavailable
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.manifests[repository]
	if !ok {
		return plainforge.BadManifest
	}
	updated, changed, err := plainforge.UpdateAppYAML(cur, digest)
	if err != nil {
		return plainforge.BadManifest
	}
	if changed {
		s.manifests[repository] = updated
		s.commits = append(s.commits, PinnedCommit{
			Repository: repository,
			App:        appSlug,
			Digest:     digest,
			Message:    strings.TrimSuffix(plainforge.CommitMessage(appSlug, digest), "\n"),
		})
	}
	return plainforge.Pinned
}

// Commits returns a copy of all [skip-ci] commits recorded so far.
func (s *ForgeStore) Commits() []PinnedCommit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]PinnedCommit(nil), s.commits...)
}

// Manifest returns the current app.yaml bytes for repository.
func (s *ForgeStore) Manifest(repository string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.manifests[repository]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), b...), true
}

// Plainforge delivers push notifications in the format of contract/v1/push.md
// with the bearer token of Plainforge's ServiceAccount.
type Plainforge struct {
	Keelworks http.Handler
	Token     string
}

// Notification builds the body Plainforge sends for a push that moved ref
// from the commit before to the commit after.
func Notification(repository, ref, before, after string) []byte {
	body, _ := json.Marshal(struct {
		Repository string `json:"repository"`
		Ref        string `json:"ref"`
		Before     string `json:"before"`
		After      string `json:"after"`
	}{repository, ref, before, after})
	return body
}

// Push delivers a notification and returns Keelworks' status and answer.
func (p Plainforge) Push(repository, ref, before, after string) (int, []byte) {
	return p.Deliver(Notification(repository, ref, before, after))
}

// Deliver posts a raw body to the webhook path with Plainforge's token.
func (p Plainforge) Deliver(body []byte) (int, []byte) {
	status, _, answer := Call(p.Keelworks, []string{"Bearer " + p.Token}, body)
	return status, answer
}

// Call posts a raw body to the webhook path, in memory, with exactly the
// given Authorization header values.
func Call(keelworks http.Handler, authorization []string, body []byte) (int, http.Header, []byte) {
	req, err := http.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	if err != nil {
		panic("standin: fixed request cannot be built") // unreachable
	}
	req.Header.Set("Content-Type", "application/json")
	for _, value := range authorization {
		req.Header.Add("Authorization", value)
	}
	rec := &recorder{header: http.Header{}, status: http.StatusOK}
	keelworks.ServeHTTP(rec, req)
	return rec.status, rec.header, rec.body.Bytes()
}

type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) WriteHeader(status int)      { r.status = status }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }
