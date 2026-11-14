package registry

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestComponentJSONMatchesSelfEntry(t *testing.T) {
	data, err := os.ReadFile("../../component.json")
	if err != nil {
		t.Fatal(err)
	}
	var self struct {
		Slug       string `json:"slug"`
		Repository string `json:"repository"`
		Image      string `json:"image"`
	}
	if err := json.Unmarshal(data, &self); err != nil {
		t.Fatal(err)
	}
	got, ok := ByRepository(self.Repository)
	if !ok {
		t.Fatalf("repository %q missing from registry table", self.Repository)
	}
	if got.Slug != self.Slug || got.Image != self.Image || got.Builder != BuilderKo {
		t.Fatalf("got %+v, want slug=%q image=%q builder=ko", got, self.Slug, self.Image)
	}
}

func TestBuildersMatchSpec(t *testing.T) {
	conn, ok := ByRepository("clearedstack.com")
	if !ok || conn.Slug != "conn" || conn.Image != "conn" || conn.Builder != BuilderBuildKit {
		t.Fatalf("clearedstack.com = %+v, %v", conn, ok)
	}
	koRepos := []string{
		"plainforge", "lazarette", "keelworks", "shipkeeper", "muster",
		"trawl", "sextant", "assay", "countersign", "waybill",
		"moldloft", "shakedown", "chartroom", "tidegauge", "bellbuoy",
	}
	for _, repo := range koRepos {
		got, ok := ByRepository(repo)
		if !ok || got.Slug != repo || got.Image != repo || got.Builder != BuilderKo {
			t.Errorf("ByRepository(%q) = %+v, %v", repo, got, ok)
		}
	}
	if len(All()) != 1+len(koRepos) {
		t.Fatalf("All() has %d targets, want %d", len(All()), 1+len(koRepos))
	}
	for _, unknown := range []string{"logbook", "unknown-tool", ""} {
		if _, ok := ByRepository(unknown); ok {
			t.Errorf("ByRepository(%q) unexpectedly succeeded", unknown)
		}
	}
}

func TestValidDigest(t *testing.T) {
	good := "sha256:" + strings.Repeat("a9", 32)
	if !ValidDigest(good) {
		t.Fatalf("ValidDigest(%q) = false, want true", good)
	}
	for _, bad := range []string{
		"",
		"sha256:",
		"sha256:abcd",
		"sha512:" + strings.Repeat("a9", 32),
		"sha256:" + strings.Repeat("A9", 32),
		"sha256:" + strings.Repeat("g9", 32),
		strings.Repeat("a9", 32),
	} {
		if ValidDigest(bad) {
			t.Errorf("ValidDigest(%q) = true, want false", bad)
		}
	}
}
