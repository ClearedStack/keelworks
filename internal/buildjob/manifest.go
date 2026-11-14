// Package buildjob renders Keelworks' ephemeral batch/v1 build Jobs, runs them
// on a Kubernetes cluster (or an in-memory stand-in), verifies the resulting
// signed image in Lazarette, and pins the new digest in Plainforge.
package buildjob

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/ClearedStack/keelworks/internal/registry"
)

// Namespace is where Keelworks and its ephemeral build Jobs run.
const Namespace = "clearedstack"

// PodLabelName identifies build Job pods for NetworkPolicy selection.
const PodLabelName = "keelworks-build"

// Environment variables passed to the builder container.
const (
	EnvRepository    = "KEELWORKS_REPOSITORY"
	EnvApp           = "KEELWORKS_APP"
	EnvBuilder       = "KEELWORKS_BUILDER"
	EnvRef           = "KEELWORKS_REF"
	EnvCommit        = "KEELWORKS_COMMIT"
	EnvPlainforgeURL = "KEELWORKS_PLAINFORGE_URL"
	EnvImage         = "KEELWORKS_IMAGE"
	EnvCosignKey     = "KEELWORKS_COSIGN_KEY"
)

const (
	nonRootUID    = 65532
	cosignKeyDir  = "/var/run/secrets/keelworks/cosign"
	cosignKeyFile = cosignKeyDir + "/cosign.key"
	workDir       = "/work"
	tmpDir        = "/tmp"
)

// Spec is everything that decides a build Job. Two equal Specs render the exact
// same bytes.
type Spec struct {
	RunID         string
	Target        registry.Target
	Ref           string
	Commit        string
	BuilderImage  string        // pinned ko or rootless BuildKit image in Lazarette
	PlainforgeURL string        // cluster-private Plainforge base URL
	ImageRegistry string        // host:port of Lazarette
	CosignSecret  string        // Secret holding cosign.key in clearedstack
	PriorityClass string        // low PriorityClass for ephemeral build Jobs
	Deadline      time.Duration // how long the Job may run
}

// Name returns the Job's Kubernetes object name (a valid DNS label).
func (s Spec) Name() string { return "keelworks-build-" + s.RunID }

// ImageRef returns the full Lazarette image repository path without a tag or digest.
func (s Spec) ImageRef() string {
	return strings.TrimRight(s.ImageRegistry, "/") + "/" + s.Target.Image
}

type manifest struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   metadata `json:"metadata"`
	Spec       jobSpec  `json:"spec"`
}

