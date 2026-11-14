// Package registry holds Keelworks' copy of the ClearedStack naming registry
// entries for the repositories Keelworks tests, builds and signs, plus image
// digest validation shared across packages.
//
// The one place names are written down is the ClearedStack naming registry.
// If this table and the registry disagree, the registry is right.
package registry

import "strings"

// Builder names the unprivileged builder a Job runs for a repository.
type Builder string

const (
	// BuilderKo builds Go control-plane services and workers straight from
	// source with ko, with no Dockerfile and no daemon.
	BuilderKo Builder = "ko"
	// BuilderBuildKit builds multi-layer web workloads inside a rootless
	// BuildKit pod.
	BuilderBuildKit Builder = "buildkit"
)

// Target is a repository Keelworks has a build rule for.
type Target struct {
	Slug       string  // canonical component identifier and app.yaml name
	Repository string  // repository name in Plainforge
	Image      string  // image repository name in Lazarette
	Builder    Builder // builder used by the ephemeral Job
}

// targets lists every repository SPEC.md gives a builder rule for:
// the five Go control-plane services, Conn (clearedstack.com), and the ten Go
// workers (Trawl through Bellbuoy). Logbook uses upstream postgres:17 and is
// intentionally absent: Keelworks does not build an image for it.
var targets = []Target{
	{Slug: "conn", Repository: "clearedstack.com", Image: "conn", Builder: BuilderBuildKit},
	{Slug: "muster", Repository: "muster", Image: "muster", Builder: BuilderKo},
	{Slug: "plainforge", Repository: "plainforge", Image: "plainforge", Builder: BuilderKo},
	{Slug: "lazarette", Repository: "lazarette", Image: "lazarette", Builder: BuilderKo},
	{Slug: "keelworks", Repository: "keelworks", Image: "keelworks", Builder: BuilderKo},
	{Slug: "shipkeeper", Repository: "shipkeeper", Image: "shipkeeper", Builder: BuilderKo},
	{Slug: "trawl", Repository: "trawl", Image: "trawl", Builder: BuilderKo},
	{Slug: "sextant", Repository: "sextant", Image: "sextant", Builder: BuilderKo},
	{Slug: "assay", Repository: "assay", Image: "assay", Builder: BuilderKo},
	{Slug: "countersign", Repository: "countersign", Image: "countersign", Builder: BuilderKo},
	{Slug: "waybill", Repository: "waybill", Image: "waybill", Builder: BuilderKo},
	{Slug: "moldloft", Repository: "moldloft", Image: "moldloft", Builder: BuilderKo},
	{Slug: "shakedown", Repository: "shakedown", Image: "shakedown", Builder: BuilderKo},
	{Slug: "chartroom", Repository: "chartroom", Image: "chartroom", Builder: BuilderKo},
	{Slug: "tidegauge", Repository: "tidegauge", Image: "tidegauge", Builder: BuilderKo},
	{Slug: "bellbuoy", Repository: "bellbuoy", Image: "bellbuoy", Builder: BuilderKo},
}

var byRepo = func() map[string]Target {
	m := make(map[string]Target, len(targets))
	for _, t := range targets {
		m[t.Repository] = t
	}
	return m
}()

// All returns a copy of every target Keelworks knows how to build.
func All() []Target {
	out := make([]Target, len(targets))
	copy(out, targets)
	return out
}

// ByRepository looks up the build target for a Plainforge repository name.
func ByRepository(repository string) (Target, bool) {
	t, ok := byRepo[repository]
	return t, ok
}

// ValidDigest reports whether s is "sha256:" followed by 64 lowercase
// hexadecimal digits.
func ValidDigest(s string) bool {
	hex, ok := strings.CutPrefix(s, "sha256:")
	if !ok || len(hex) != 64 {
		return false
	}
	for i := 0; i < len(hex); i++ {
		c := hex[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
