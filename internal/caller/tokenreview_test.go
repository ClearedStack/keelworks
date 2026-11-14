package caller

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	fixtures = "../../fixtures/tokenreview"
	// own is the made-up token Keelworks itself presents to the cluster.
	own = "made-up-token-of-keelworks"
)

func read(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func ownToken() (string, error) { return own, nil }

// apiServer is a made-up API server that gives one fixed answer. It fails
// the test when the request is not exactly the saved one.
func apiServer(t *testing.T, status int, answer []byte) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	want := read(t, filepath.Join(fixtures, "request.json"))
	var calls atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.Method != http.MethodPost:
			t.Errorf("method %s", r.Method)
		case r.URL.Path != "/apis/authentication.k8s.io/v1/tokenreviews" || r.URL.RawQuery != "":
			t.Errorf("path %s", r.URL.RequestURI())
		case r.Header.Get("Authorization") != "Bearer "+own:
			t.Errorf("Keelworks did not present its own token")
		case r.Header.Get("Content-Type") != "application/json":
			t.Errorf("content type %q", r.Header.Get("Content-Type"))
		case !bytes.Equal(body, want):
			t.Errorf("request differs from the saved one\n--- got ---\n%s--- want ---\n%s", body, want)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(answer)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// TestSavedTokenReviews replays every directory under
// fixtures/tokenreview/cases through the real client and the decision.
func TestSavedTokenReviews(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(fixtures, "cases"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no saved examples found")
	}
	for _, entry := range entries {
		dir := filepath.Join(fixtures, "cases", entry.Name())
		t.Run(entry.Name(), func(t *testing.T) {
			status, err := strconv.Atoi(strings.TrimSpace(string(read(t, filepath.Join(dir, "status.txt")))))
			if err != nil {
				t.Fatal(err)
			}
			want := strings.TrimSpace(string(read(t, filepath.Join(dir, "verdict.txt"))))
			srv, calls := apiServer(t, status, read(t, filepath.Join(dir, "answer.json")))

			reviewer := NewAPIReviewer(srv.URL, srv.Client(), ownToken)
			got := Check(context.Background(), reviewer, 10*time.Second, []string{"Bearer " + made})
			if got.String() != want {
				t.Errorf("verdict %v, want %s", got, want)
			}
			if n := calls.Load(); n != 1 {
				t.Errorf("the cluster was asked %d times, want 1", n)
			}
		})
	}
}

func TestReviewBodyIsTheSavedRequest(t *testing.T) {
	want := read(t, filepath.Join(fixtures, "request.json"))
	if got := ReviewBody(made, Audience); !bytes.Equal(got, want) {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestReviewNeverFollowsARedirect(t *testing.T) {
	yes := read(t, filepath.Join(fixtures, "cases", "plainforge", "answer.json"))
	var reached atomic.Int64
	elsewhere := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(yes)
	}))
	defer elsewhere.Close()
	for _, status := range []int{http.StatusMovedPermanently, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, elsewhere.URL+r.URL.Path, status)
		}))
		client := srv.Client()
		followed := false
		client.CheckRedirect = func(*http.Request, []*http.Request) error { followed = true; return nil }
		reviewer := NewAPIReviewer(srv.URL, client, ownToken)
		if got := Check(context.Background(), reviewer, 10*time.Second, []string{"Bearer " + made}); got != Unavailable {
			t.Errorf("status %d: verdict %v, want unavailable", status, got)
		}
		if followed {
			t.Errorf("status %d: the caller's own client was asked to follow", status)
		}
		srv.Close()
	}
	if reached.Load() != 0 {
		t.Fatal("the tokens were carried to another server")
	}
}

func TestReviewTimesOutWhenTheServerStalls(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	reviewer := NewAPIReviewer(srv.URL, srv.Client(), ownToken)
	if got := Check(context.Background(), reviewer, 50*time.Millisecond, []string{"Bearer " + made}); got != TimedOut {
		t.Fatalf("verdict %v, want timed-out", got)
	}
}

func TestReviewRefusesAnAnswerOverTheLimit(t *testing.T) {
	yes := read(t, filepath.Join(fixtures, "cases", "plainforge", "answer.json"))
	padded := append(append([]byte(nil), yes...), bytes.Repeat([]byte(" "), maxAnswer)...)
	srv, _ := apiServer(t, http.StatusCreated, padded)
	reviewer := NewAPIReviewer(srv.URL, srv.Client(), ownToken)
	if _, err := reviewer.Review(context.Background(), made, Audience); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestReviewWithoutItsOwnTokenAsksNobody(t *testing.T) {
	srv, calls := apiServer(t, http.StatusCreated, nil)
	reviewer := NewAPIReviewer(srv.URL, srv.Client(), FileCredential(filepath.Join(t.TempDir(), "absent")))
	if _, err := reviewer.Review(context.Background(), made, Audience); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if calls.Load() != 0 {
		t.Fatal("the cluster was asked without a token of Keelworks' own")
	}
}

// TestReviewErrorsNameNothing checks that whatever goes wrong, the error is
// the one fixed sentence: no address, no path, no token.
func TestReviewErrorsNameNothing(t *testing.T) {
	gone := httptest.NewTLSServer(http.NotFoundHandler())
	client, url := gone.Client(), gone.URL
	gone.Close()
	untrusted := httptest.NewTLSServer(http.NotFoundHandler())
	defer untrusted.Close()

	path := filepath.Join(t.TempDir(), "absent")
	reviewers := map[string]*APIReviewer{
		"server is gone":       NewAPIReviewer(url, client, ownToken),
		"server is untrusted":  NewAPIReviewer(untrusted.URL, &http.Client{}, ownToken),
		"address is malformed": NewAPIReviewer("https://made up", &http.Client{}, ownToken),
		"own token is missing": NewAPIReviewer(url, client, FileCredential(path)),
	}
	for name, reviewer := range reviewers {
		_, err := reviewer.Review(context.Background(), made, Audience)
		if err != ErrUnavailable {
			t.Errorf("%s: err = %v, want exactly ErrUnavailable", name, err)
		}
	}
}

func TestFileCredentialIsReadEveryTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	credential := FileCredential(path)
	for _, step := range []struct {
		content string
		want    string
	}{
		{"made-up-first-token\n", "made-up-first-token"},
		{"  made-up-second-token  \n", "made-up-second-token"},
		{"\n", ""},
	} {
		if err := os.WriteFile(path, []byte(step.content), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := credential()
		if got != step.want || (err == nil) != (step.want != "") {
			t.Errorf("content %q: got %q, err %v", step.content, got, err)
		}
	}
}
