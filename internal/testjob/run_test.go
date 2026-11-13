package testjob

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ClearedStack/keelworks/internal/fault"
)

type fakeCluster struct {
	createErr error
	outcome   Outcome
	waitErr   error
	stall     bool
	deleted   int
}

func (f *fakeCluster) Create(ctx context.Context, manifest []byte) error { return f.createErr }
func (f *fakeCluster) Wait(ctx context.Context, namespace, name string) (Outcome, error) {
	if f.stall {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	return f.outcome, f.waitErr
}
func (f *fakeCluster) Delete(ctx context.Context, namespace, name string) error {
	f.deleted++
	return nil
}

func spec() Spec {
	return Spec{RunID: "muster-000000000000", Repository: "muster", Ref: "refs/heads/main",
		Commit: "1111111111111111111111111111111111111111", Image: "made-up-image",
		PriorityClass: "made-up-low-priority", Deadline: time.Minute}
}

func TestRunVerdicts(t *testing.T) {
	cases := []struct {
		name    string
		cluster *fakeCluster
		want    Result
		deletes int
	}{
		{"passed", &fakeCluster{outcome: Passed}, Result{State: StatePassed}, 1},
		{"failed", &fakeCluster{outcome: Failed}, Result{State: StateFailed}, 1},
		{"cluster deadline", &fakeCluster{outcome: DeadlineExceeded}, Result{State: StateTimedOut, Code: fault.Timeout}, 1},
		{"preempted", &fakeCluster{outcome: Preempted}, Result{State: StateInterrupted, Reason: ReasonPreempted}, 1},
		{"lost", &fakeCluster{waitErr: ErrLost}, Result{State: StateInterrupted, Reason: ReasonJobLost}, 1},
		{"unknown outcome", &fakeCluster{outcome: Outcome(99)}, Result{State: StateInterrupted, Reason: ReasonJobLost}, 1},
		{"refused", &fakeCluster{createErr: ErrRefused}, Result{State: StateInterrupted, Reason: ReasonLaunchRefused}, 0},
		{"refused with outside text", &fakeCluster{createErr: errors.New("made-up-host.example refused")},
			Result{State: StateInterrupted, Reason: ReasonLaunchRefused}, 0},
	}
	for _, c := range cases {
		got := Run(context.Background(), c.cluster, spec())
		if got != c.want || c.cluster.deleted != c.deletes {
			t.Errorf("%s: got %+v with %d deletes, want %+v with %d", c.name, got, c.cluster.deleted, c.want, c.deletes)
		}
	}
}

func TestRunTimesOutAtTheDeadlineAndDeletesTheJob(t *testing.T) {
	c := &fakeCluster{stall: true}
	s := spec()
	s.Deadline = 20 * time.Millisecond
	got := Run(context.Background(), c, s)
	if (got != Result{State: StateTimedOut, Code: fault.Timeout}) || c.deleted != 1 {
		t.Fatalf("got %+v with %d deletes", got, c.deleted)
	}
}

func TestRunReportsShutdown(t *testing.T) {
	c := &fakeCluster{stall: true}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	got := Run(ctx, c, spec())
	if (got != Result{State: StateInterrupted, Reason: ReasonShutdown}) || c.deleted != 1 {
		t.Fatalf("got %+v with %d deletes", got, c.deleted)
	}
}

func TestRenderIsTheSameEveryTimeAndRoundsTheDeadlineUp(t *testing.T) {
	s := spec()
	s.Deadline = 1500 * time.Millisecond
	a, b := Render(s), Render(s)
	if string(a) != string(b) {
		t.Fatal("two renders of one Spec differ")
	}
	if !contains(a, `"activeDeadlineSeconds": 2,`) {
		t.Fatalf("deadline was not rounded up:\n%s", a)
	}
}

func contains(haystack []byte, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == needle {
			return true
		}
	}
	return false
}
