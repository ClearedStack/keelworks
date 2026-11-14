package buildjob

import (
	"context"
	"errors"
	"time"

	"github.com/ClearedStack/keelworks/internal/fault"
	"github.com/ClearedStack/keelworks/internal/lazarette"
	"github.com/ClearedStack/keelworks/internal/plainforge"
	"github.com/ClearedStack/keelworks/internal/registry"
)

// Status is how a build Job ended on the cluster.
type Status int

const (
	// Succeeded: tests passed, image built, signed, and pushed; Digest holds sha256:<hex>.
	Succeeded Status = iota + 1
	// TestFailed: golden fixture tests or image compilation failed before push.
	TestFailed
	// SignFailed: cosign signing failed, or the Job exited 0 without a valid digest.
	SignFailed
	// DeadlineExceeded: the cluster stopped the Job at its deadline.
	DeadlineExceeded
	// Preempted: the cluster evicted the Job under memory pressure.
	Preempted
)

// Outcome is what a Cluster reports when a build Job ends.
type Outcome struct {
	Status Status
	Digest string
}

// Errors a Cluster may return. Keelworks never repeats a cluster's error text.
var (
	ErrRefused = errors.New("cluster refused the job")
	ErrLost    = errors.New("cluster has no such job")
)

// Cluster is the part of the Kubernetes API Keelworks needs for build Jobs.
type Cluster interface {
	Create(ctx context.Context, manifest []byte) error
	Wait(ctx context.Context, namespace, name string) (Outcome, error)
	Delete(ctx context.Context, namespace, name string) error
}

// States a finished run can be in.
const (
	StatePassed      = "passed"
	StateFailed      = "failed"
	StateTimedOut    = "timed-out"
	StateInterrupted = "interrupted"
)

// Reasons for StateFailed.
const (
	ReasonTestFailed = "test-failed"
	ReasonSignFailed = "sign-failed"
	ReasonPinFailed  = "manifest-pin-failed"
)

// Reasons for StateInterrupted.
const (
	ReasonPreempted            = "preempted"
	ReasonLaunchRefused        = "launch-refused"
	ReasonJobLost              = "job-lost"
	ReasonLazaretteUnavailable = "lazarette-unavailable"
	ReasonPinUnavailable       = "pin-unavailable"
	ReasonShutdown             = "shutdown"
)

// Result is the verdict on one end-to-end build run.
type Result struct {
	State  string
	Digest string     // sha256:<64 hex> when State is passed, otherwise empty
	Code   fault.Code // E_TIMEOUT on timed-out, E_PARSE on manifest-pin-failed
	Reason string     // set when State is failed or interrupted
}

// cleanupTimeout bounds the Delete call that follows every Job.
const cleanupTimeout = 10 * time.Second

// Run launches the build Job for spec, cleans it up, verifies the signed image
// in Lazarette, and pins the verified digest into app.yaml in Plainforge. It
// never updates app.yaml unless tests passed and Lazarette holds a valid
// cosign signature for the built digest.
func Run(ctx context.Context, c Cluster, verifier lazarette.Verifier, pinner plainforge.Pinner, spec Spec) Result {
	outcome, res, ok := runJob(ctx, c, spec)
	if !ok {
		return res
	}
	if !registry.ValidDigest(outcome.Digest) {
		return Result{State: StateFailed, Reason: ReasonSignFailed}
	}

	switch verifier.Verify(ctx, spec.Target.Image, outcome.Digest) {
	case lazarette.Verified:
	case lazarette.Unsigned:
		return Result{State: StateFailed, Reason: ReasonSignFailed}
	case lazarette.TimedOut:
		return Result{State: StateTimedOut, Code: fault.Timeout}
	default:
		if ctx.Err() != nil {
			return Result{State: StateInterrupted, Reason: ReasonShutdown}
		}
		return Result{State: StateInterrupted, Reason: ReasonLazaretteUnavailable}
	}

	switch pinner.Pin(ctx, spec.Target.Repository, spec.Target.Slug, outcome.Digest) {
	case plainforge.Pinned:
		return Result{State: StatePassed, Digest: outcome.Digest}
	case plainforge.BadManifest:
		return Result{State: StateFailed, Code: fault.Parse, Reason: ReasonPinFailed}
	case plainforge.TimedOut:
		return Result{State: StateTimedOut, Code: fault.Timeout}
	default:
		if ctx.Err() != nil {
			return Result{State: StateInterrupted, Reason: ReasonShutdown}
		}
		return Result{State: StateInterrupted, Reason: ReasonPinUnavailable}
	}
}

func runJob(ctx context.Context, c Cluster, spec Spec) (Outcome, Result, bool) {
	jobCtx, cancel := context.WithTimeout(ctx, spec.Deadline)
	defer cancel()

	if err := c.Create(jobCtx, Render(spec)); err != nil {
		if r, ok := stopped(ctx, jobCtx); ok {
			cleanup(c, spec)
			return Outcome{}, r, false
		}
		return Outcome{}, Result{State: StateInterrupted, Reason: ReasonLaunchRefused}, false
	}
	defer cleanup(c, spec)

	outcome, err := c.Wait(jobCtx, Namespace, spec.Name())
	if err != nil {
		if r, ok := stopped(ctx, jobCtx); ok {
			return Outcome{}, r, false
		}
		return Outcome{}, Result{State: StateInterrupted, Reason: ReasonJobLost}, false
	}
	switch outcome.Status {
	case Succeeded:
		return outcome, Result{}, true
	case TestFailed:
		return Outcome{}, Result{State: StateFailed, Reason: ReasonTestFailed}, false
	case SignFailed:
		return Outcome{}, Result{State: StateFailed, Reason: ReasonSignFailed}, false
	case DeadlineExceeded:
		return Outcome{}, Result{State: StateTimedOut, Code: fault.Timeout}, false
	case Preempted:
		return Outcome{}, Result{State: StateInterrupted, Reason: ReasonPreempted}, false
	}
	return Outcome{}, Result{State: StateInterrupted, Reason: ReasonJobLost}, false
}

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