type metadata struct {
	Name      string            `json:"name,omitempty"`
	Namespace string            `json:"namespace,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type jobSpec struct {
	ActiveDeadlineSeconds int64       `json:"activeDeadlineSeconds"`
	BackoffLimit          int         `json:"backoffLimit"`
	Template              podTemplate `json:"template"`
}

type podTemplate struct {
	Metadata metadata `json:"metadata"`
	Spec     podSpec  `json:"spec"`
}

type podSpec struct {
	Affinity                     affinity           `json:"affinity"`
	AutomountServiceAccountToken bool               `json:"automountServiceAccountToken"`
	Containers                   []container        `json:"containers"`
	PriorityClassName            string             `json:"priorityClassName"`
	RestartPolicy                string             `json:"restartPolicy"`
	SecurityContext              podSecurityContext `json:"securityContext"`
	Volumes                      []volume           `json:"volumes"`
}

type affinity struct {
	NodeAffinity nodeAffinity `json:"nodeAffinity"`
}

type nodeAffinity struct {
	Required requiredNodeAffinity `json:"requiredDuringSchedulingIgnoredDuringExecution"`
}

type requiredNodeAffinity struct {
	NodeSelectorTerms []nodeSelectorTerm `json:"nodeSelectorTerms"`
}

type nodeSelectorTerm struct {
	MatchExpressions []matchExpression `json:"matchExpressions"`
}

type matchExpression struct {
	Key      string   `json:"key"`
	Operator string   `json:"operator"`
	Values   []string `json:"values"`
}

type podSecurityContext struct {
	FSGroup        int64          `json:"fsGroup"`
	RunAsGroup     int64          `json:"runAsGroup"`
	RunAsNonRoot   bool           `json:"runAsNonRoot"`
	RunAsUser      int64          `json:"runAsUser"`
	SeccompProfile seccompProfile `json:"seccompProfile"`
}

type seccompProfile struct {
	Type string `json:"type"`
}

type container struct {
	Name                     string                   `json:"name"`
	Image                    string                   `json:"image"`
	Env                      []envVar                 `json:"env"`
	Resources                resources                `json:"resources"`
	SecurityContext          containerSecurityContext `json:"securityContext"`
	TerminationMessagePath   string                   `json:"terminationMessagePath"`
	TerminationMessagePolicy string                   `json:"terminationMessagePolicy"`
	VolumeMounts             []volumeMount            `json:"volumeMounts"`
}

type envVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type resources struct {
	Limits   map[string]string `json:"limits"`
	Requests map[string]string `json:"requests"`
}

type containerSecurityContext struct {
	AllowPrivilegeEscalation bool         `json:"allowPrivilegeEscalation"`
	Capabilities             capabilities `json:"capabilities"`
	Privileged               bool         `json:"privileged"`
	ReadOnlyRootFilesystem   bool         `json:"readOnlyRootFilesystem"`
	RunAsNonRoot             bool         `json:"runAsNonRoot"`
}

type capabilities struct {
	Drop []string `json:"drop"`
}

type volumeMount struct {
	MountPath string `json:"mountPath"`
	Name      string `json:"name"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

type volume struct {
	EmptyDir *emptyDirVolume `json:"emptyDir,omitempty"`
	Name     string          `json:"name"`
	Secret   *secretVolume   `json:"secret,omitempty"`
}

type emptyDirVolume struct {
	SizeLimit string `json:"sizeLimit"`
}

type secretVolume struct {
	Items      []keyToPath `json:"items"`
	SecretName string      `json:"secretName"`
}

type keyToPath struct {
	Key  string `json:"key"`
	Path string `json:"path"`
}

// Render returns the Kubernetes batch/v1 Job for s as indented JSON ending in
// a newline. The Job runs once, on arm64 (excluding cm3588-plus), as non-root
// user 65532, with a read-only root filesystem, all capabilities dropped, no
// privilege escalation, no service account token, scratch emptyDirs, and
// read-only cosign.key projection.
func Render(s Spec) []byte {
	seconds := int64((s.Deadline + time.Second - 1) / time.Second)
	labels := map[string]string{
		"app.kubernetes.io/component":  "build",
		"app.kubernetes.io/instance":   s.RunID,
		"app.kubernetes.io/managed-by": "keelworks",
		"app.kubernetes.io/name":       PodLabelName,
		"app.kubernetes.io/part-of":    s.Target.Slug,
	}
	m := manifest{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Metadata:   metadata{Name: s.Name(), Namespace: Namespace, Labels: labels},
		Spec: jobSpec{
			ActiveDeadlineSeconds: seconds,
			BackoffLimit:          0,
			Template: podTemplate{
				Metadata: metadata{Labels: labels},
				Spec: podSpec{
					Affinity: affinity{
						NodeAffinity: nodeAffinity{
							Required: requiredNodeAffinity{
								NodeSelectorTerms: []nodeSelectorTerm{{
									MatchExpressions: []matchExpression{
										{Key: "kubernetes.io/arch", Operator: "In", Values: []string{"arm64"}},
										{Key: "kubernetes.io/hostname", Operator: "NotIn", Values: []string{"cm3588-plus"}},
									},
								}},
							},
						},
					},
					AutomountServiceAccountToken: false,
					Containers: []container{{
						Name:  "build",
						Image: s.BuilderImage,
						Env: []envVar{
							{Name: EnvRepository, Value: s.Target.Repository},
							{Name: EnvApp, Value: s.Target.Slug},
							{Name: EnvBuilder, Value: string(s.Target.Builder)},
							{Name: EnvRef, Value: s.Ref},
							{Name: EnvCommit, Value: s.Commit},
							{Name: EnvPlainforgeURL, Value: strings.TrimRight(s.PlainforgeURL, "/")},
							{Name: EnvImage, Value: s.ImageRef()},
							{Name: EnvCosignKey, Value: cosignKeyFile},
							{Name: "HOME", Value: workDir},
							{Name: "TMPDIR", Value: tmpDir},
						},
						Resources: resources{
							Requests: map[string]string{"cpu": "100m", "memory": "128Mi"},
							Limits:   map[string]string{"cpu": "1", "memory": "1Gi"},
						},
						SecurityContext: containerSecurityContext{
							AllowPrivilegeEscalation: false,
							Capabilities:             capabilities{Drop: []string{"ALL"}},
							Privileged:               false,
							ReadOnlyRootFilesystem:   true,
							RunAsNonRoot:             true,
						},
						TerminationMessagePath:   "/dev/termination-log",
						TerminationMessagePolicy: "File",
						VolumeMounts: []volumeMount{
							{Name: "work", MountPath: workDir},
							{Name: "tmp", MountPath: tmpDir},
							{Name: "signing-key", MountPath: cosignKeyDir, ReadOnly: true},
						},
					}},
					PriorityClassName: s.PriorityClass,
					RestartPolicy:     "Never",
					SecurityContext: podSecurityContext{
						FSGroup:        nonRootUID,
						RunAsGroup:     nonRootUID,
						RunAsNonRoot:   true,
						RunAsUser:      nonRootUID,
						SeccompProfile: seccompProfile{Type: "RuntimeDefault"},
					},
					Volumes: []volume{
						{Name: "work", EmptyDir: &emptyDirVolume{SizeLimit: "1Gi"}},
						{Name: "tmp", EmptyDir: &emptyDirVolume{SizeLimit: "256Mi"}},
						{
							Name: "signing-key",
							Secret: &secretVolume{
								SecretName: s.CosignSecret,
								Items:      []keyToPath{{Key: "cosign.key", Path: "cosign.key"}},
							},
						},
					},
				},
			},
		},
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		panic("buildjob: manifest cannot be encoded") // unreachable: plain structs
	}
	return append(out, '\n')
}
