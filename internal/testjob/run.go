package testjob

import (
	"context"
	"errors"
	"time"

	"github.com/clearedstack/keelworks/internal/fault"
)

// Outcome is how a Job ended, as the cluster reports it.
type Outcome int

const (
	// Passed: the tests ran and passed.
	Passed Outcome = iota + 1
	// Failed: the tests ran and failed.
	Failed
	// DeadlineExceeded: the cluster stopped the Job at its deadline.
	DeadlineExceeded
	// Preempted: the cluster evicted the Job to free memory.
	Preempted
)

// Errors a Cluster may return. Keelworks never shows a cluster's own error
// text to anyone.
var (
	ErrRefused = errors.New("cluster refused the job")
	ErrLost    = errors.New("cluster has no such job")
)

// Cluster is the part of the Kubernetes API Keelworks needs for a test Job.
type Cluster interface {
	// Create submits a Job manifest as rendered by Render.
	Create(ctx context.Context, manifest []byte) error
	// Wait blocks until the named Job ends and says how.
	Wait(ctx context.Context, namespace, name string) (Outcome, error)
	// Delete removes the Job and its pod.
	Delete(ctx context.Context, namespace, name string) error
}

// States a finished run can be in.
const (
	StatePassed      = "passed"
	StateFailed      = "failed"
	StateTimedOut    = "timed-out"
	StateInterrupted = "interrupted"
)

// Reasons for StateInterrupted.
const (
	ReasonPreempted     = "preempted"
	ReasonLaunchRefused = "launch-refused"
	ReasonJobLost       = "job-lost"
	ReasonShutdown      = "shutdown"
)

// Result is the verdict on one test Job.
type Result struct {
	State  string
	Code   fault.Code // E_TIMEOUT when State is timed-out, otherwise empty
	Reason string     // set when State is interrupted
}

// cleanupTimeout bounds the Delete call that follows every Job.
const cleanupTimeout = 10 * time.Second

// Run creates the Job for spec, waits for it within spec.Deadline, deletes
// it, and returns the verdict. It never retries and never guesses: anything
// other than a clear pass or fail is reported as what it was.
func Run(ctx context.Context, c Cluster, spec Spec) Result {
	jobCtx, cancel := context.WithTimeout(ctx, spec.Deadline)
	defer cancel()

	if err := c.Create(jobCtx, Render(spec)); err != nil {
		if r, ok := stopped(ctx, jobCtx); ok {
			cleanup(c, spec)
			return r
		}
		return Result{State: StateInterrupted, Reason: ReasonLaunchRefused}
	}
	defer cleanup(c, spec)

	outcome, err := c.Wait(jobCtx, Namespace, spec.Name())
	if err != nil {
		if r, ok := stopped(ctx, jobCtx); ok {
			return r
		}
		return Result{State: StateInterrupted, Reason: ReasonJobLost}
	}
	switch outcome {
	case Passed:
		return Result{State: StatePassed}
	case Failed:
		return Result{State: StateFailed}
	case DeadlineExceeded:
		return Result{State: StateTimedOut, Code: fault.Timeout}
	case Preempted:
		return Result{State: StateInterrupted, Reason: ReasonPreempted}
	}
	return Result{State: StateInterrupted, Reason: ReasonJobLost}
}

// stopped reports whether the call ended because Keelworks is shutting down
// or because the deadline passed.
func stopped(parent, jobCtx context.Context) (Result, bool) {
	if parent.Err() != nil {
		return Result{State: StateInterrupted, Reason: ReasonShutdown}, true
	}
	if errors.Is(jobCtx.Err(), context.DeadlineExceeded) {
		return Result{State: StateTimedOut, Code: fault.Timeout}, true
	}
	return Result{}, false
}

func cleanup(c Cluster, spec Spec) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	_ = c.Delete(ctx, Namespace, spec.Name())
}
