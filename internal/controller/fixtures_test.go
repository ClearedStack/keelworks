package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/keelworks/internal/standin"
)

var update = flag.Bool("update", false, "rewrite the expected files under fixtures/cases")

const fixtures = "../../fixtures"

// plainforgeToken is the made-up token fixtures/standin.json gives Plainforge.
const plainforgeToken = "made-up-token-of-plainforge"

func testConfig() Config {
	return Config{
		KoImage:       "lazarette.clearedstack.svc.cluster.local:5000/made-up-ko-builder@sha256:" + strings.Repeat("aa", 32),
		BuildKitImage: "lazarette.clearedstack.svc.cluster.local:5000/made-up-buildkit-builder@sha256:" + strings.Repeat("bb", 32),
		PlainforgeURL: "http://plainforge.clearedstack.svc.cluster.local:8080",
		ImageRegistry: "lazarette.clearedstack.svc.cluster.local:5000",
		CosignSecret:  "keelworks-cosign",
		PriorityClass: "cs-build-low",
		JobTimeout:    10 * time.Minute,
		ReviewTimeout: time.Second,
		MaxRuns:       8,
	}
}

func script(t *testing.T) standin.Script {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtures, "standin.json"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := standin.ParseScript(data)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestSavedExamples replays every directory under fixtures/cases.
func TestSavedExamples(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(fixtures, "cases"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no saved examples found")
	}
	for _, entry := range entries {
		dir := filepath.Join(fixtures, "cases", entry.Name())
		t.Run(entry.Name(), func(t *testing.T) {
			cfg := testConfig()
			if raw, err := os.ReadFile(filepath.Join(dir, "job-timeout.txt")); err == nil {
				if cfg.JobTimeout, err = time.ParseDuration(strings.TrimSpace(string(raw))); err != nil {
					t.Fatal(err)
				}
			}
			sc := script(t)
			laz := standin.NewLazarette()
			cluster := standin.NewCluster(sc, laz)
			forgeStore := standin.NewForgeStore(sc)
			ctl, err := New(cfg, cluster, standin.NewReviewer(sc), laz, forgeStore)
			if err != nil {
				t.Fatal(err)
			}
			defer ctl.Close()

			body, err := os.ReadFile(filepath.Join(dir, "request.body"))
			if err != nil {
				t.Fatal(err)
			}
			authorization := []string{"Bearer " + plainforgeToken}
			if raw, err := os.ReadFile(filepath.Join(dir, "authorization.txt")); err == nil {
				authorization = strings.FieldsFunc(string(raw), func(r rune) bool { return r == '\n' })
			} else if !errors.Is(err, fs.ErrNotExist) {
				t.Fatal(err)
			}
			status, _, answer := standin.Call(ctl, authorization, body)
			response := fmt.Sprintf("HTTP %d\n\n%s", status, answer)
			check(t, filepath.Join(dir, "response.txt"), []byte(response))

			var job, report, pin []byte
			if status == http.StatusAccepted {
				id := strings.TrimSuffix(strings.SplitN(string(answer), `"run":"`, 2)[1], "\"}\n")
				select {
				case <-ctl.Done(id):
				case <-time.After(30 * time.Second):
					t.Fatal("the run did not end")
				}
				rec := httptest.NewRecorder()
				ctl.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs/"+id, nil))
				if rec.Code != http.StatusOK {
					t.Fatalf("GET /runs/%s: status %d", id, rec.Code)
				}
				report = rec.Body.Bytes()
				created := cluster.Created()
				if len(created) != 1 {
					t.Fatalf("%d Jobs were created, want 1", len(created))
				}
				job = created[0]
				if commits := forgeStore.Commits(); len(commits) > 0 {
					if len(commits) != 1 {
						t.Fatalf("%d Plainforge commits were written, want at most 1", len(commits))
					}
					encoded, err := json.MarshalIndent(commits[0], "", "  ")
					if err != nil {
						t.Fatal(err)
					}
					pin = append(encoded, '\n')
				}
			} else {
				if n := len(cluster.Created()); n != 0 {
					t.Fatalf("%d Jobs were created for a notification that was not accepted", n)
				}
				if n := len(forgeStore.Commits()); n != 0 {
					t.Fatalf("%d Plainforge commits were written for a notification that was not accepted", n)
				}
			}
			if n := cluster.Live(); n != 0 {
				t.Fatalf("%d Jobs were left behind", n)
			}
			check(t, filepath.Join(dir, "job.json"), job)
			check(t, filepath.Join(dir, "run.json"), report)
			check(t, filepath.Join(dir, "pin.json"), pin)
		})
	}
}

func check(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if got == nil {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Fatal(err)
			}
			return
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		if got != nil {
			t.Errorf("%s is not saved, but the output was:\n%s", path, got)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Errorf("%s is saved, but nothing was produced", path)
		return
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs\n--- got ---\n%s--- want ---\n%s", path, got, want)
	}
}
