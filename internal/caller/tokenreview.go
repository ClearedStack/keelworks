package caller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
)

// ErrUnavailable is the only error an APIReviewer returns. It carries no
// text from outside, so it cannot name a machine, an address or a credential.
var ErrUnavailable = errors.New("the cluster gave no usable answer about the token")

// reviewPath is where the Kubernetes API takes a TokenReview.
const reviewPath = "/apis/authentication.k8s.io/v1/tokenreviews"

const (
	reviewAPIVersion = "authentication.k8s.io/v1"
	reviewKind       = "TokenReview"
)

// maxAnswer is the largest TokenReview answer Keelworks reads, in bytes.
const maxAnswer = 64 << 10

// APIReviewer is the Reviewer that asks a Kubernetes API server. Keelworks'
// own ServiceAccount must be allowed to create tokenreviews.
type APIReviewer struct {
	base       string
	client     *http.Client
	credential func() (string, error)
}

// NewAPIReviewer returns a Reviewer that posts TokenReviews to the API server
// at base, for example "https://host:port". client must trust that server.
// credential returns Keelworks' own token and is called for every review,
// because the cluster replaces that token from time to time.
func NewAPIReviewer(base string, client *http.Client, credential func() (string, error)) *APIReviewer {
	c := *client
	// Never follow a redirect: it would carry both tokens somewhere else.
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &APIReviewer{base: strings.TrimRight(base, "/"), client: &c, credential: credential}
}

// FileCredential returns a credential function that reads a token from a
// file each time it is called.
func FileCredential(path string) func() (string, error) {
	return func() (string, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", ErrUnavailable
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			return "", ErrUnavailable
		}
		return token, nil
	}
}

type reviewRequest struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Spec       reviewSpec `json:"spec"`
}

type reviewSpec struct {
	Token     string   `json:"token"`
	Audiences []string `json:"audiences"`
}

type reviewAnswer struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Status     struct {
		Authenticated bool `json:"authenticated"`
		User          struct {
			Username string `json:"username"`
		} `json:"user"`
		Audiences []string `json:"audiences"`
		Error     string   `json:"error"`
	} `json:"status"`
}

// ReviewBody returns the exact TokenReview Keelworks sends for a token, as
// one line of JSON.
func ReviewBody(token, audience string) []byte {
	body, err := json.Marshal(reviewRequest{
		APIVersion: reviewAPIVersion,
		Kind:       reviewKind,
		Spec:       reviewSpec{Token: token, Audiences: []string{audience}},
	})
	if err != nil {
		panic("caller: review cannot be encoded") // unreachable: plain structs
	}
	return append(body, '\n')
}

// Review asks the API server about token.
func (a *APIReviewer) Review(ctx context.Context, token, audience string) (Identity, error) {
	own, err := a.credential()
	if err != nil {
		return Identity{}, ErrUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+reviewPath, bytes.NewReader(ReviewBody(token, audience)))
	if err != nil {
		return Identity{}, ErrUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+own)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return Identity{}, ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return Identity{}, ErrUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil || len(data) > maxAnswer {
		return Identity{}, ErrUnavailable
	}
	var answer reviewAnswer
	if json.Unmarshal(data, &answer) != nil || answer.APIVersion != reviewAPIVersion || answer.Kind != reviewKind {
		return Identity{}, ErrUnavailable
	}
	status := answer.Status
	return Identity{
		// An answer that reports an error is not a yes, whatever else it says.
		Authenticated: status.Authenticated && status.Error == "",
		Username:      status.User.Username,
		Audiences:     status.Audiences,
	}, nil
}
