package push

import (
	"errors"
	"strings"
	"testing"

	"github.com/ClearedStack/keelworks/internal/fault"
	"github.com/ClearedStack/keelworks/internal/registry"
)

const (
	before = "abababababababababababababababababababab"
	after  = "1111111111111111111111111111111111111111"
	zero   = "0000000000000000000000000000000000000000"
)

func body(repository, ref, before, after string) []byte {
	return []byte(`{"repository":"` + repository + `","ref":"` + ref + `","before":"` + before + `","after":"` + after + `"}`)
}

func code(err error) fault.Code {
	var f *fault.Fault
	if errors.As(err, &f) {
		return f.Code
	}
	return ""
}

func TestFieldOrderDoesNotChangeTheDecision(t *testing.T) {
	a, errA := Decide(body("muster", "refs/heads/main", before, after))
	b, errB := Decide([]byte(` { "after":"` + after + `", "ref":"refs/heads/main", "before":"` + before + `", "repository":"muster" } `))
	if errA != nil || errB != nil || a != b {
		t.Fatalf("a=%+v (%v), b=%+v (%v)", a, errA, b, errB)
	}
	want := Event{Repository: "muster", Ref: "refs/heads/main", Before: before, After: after}
	if a.Event != want || a.Ignore != "" || a.Target.Builder != registry.BuilderKo {
		t.Fatalf("decision = %+v", a)
	}
}

func TestConnRepositoryUsesBuildKitAndSlugPrefix(t *testing.T) {
	d, err := Decide(body("clearedstack.com", "refs/heads/main", before, after))
	if err != nil {
		t.Fatalf("Decide(clearedstack.com): %v", err)
	}
	if d.Target.Slug != "conn" || d.Target.Image != "conn" || d.Target.Builder != registry.BuilderBuildKit {
		t.Fatalf("Target = %+v", d.Target)
	}
	id := d.Event.RunID()
	if !strings.HasPrefix(id, "conn-") || strings.Contains(id, ".") {
		t.Fatalf("RunID = %q, want conn-<hex> with no dots", id)
	}
}

func TestRunIDIsStableAndSeparatesRefs(t *testing.T) {
	main := Event{Repository: "muster", Ref: "refs/heads/main", After: after}
	tag := Event{Repository: "muster", Ref: "refs/tags/v1", After: after}
	if got := main.RunID(); got != "muster-674f2946f186" {
		t.Errorf("RunID = %q", got)
	}
	if main.RunID() == tag.RunID() {
		t.Error("a branch push and a tag push of one commit share a run")
	}
	moved := main
	moved.Before = before
	if main.RunID() != moved.RunID() {
		t.Error("the run depends on the commit the ref pointed at before")
	}
}

func TestBrokenDataIsReportedBeforeUnfamiliarData(t *testing.T) {
	// One more field than the rules cover, and a broken commit.
	_, err := Decide([]byte(`{"repository":"muster","ref":"refs/heads/main","before":"` + before + `","after":"zz","extra":1}`))
	if code(err) != fault.Parse {
		t.Fatalf("err = %v, want E_PARSE", err)
	}
	// A deleted ref of a kind no rule covers, with a broken before.
	_, err = Decide(body("muster", "refs/notes/review", "zz", zero))
	if code(err) != fault.Parse {
		t.Fatalf("err = %v, want E_PARSE", err)
	}
}

func TestANewRefIsTestedAndADeletedRefIsRefused(t *testing.T) {
	d, err := Decide(body("muster", "refs/heads/main", zero, after))
	if err != nil || d.Ignore != "" || d.Event.After != after {
		t.Fatalf("new ref: d=%+v err=%v", d, err)
	}
	for _, ref := range []string{"refs/heads/main", "refs/heads/try-a-thing", "refs/tags/v1"} {
		for _, was := range []string{before, zero} {
			if _, err := Decide(body("muster", ref, was, zero)); code(err) != fault.NeedsSubtool {
				t.Errorf("deleted %s (before %s): err = %v, want E_NEEDS_SUBTOOL", ref, was, err)
			}
		}
	}
}

func TestUnfamiliarRepositoryIsRefusedWithNeedsSubtool(t *testing.T) {
	for _, repo := range []string{"logbook", "unknown-tool", "a2345678901234567890123456789012"} {
		_, err := Decide(body(repo, "refs/heads/main", before, after))
		if code(err) != fault.NeedsSubtool {
			t.Errorf("repository %q: err = %v, want E_NEEDS_SUBTOOL", repo, err)
		}
	}
}

func TestTheFormatBeforeThisOneIsRefused(t *testing.T) {
	old := `{"repository":"muster","ref":"refs/heads/main","commit":"` + after + `","message":"Tidy the roll call"}`
	if _, err := Decide([]byte(old)); code(err) != fault.Parse {
		t.Fatalf("err = %v, want E_PARSE", err)
	}
	both := `{"repository":"muster","ref":"refs/heads/main","before":"` + before + `","after":"` + after + `","message":"[skip-ci]"}`
	if _, err := Decide([]byte(both)); code(err) != fault.NeedsSubtool {
		t.Fatalf("err = %v, want E_NEEDS_SUBTOOL", err)
	}
}

func TestRepositoryAndFieldShapes(t *testing.T) {
	long := "a2345678901234567890123456789012" // 32 characters: valid shape, unfamiliar repository
	if _, err := Decide(body(long, "refs/tags/v1.0.0-rc.1", zero, after)); code(err) != fault.NeedsSubtool {
		t.Fatalf("32-char repo: err = %v, want E_NEEDS_SUBTOOL", err)
	}
	for _, bad := range []string{long + "x", "ends-", "ends.", ".starts", "two..dots", "bad-.dot", "bad.-dot", "9lives", ""} {
		if _, err := Decide(body(bad, "refs/heads/main", before, after)); code(err) != fault.Parse {
			t.Errorf("repository %q: err = %v, want E_PARSE", bad, err)
		}
	}
	for _, bad := range []string{"refs/heads/", "refs//main", "refs/heads/a b", "refs/heads", "heads/main", "refs/heads/main/"} {
		if _, err := Decide(body("muster", bad, before, after)); code(err) != fault.Parse {
			t.Errorf("ref %q: err = %v, want E_PARSE", bad, err)
		}
	}
	for _, bad := range []string{"", "1111", after + "1", "ABABABABABABABABABABABABABABABABABABABAB", "g111111111111111111111111111111111111111"} {
		if _, err := Decide(body("muster", "refs/heads/main", bad, after)); code(err) != fault.Parse {
			t.Errorf("before %q: err = %v, want E_PARSE", bad, err)
		}
		if _, err := Decide(body("muster", "refs/heads/main", before, bad)); code(err) != fault.Parse {
			t.Errorf("after %q: err = %v, want E_PARSE", bad, err)
		}
	}
}
