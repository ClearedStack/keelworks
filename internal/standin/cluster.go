// Package standin holds in-memory stand-ins for what Keelworks talks to: the
// cluster that runs Jobs and checks tokens, Lazarette, and Plainforge. Nothing
// here talks to a network or a disk.
package standin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/ClearedStack/keelworks/internal/buildjob"
	"github.com/ClearedStack/keelworks/internal/registry"
)

// Scripted outcomes of a build Job.
const (
	OutcomePass     = "pass"      // tests pass, built, signed in Lazarette
	OutcomeUnsigned = "unsigned"  // Job exits 0 with a digest that is not signed in Lazarette
	OutcomeSignFail = "sign-fail" // cosign signing fails inside the Job
	OutcomeFail     = "fail"      // golden tests or build fail before push
	OutcomeStall    = "stall"     // never finishes; the deadline ends it
	OutcomePreempt  = "preempt"   // evicted under memory pressure
)

// Script says how each made-up commit ends, which bearer tokens the cluster
// knows, and any per-repository app.yaml overrides in Plainforge.
type Script struct {
	Outcomes  []ScriptedOutcome  `json:"outcomes"`
	Callers   []ScriptedCaller   `json:"callers"`
	Manifests []ScriptedManifest `json:"manifests"`
}

// ScriptedOutcome is one line of a Script.
type ScriptedOutcome struct {
	Repository string `json:"repository"`
	Commit     string `json:"commit"`
	Outcome    string `json:"outcome"`
	Digest     string `json:"digest,omitempty"`
}

