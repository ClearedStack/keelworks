package push

import (
	"errors"
	"testing"

	"github.com/clearedstack/keelworks/internal/fault"
)

const commit = "1111111111111111111111111111111111111111"

func TestFieldOrderDoesNotChangeTheDecision(t *testing.T) {
	a, errA := Decide([]byte(`{"repository":"muster","ref":"refs/heads/main","commit":"` + commit + `","message":"m"}`))
	b, errB := Decide([]byte(` { "message":"m", "commit":"` + commit + `", "ref":"refs/heads/main", "repository":"muster" } `))
	if errA != nil || errB != nil || a != b {
		t.Fatalf("a=%+v (%v), b=%+v (%v)", a, errA, b, errB)
	}
}

func TestRunIDIsStableAndSeparatesRefs(t *testing.T) {
	main := Event{Repository: "muster", Ref: "refs/heads/main", Commit: commit}
	tag := Event{Repository: "muster", Ref: "refs/tags/v1", Commit: commit}
	if got := main.RunID(); got != "muster-674f2946f186" {
		t.Errorf("RunID = %q", got)
	}
	if main.RunID() == tag.RunID() {
		t.Error("a branch push and a tag push of one commit share a run")
	}
}

func TestBrokenDataIsReportedBeforeUnfamiliarData(t *testing.T) {
	// One more field than the rules cover, and a broken commit.
	_, err := Decide([]byte(`{"repository":"muster","ref":"refs/heads/main","commit":"zz","message":"m","extra":1}`))
	var f *fault.Fault
	if !errors.As(err, &f) || f.Code != fault.Parse {
		t.Fatalf("err = %v, want E_PARSE", err)
	}
}

func TestLongestAllowedNamesAreAccepted(t *testing.T) {
	repo := "a2345678901234567890123456789012" // 32 characters
	d, err := Decide([]byte(`{"repository":"` + repo + `","ref":"refs/tags/v1.0.0-rc.1","commit":"` + commit + `","message":""}`))
	if err != nil || d.Ignore != "" {
		t.Fatalf("d=%+v err=%v", d, err)
	}
	if n := len("keelworks-test-" + d.Event.RunID()); n > 63 {
		t.Fatalf("job name is %d characters, over the 63 a DNS label allows", n)
	}
	for _, bad := range []string{repo + "x", "ends-", "9lives", ""} {
		if _, err := Decide([]byte(`{"repository":"` + bad + `","ref":"refs/heads/main","commit":"` + commit + `","message":""}`)); err == nil {
			t.Errorf("repository %q was accepted", bad)
		}
	}
	for _, bad := range []string{"refs/heads/", "refs//main", "refs/heads/a b", "refs/heads", "heads/main", "refs/heads/main/"} {
		if _, err := Decide([]byte(`{"repository":"muster","ref":"` + bad + `","commit":"` + commit + `","message":""}`)); err == nil {
			t.Errorf("ref %q was accepted", bad)
		}
	}
}
