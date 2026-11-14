package standin

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/ClearedStack/keelworks/internal/caller"
)

func callers() Script {
	return Script{Callers: []ScriptedCaller{
		{Token: "made-up-token-of-plainforge", Username: caller.Plainforge, Audiences: []string{caller.Audience}},
		{Token: "made-up-token-for-another-audience", Username: caller.Plainforge, Audiences: []string{"made-up-other-audience"}},
		{Token: "made-up-token-the-check-stalls-on", Review: ReviewStall},
		{Token: "made-up-token-the-check-fails-on", Review: ReviewUnavailable},
	}}
}

func TestReviewerVouchesOnlyForAKnownTokenAndItsAudience(t *testing.T) {
	r := NewReviewer(callers())
	ctx := context.Background()

	id, err := r.Review(ctx, "made-up-token-of-plainforge", caller.Audience)
	want := caller.Identity{Authenticated: true, Username: caller.Plainforge, Audiences: []string{caller.Audience}}
	if err != nil || !reflect.DeepEqual(id, want) {
		t.Fatalf("known token: %+v, %v", id, err)
	}
	for _, c := range []struct{ token, audience string }{
		{"made-up-token-nobody-holds", caller.Audience},
		{"made-up-token-for-another-audience", caller.Audience},
		{"made-up-token-of-plainforge", "made-up-other-audience"},
		{"", caller.Audience},
	} {
		id, err := r.Review(ctx, c.token, c.audience)
		if err != nil || id.Authenticated || id.Username != "" || len(id.Audiences) != 0 {
			t.Errorf("token %q for %q: %+v, %v", c.token, c.audience, id, err)
		}
	}
	if _, err := r.Review(ctx, "made-up-token-the-check-fails-on", caller.Audience); err == nil {
		t.Error("the failing check gave an answer")
	}
	if n := r.Reviews(); n != 6 {
		t.Errorf("%d reviews counted, want 6", n)
	}
}

func TestReviewerStallsUntilTheCallerGivesUp(t *testing.T) {
	r := NewReviewer(callers())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := r.Review(ctx, "made-up-token-the-check-stalls-on", caller.Audience); err == nil {
		t.Fatal("the stalling check gave an answer")
	}
}

func TestScriptWithABrokenCallerIsRefused(t *testing.T) {
	good := `{"callers":[{"token":"made-up-a","username":"made-up-name","audiences":["keelworks"]},{"token":"made-up-b","review":"stall"}]}`
	if s, err := ParseScript([]byte(good)); err != nil || len(s.Callers) != 2 {
		t.Fatalf("a good script was refused: %v", err)
	}
	for name, bad := range map[string]string{
		"no token":       `{"callers":[{"username":"made-up-name"}]}`,
		"repeated token": `{"callers":[{"token":"made-up-a"},{"token":"made-up-a"}]}`,
		"unknown review": `{"callers":[{"token":"made-up-a","review":"maybe"}]}`,
	} {
		if _, err := ParseScript([]byte(bad)); err == nil {
			t.Errorf("%s: the script was accepted", name)
		}
	}
}