// DeterministicDigest returns a deterministic sha256 digest for a made-up
// repository and commit when a script entry does not specify one explicitly.
func DeterministicDigest(repository, commit string) string {
	sum := sha256.Sum256([]byte("keelworks-standin\x00" + repository + "\x00" + commit))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ParseScript reads a Script from JSON and refuses anything it does not know.
func ParseScript(data []byte) (Script, error) {
	var s Script
	if err := json.Unmarshal(data, &s); err != nil {
		return Script{}, errors.New("stand-in script is not valid JSON")
	}
	for i, o := range s.Outcomes {
		switch o.Outcome {
		case OutcomePass, OutcomeUnsigned, OutcomeSignFail, OutcomeFail, OutcomeStall, OutcomePreempt:
		default:
			return Script{}, fmt.Errorf("stand-in script line %d has an unknown outcome", i+1)
		}
		if o.Digest != "" && !registry.ValidDigest(o.Digest) {
			return Script{}, fmt.Errorf("stand-in script line %d has a malformed digest", i+1)
		}
	}
	seen := map[string]bool{}
	for i, c := range s.Callers {
		switch {
		case c.Token == "":
			return Script{}, fmt.Errorf("stand-in script caller %d has no token", i+1)
		case seen[c.Token]:
			return Script{}, fmt.Errorf("stand-in script caller %d repeats a token", i+1)
		case c.Review != "" && c.Review != ReviewStall && c.Review != ReviewUnavailable:
			return Script{}, fmt.Errorf("stand-in script caller %d has an unknown review", i+1)
		}
		seen[c.Token] = true
	}
	for i, m := range s.Manifests {
		switch m.Pin {
		case "", "stall", "unavailable":
		default:
			return Script{}, fmt.Errorf("stand-in script manifest %d has an unknown pin mode", i+1)
		}
	}
	return s, nil
}

type liveJob struct {
	outcome ScriptedOutcome
	app     string
	known   bool
}

// Cluster is an in-memory buildjob.Cluster. It checks every Job against the
// security and scheduling rules of SPEC.md and refuses one that breaks them.
type Cluster struct {
	mu        sync.Mutex
	outcomes  map[string]ScriptedOutcome // repository@commit -> outcome
	live      map[string]liveJob         // namespace/name -> liveJob
	lazarette *Lazarette
	created   [][]byte
}

// keep is how many accepted manifests the stand-in remembers.
const keep = 64

// NewCluster returns a Cluster that plays back script. When laz is non-nil, a
// passing Job also records its signed digest in laz.
func NewCluster(script Script, laz *Lazarette) *Cluster {
	c := &Cluster{
		outcomes:  map[string]ScriptedOutcome{},
		live:      map[string]liveJob{},
		lazarette: laz,
	}
	for _, o := range script.Outcomes {
		if o.Digest == "" && (o.Outcome == OutcomePass || o.Outcome == OutcomeUnsigned) {
			o.Digest = DeterministicDigest(o.Repository, o.Commit)
		}
		c.outcomes[o.Repository+"@"+o.Commit] = o
	}
	return c
}

type job struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		ActiveDeadlineSeconds int64 `json:"activeDeadlineSeconds"`
		BackoffLimit          *int  `json:"backoffLimit"`
		Template              struct {
			Spec struct {
				AutomountServiceAccountToken *bool  `json:"automountServiceAccountToken"`
				PriorityClassName            string `json:"priorityClassName"`
				HostNetwork                  bool   `json:"hostNetwork"`
				HostPID                      bool   `json:"hostPID"`
				HostIPC                      bool   `json:"hostIPC"`
				Affinity                     struct {
					NodeAffinity struct {
						Required struct {
							NodeSelectorTerms []struct {
								MatchExpressions []struct {
									Key      string   `json:"key"`
									Operator string   `json:"operator"`
									Values   []string `json:"values"`
								} `json:"matchExpressions"`
							} `json:"nodeSelectorTerms"`
						} `json:"requiredDuringSchedulingIgnoredDuringExecution"`
					} `json:"nodeAffinity"`
				} `json:"affinity"`
				Volumes []struct {
					Name     string          `json:"name"`
					HostPath json.RawMessage `json:"hostPath"`
					EmptyDir json.RawMessage `json:"emptyDir"`
					Secret   *struct {
						SecretName string `json:"secretName"`
						Items      []struct {
							Key  string `json:"key"`
							Path string `json:"path"`
						} `json:"items"`
					} `json:"secret"`
				} `json:"volumes"`
				SecurityContext struct {
					RunAsNonRoot   bool `json:"runAsNonRoot"`
					SeccompProfile struct {
						Type string `json:"type"`
					} `json:"seccompProfile"`
				} `json:"securityContext"`
				Containers []struct {
					Image string `json:"image"`
					Env   []struct {
						Name  string `json:"name"`
						Value string `json:"value"`
					} `json:"env"`
					VolumeMounts []struct {
						Name     string `json:"name"`
						ReadOnly bool   `json:"readOnly"`
					} `json:"volumeMounts"`
					SecurityContext struct {
						AllowPrivilegeEscalation *bool `json:"allowPrivilegeEscalation"`
						Privileged               bool  `json:"privileged"`
						ReadOnlyRootFilesystem   bool  `json:"readOnlyRootFilesystem"`
						RunAsNonRoot             bool  `json:"runAsNonRoot"`
						Capabilities             struct {
							Add  []string `json:"add"`
							Drop []string `json:"drop"`
						} `json:"capabilities"`
					} `json:"securityContext"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

// Admit checks a manifest against the rules a Keelworks build Job must keep.
// It returns the first broken rule, or "" when the Job is allowed.
func Admit(manifest []byte) string {
	var j job
	if err := json.Unmarshal(manifest, &j); err != nil {
		return "manifest is not valid JSON"
	}
	pod := j.Spec.Template.Spec
	switch {
	case j.APIVersion != "batch/v1" || j.Kind != "Job":
		return "not a batch/v1 Job"
	case j.Metadata.Namespace != buildjob.Namespace:
		return "wrong namespace"
	case j.Metadata.Name == "":
		return "no name"
	case j.Spec.ActiveDeadlineSeconds <= 0:
		return "no deadline"
	case pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken:
		return "mounts service account token"
	case pod.PriorityClassName == "":
		return "no priority class"
	case pod.HostNetwork || pod.HostPID || pod.HostIPC:
		return "uses the host's namespaces"
	case !hasRequiredNodeAffinity(&j):
		return "missing arm64 or cm3588-plus node affinity"
	case !pod.SecurityContext.RunAsNonRoot:
		return "pod may run as root"
	case pod.SecurityContext.SeccompProfile.Type != "RuntimeDefault":
		return "missing RuntimeDefault seccomp profile"
	case len(pod.Containers) != 1:
		return "not exactly one container"
	}
	for _, v := range pod.Volumes {
		if len(v.HostPath) != 0 {
			return "mounts a host path"
		}
		switch v.Name {
		case "work", "tmp":
			if len(v.EmptyDir) == 0 || v.Secret != nil {
				return "unexpected volume source"
			}
		case "signing-key":
			if v.Secret == nil || v.Secret.SecretName == "" || len(v.Secret.Items) != 1 || v.Secret.Items[0].Key != "cosign.key" {
				return "signing-key volume must project cosign.key from a Secret"
			}
		default:
			return "unexpected volume"
		}
	}
	c := pod.Containers[0]
	sc := c.SecurityContext
	switch {
	case c.Image == "":
		return "no image"
	case sc.Privileged:
		return "privileged container"
	case sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation:
		return "privilege escalation allowed"
	case !sc.ReadOnlyRootFilesystem:
		return "root filesystem is writable"
	case !sc.RunAsNonRoot:
		return "container may run as root"
	case len(sc.Capabilities.Add) != 0:
		return "adds a capability"
	case len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL":
		return "does not drop every capability"
	}
	for _, vm := range c.VolumeMounts {
		if vm.Name == "signing-key" && !vm.ReadOnly {
			return "signing-key mount is writable"
		}
	}
	return ""
}

func hasRequiredNodeAffinity(j *job) bool {
	terms := j.Spec.Template.Spec.Affinity.NodeAffinity.Required.NodeSelectorTerms
	if len(terms) == 0 {
		return false
	}
	for _, term := range terms {
		var archOK, hostOK bool
		for _, expr := range term.MatchExpressions {
			if expr.Key == "kubernetes.io/arch" && expr.Operator == "In" && len(expr.Values) == 1 && expr.Values[0] == "arm64" {
				archOK = true
			}
			if expr.Key == "kubernetes.io/hostname" && expr.Operator == "NotIn" && len(expr.Values) == 1 && expr.Values[0] == "cm3588-plus" {
				hostOK = true
			}
		}
		if !archOK || !hostOK {
			return false
		}
	}
	return true
}

// Create admits and stores a Job.
func (c *Cluster) Create(ctx context.Context, manifest []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if Admit(manifest) != "" {
		return buildjob.ErrRefused
	}
	var j job
	_ = json.Unmarshal(manifest, &j)
	env := map[string]string{}
	for _, e := range j.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	key := j.Metadata.Namespace + "/" + j.Metadata.Name
	repo := env[buildjob.EnvRepository]
	commit := env[buildjob.EnvCommit]

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.live[key]; exists {
		return buildjob.ErrRefused
	}
	out, known := c.outcomes[repo+"@"+commit]
	c.live[key] = liveJob{outcome: out, app: env[buildjob.EnvApp], known: known}
	if len(c.created) == keep {
		c.created = c.created[1:]
	}
	c.created = append(c.created, append([]byte(nil), manifest...))
	return nil
}

// Wait plays back the scripted outcome of a Job.
func (c *Cluster) Wait(ctx context.Context, namespace, name string) (buildjob.Outcome, error) {
	c.mu.Lock()
	lj, ok := c.live[namespace+"/"+name]
	laz := c.lazarette
	c.mu.Unlock()
	if !ok || !lj.known {
		return buildjob.Outcome{}, buildjob.ErrLost
	}
	switch lj.outcome.Outcome {
	case OutcomePass:
		if laz != nil {
			if target, ok := registry.ByRepository(lj.outcome.Repository); ok {
				laz.RecordSigned(target.Image, lj.outcome.Digest)
			}
		}
		return buildjob.Outcome{Status: buildjob.Succeeded, Digest: lj.outcome.Digest}, nil
	case OutcomeUnsigned:
		return buildjob.Outcome{Status: buildjob.Succeeded, Digest: lj.outcome.Digest}, nil
	case OutcomeSignFail:
		return buildjob.Outcome{Status: buildjob.SignFailed}, nil
	case OutcomeFail:
		return buildjob.Outcome{Status: buildjob.TestFailed}, nil
	case OutcomePreempt:
		return buildjob.Outcome{Status: buildjob.Preempted}, nil
	case OutcomeStall:
		<-ctx.Done()
		return buildjob.Outcome{}, ctx.Err()
	}
	return buildjob.Outcome{}, buildjob.ErrLost
}

// Delete removes a Job. Deleting a Job that is not there is not an error.
func (c *Cluster) Delete(ctx context.Context, namespace, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.live, namespace+"/"+name)
	return nil
}

// Created returns the manifests the cluster accepted, oldest first, the
// last 64 at most.
func (c *Cluster) Created() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.created...)
}

// Live returns how many Jobs exist right now.
func (c *Cluster) Live() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.live)
}
