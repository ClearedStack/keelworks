package caller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const made = "made-up-token-of-plainforge"

// fakeReviewer answers every question the same way and remembers what it
// was asked.
type fakeReviewer struct {
	id       Identity
	err      error
	stall    bool
	asked    int
	token    string
	audience string
}

func (f *fakeReviewer) Review(ctx context.Context, token, audience string) (Identity, error) {
	f.asked++
	f.token, f.audience = token, audience
	if f.stall {
		<-ctx.Done()
		return Identity{}, ctx.Err()
	}
	return f.id, f.err
}

func plainforge() Identity {
	return Identity{Authenticated: true, Username: Plainforge, Audiences: []string{Audience}}
}

func TestNamesComeFromTheRegistry(t *testing.T) {
	if Audience != "keelworks" || Plainforge != "system:serviceaccount:clearedstack:plainforge" {
		t.Fatalf("Audience = %q, Plainforge = %q", Audience, Plainforge)
	}
}

func TestCheckVerdicts(t *testing.T) {
	with := func(change func(*Identity)) Identity {
		id := plainforge()
		change(&id)
		return id
	}
	cases := []struct {
		name string
		r    *fakeReviewer
		want Verdict
	}{
		{"plainforge", &fakeReviewer{id: plainforge()}, Allowed},
		{"more audiences than asked", &fakeReviewer{id: with(func(i *Identity) { i.Audiences = []string{"made-up-other-audience", Audience} })}, Allowed},
		{"not authenticated", &fakeReviewer{id: with(func(i *Identity) { i.Authenticated = false })}, BadToken},
		{"nothing known", &fakeReviewer{}, BadToken},
		{"audience not confirmed", &fakeReviewer{id: with(func(i *Identity) { i.Audiences = nil })}, BadToken},
		{"another audience confirmed", &fakeReviewer{id: with(func(i *Identity) { i.Audiences = []string{"made-up-other-audience"} })}, BadToken},
		{"another service account", &fakeReviewer{id: with(func(i *Identity) { i.Username = "system:serviceaccount:clearedstack:muster" })}, NotPlainforge},
		{"a namesake elsewhere", &fakeReviewer{id: with(func(i *Identity) { i.Username = "system:serviceaccount:cs-workers:plainforge" })}, NotPlainforge},
		{"a user named like it", &fakeReviewer{id: with(func(i *Identity) { i.Username = "plainforge" })}, NotPlainforge},
		{"longer name", &fakeReviewer{id: with(func(i *Identity) { i.Username = Plainforge + "-2" })}, NotPlainforge},
		{"no name", &fakeReviewer{id: with(func(i *Identity) { i.Username = "" })}, NotPlainforge},
		{"no answer", &fakeReviewer{err: errors.New("made-up-host.example did not answer")}, Unavailable},
		{"no answer beside a yes", &fakeReviewer{id: plainforge(), err: ErrUnavailable}, Unavailable},
	}
	for _, c := range cases {
		got := Check(context.Background(), c.r, time.Minute, []string{"Bearer " + made})
		if got != c.want {
			t.Errorf("%s: verdict %v, want %v", c.name, got, c.want)
		}
		if c.r.asked != 1 || c.r.token != made || c.r.audience != Audience {
			t.Errorf("%s: asked %d times about %q for %q", c.name, c.r.asked, c.r.token, c.r.audience)
		}
	}
}

func TestCheckReadsExactlyOneBearerToken(t *testing.T) {
	long := strings.Repeat("a", MaxToken)
	good := [][]string{
		{"Bearer " + made},
		{"Bearer " + long},
		{"Bearer AZaz09-._~+/"},
		{"Bearer made-up=="},
	}
	for _, h := range good {
		r := &fakeReviewer{id: plainforge()}
		if got := Check(context.Background(), r, time.Minute, h); got != Allowed || r.token != strings.TrimPrefix(h[0], "Bearer ") {
			t.Errorf("%.30q: verdict %v, token %.30q", h, got, r.token)
		}
	}
	bad := [][]string{
		nil,
		{},
		{""},
		{"Bearer"},
		{"Bearer "},
		{"Bearer " + long + "a"},
		{"Bearer " + made, "Bearer " + made},
		{"bearer " + made},
		{"BEARER " + made},
		{"Token " + made},
		{"Basic bWFkZS11cDptYWRlLXVw"},
		{made},
		{" Bearer " + made},
		{"Bearer  " + made},
		{"Bearer\t" + made},
		{"Bearer " + made + " "},
		{"Bearer made up"},
		{"Bearer made,up"},
		{"Bearer made\"up"},
		{"Bearer made=up"},
		{"Bearer =="},
		{"Bearer made-üp"},
	}
	for _, h := range bad {
		r := &fakeReviewer{id: plainforge()}
		if got := Check(context.Background(), r, time.Minute, h); got != NoToken || r.asked != 0 {
			t.Errorf("%.30q: verdict %v after asking the cluster %d times, want no-token and 0", h, got, r.asked)
		}
	}
}

func TestCheckTimesOutWhenTheClusterStalls(t *testing.T) {
	start := time.Now()
	got := Check(context.Background(), &fakeReviewer{stall: true}, 20*time.Millisecond, []string{"Bearer " + made})
	if got != TimedOut {
		t.Fatalf("verdict %v, want timed-out", got)
	}
	if waited := time.Since(start); waited > 10*time.Second {
		t.Fatalf("waited %v", waited)
	}
}

func TestCheckDoesNotBlameTheClusterWhenTheCallerLeaves(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	if got := Check(ctx, &fakeReviewer{stall: true}, time.Minute, []string{"Bearer " + made}); got != Unavailable {
		t.Fatalf("verdict %v, want unavailable", got)
	}
}

func TestVerdictNames(t *testing.T) {
	want := map[Verdict]string{
		Allowed: "allowed", NoToken: "no-token", BadToken: "bad-token", NotPlainforge: "not-plainforge",
		TimedOut: "timed-out", Unavailable: "unavailable", Verdict(0): "unknown", Verdict(99): "unknown",
	}
	for v, name := range want {
		if v.String() != name {
			t.Errorf("Verdict(%d) = %q, want %q", int(v), v.String(), name)
		}
	}
}
