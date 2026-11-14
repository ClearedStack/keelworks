// Package plainforge updates the pinned image digest in app.yaml inside
// Plainforge over cluster-private Git smart HTTP, tagging every commit with
// [skip-ci] so Plainforge does not announce the commit back to Keelworks.
package plainforge

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ClearedStack/keelworks/internal/fault"
	"github.com/ClearedStack/keelworks/internal/registry"
)

// SkipMarker is placed in every commit message Keelworks writes so Plainforge
// never announces the manifest update back to Keelworks.
const SkipMarker = "[skip-ci]"

// DefaultManifestPath is the path of the app manifest inside a repository.
const DefaultManifestPath = "app.yaml"

const (
	mainRef      = "refs/heads/main"
	gitAuthor    = "Keelworks <keelworks@clearedstack.internal>"
	maxRefsBytes = 256 << 10
	maxPackBytes = 16 << 20
	maxBlobBytes = 256 << 10
	maxAttempts  = 3
)

// Status is what happened when Keelworks tried to pin a digest in Plainforge.
type Status int

const (
	// Pinned: app.yaml on refs/heads/main now records the new digest.
	Pinned Status = iota + 1
	// BadManifest: the repository has no refs/heads/main or no valid app.yaml.
	BadManifest
	// TimedOut: Plainforge did not answer in time.
	TimedOut
	// Unavailable: Plainforge was unreachable or a concurrent update raced.
	Unavailable
)

// Pinner writes a verified image digest to app.yaml in Plainforge.
type Pinner interface {
	Pin(ctx context.Context, repository, appSlug, digest string) Status
}

// CommitMessage returns the commit message Keelworks uses when pinning appSlug
// to digest. It always contains [skip-ci].
func CommitMessage(appSlug, digest string) string {
	return "pin " + appSlug + " to " + digest + " " + SkipMarker + "\n"
}

// UpdateAppYAML replaces the top-level "digest:" value in app.yaml with
// newDigest, preserving every other line and comment byte-for-byte. It refuses
// a file that has zero or multiple top-level digest lines, or a malformed one.
func UpdateAppYAML(src []byte, newDigest string) ([]byte, bool, error) {
	if !registry.ValidDigest(newDigest) {
		return nil, false, fault.New(fault.Parse, "digest is malformed")
	}
	if len(src) == 0 || len(src) > maxBlobBytes {
		return nil, false, fault.New(fault.Parse, "app.yaml is empty or too large")
	}
	if bytes.IndexByte(src, 0) >= 0 || bytes.IndexByte(src, '\r') >= 0 {
		return nil, false, fault.New(fault.Parse, "app.yaml has forbidden bytes")
	}

	hadTrailingNL := src[len(src)-1] == '\n'
	text := strings.TrimSuffix(string(src), "\n")
	lines := strings.Split(text, "\n")

	found := -1
	var oldDigest, suffix string
	for i, line := range lines {
		rest, ok := strings.CutPrefix(line, "digest:")
		if !ok {
			continue
		}
		if found >= 0 {
			return nil, false, fault.New(fault.Parse, "app.yaml has more than one digest line")
		}
		found = i
		valPart := rest
		if idx := strings.IndexByte(rest, '#'); idx >= 0 {
			valPart = rest[:idx]
			suffix = rest[idx:]
			// Preserve a single space before a trailing comment when present.
			trimmed := strings.TrimRight(valPart, " \t")
			ws := valPart[len(trimmed):]
			valPart = trimmed
			suffix = ws + suffix
		}
		val := strings.TrimSpace(valPart)
		val = strings.Trim(val, `"'`)
		if !registry.ValidDigest(val) {
			return nil, false, fault.New(fault.Parse, "app.yaml digest line is malformed")
		}
		oldDigest = val
	}
	if found < 0 {
		return nil, false, fault.New(fault.Parse, "app.yaml has no digest line")
	}
	if oldDigest == newDigest {
		return src, false, nil
	}
	lines[found] = "digest: " + newDigest + suffix
	out := strings.Join(lines, "\n")
	if hadTrailingNL || out != "" {
		out += "\n"
	}
	return []byte(out), true, nil
}

// SmartHTTPPinner updates app.yaml over Plainforge's Git smart HTTP protocol.
type SmartHTTPPinner struct {
	base         string
	manifestPath string
	client       *http.Client
	timeout      time.Duration
	now          func() time.Time
}

// NewSmartHTTPPinner returns a Pinner talking to Plainforge at base (for
// example "http://plainforge.clearedstack.svc.cluster.local:8080").
func NewSmartHTTPPinner(base string, client *http.Client, timeout time.Duration) (*SmartHTTPPinner, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("plainforge URL must be http or https with a host")
	}
	if timeout <= 0 {
		return nil, errors.New("no plainforge timeout is configured")
	}
	var c http.Client
	if client != nil {
		c = *client
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &SmartHTTPPinner{
		base:         u.String(),
		manifestPath: DefaultManifestPath,
		client:       &c,
		timeout:      timeout,
		now:          time.Now,
	}, nil
}

var (
	errConflict    = errors.New("ref moved concurrently")
	errBadManifest = errors.New("bad manifest or repository layout")
	errRemote      = errors.New("plainforge error")
)

// Pin fetches refs/heads/main of repository, updates app.yaml with digest, and
// pushes a single fast-forward [skip-ci] commit.
func (p *SmartHTTPPinner) Pin(ctx context.Context, repository, appSlug, digest string) Status {
	if repository == "" || appSlug == "" || !registry.ValidDigest(digest) {
		return BadManifest
	}
	callCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	for attempt := 0; attempt < maxAttempts; attempt++ {
		err := p.pinOnce(callCtx, repository, appSlug, digest)
		if err == nil {
			return Pinned
		}
		if parentOrDeadline(ctx, callCtx) == TimedOut {
			return TimedOut
		}
		if errors.Is(err, errBadManifest) {
			return BadManifest
		}
		if !errors.Is(err, errConflict) {
			return Unavailable
		}
	}
	return Unavailable
}

func (p *SmartHTTPPinner) pinOnce(ctx context.Context, repository, appSlug, digest string) error {
	repoPrefix := p.base + "/" + repository + ".git"

	tipOID, _, err := p.discoverRef(ctx, repoPrefix, "git-upload-pack", mainRef)
	if err != nil {
		return err
	}

	objs, err := p.fetchObjects(ctx, repoPrefix, tipOID)
	if err != nil {
		return err
	}

	commitObj, ok := objs[tipOID]
	if !ok || commitObj.kind != objCommit {
		return errBadManifest
	}
	rootTreeOID, err := parseCommitTree(commitObj.data)
	if err != nil {
		return errBadManifest
	}
	rootTreeObj, ok := objs[rootTreeOID]
	if !ok || rootTreeObj.kind != objTree {
		return errBadManifest
	}
	entries, err := parseTree(rootTreeObj.data)
	if err != nil {
		return errBadManifest
	}

	idx := -1
	for i, e := range entries {
		if e.name == p.manifestPath {
			if e.mode != "100644" {
				return errBadManifest
			}
			idx = i
			break
		}
	}
	if idx < 0 {
		return errBadManifest
	}
	blobObj, ok := objs[entries[idx].oid]
	if !ok || blobObj.kind != objBlob {
		return errBadManifest
	}

	updatedYAML, changed, err := UpdateAppYAML(blobObj.data, digest)
	if err != nil {
		return errBadManifest
	}
	if !changed {
		return nil
	}

	newBlobOID, newBlob := hashGitObject(objBlob, updatedYAML)
	entries[idx].oid = newBlobOID
	newTreeBytes := encodeTree(entries)
	newTreeOID, newTree := hashGitObject(objTree, newTreeBytes)

	ts := strconv.FormatInt(p.now().UTC().Unix(), 10) + " +0000"
	var commitBuf bytes.Buffer
	fmt.Fprintf(&commitBuf, "tree %s\nparent %s\nauthor %s %s\ncommitter %s %s\n\n%s",
		hex.EncodeToString(newTreeOID[:]),
		hex.EncodeToString(tipOID[:]),
		gitAuthor, ts,
		gitAuthor, ts,
		CommitMessage(appSlug, digest),
	)
	newCommitOID, newCommit := hashGitObject(objCommit, commitBuf.Bytes())

	// Check refs/heads/main immediately before receive-pack so a concurrent push
	// fails cleanly and retries against the new tip.
	recvTip, caps, err := p.discoverRef(ctx, repoPrefix, "git-receive-pack", mainRef)
	if err != nil {
		return err
	}
	if recvTip != tipOID {
		return errConflict
	}
	if !containsWord(caps, "report-status") {
		return errRemote
	}

	packBytes, err := buildPack([]gitObject{newBlob, newTree, newCommit})
	if err != nil {
		return errRemote
	}
	return p.pushPack(ctx, repoPrefix, tipOID, newCommitOID, packBytes)
}

func (p *SmartHTTPPinner) discoverRef(ctx context.Context, repoPrefix, service, wantRef string) ([sha1.Size]byte, string, error) {
	var zero [sha1.Size]byte
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, repoPrefix+"/info/refs?service="+service, nil)
	if err != nil {
		return zero, "", errRemote
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return zero, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return zero, "", errBadManifest
	}
	if resp.StatusCode != http.StatusOK {
		return zero, "", errRemote
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRefsBytes+1))
	if err != nil || len(data) > maxRefsBytes {
		return zero, "", errRemote
	}

	r := bytes.NewReader(data)
	line, flush, err := readPktLine(r)
	if err != nil || flush || strings.TrimSuffix(string(line), "\n") != "# service="+service {
		return zero, "", errRemote
	}
	if _, flush, err := readPktLine(r); err != nil || !flush {
		return zero, "", errRemote
	}

	var caps string
	var found [sha1.Size]byte
	var haveFound bool
	for first := true; ; first = false {
		line, flush, err := readPktLine(r)
		if err != nil {
			return zero, "", errRemote
		}
		if flush {
			break
		}
		s := strings.TrimSuffix(string(line), "\n")
		if first {
			if before, after, ok := strings.Cut(s, "\x00"); ok {
				s = before
				caps = after
			}
		}
		hexOID, refName, ok := strings.Cut(s, " ")
		if !ok {
			return zero, "", errRemote
		}
		if refName == wantRef {
			oid, ok := parseOID(hexOID)
			if !ok {
				return zero, "", errRemote
			}
			found = oid
			haveFound = true
		}
	}
	if !haveFound || found == zero {
		return zero, "", errBadManifest
	}
	return found, caps, nil
}

func (p *SmartHTTPPinner) fetchObjects(ctx context.Context, repoPrefix string, want [sha1.Size]byte) (map[[sha1.Size]byte]gitObject, error) {
	var body bytes.Buffer
	writePktString(&body, "want "+hex.EncodeToString(want[:])+" agent=keelworks/v0.1.0-draft\n")
	writePktFlush(&body)
	writePktString(&body, "done\n")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, repoPrefix+"/git-upload-pack", &body)
	if err != nil {
		return nil, errRemote
	}
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Accept", "application/x-git-upload-pack-result")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errRemote
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxPackBytes+1))
	if err != nil || len(raw) > maxPackBytes {
		return nil, errRemote
	}

	r := bytes.NewReader(raw)
	for {
		line, flush, err := readPktLine(r)
		if err != nil || flush {
			return nil, errRemote
		}
		s := strings.TrimSuffix(string(line), "\n")
		if s == "NAK" {
			break
		}
		if !strings.HasPrefix(s, "ACK ") {
			return nil, errRemote
		}
	}
	packBytes, err := io.ReadAll(r)
	if err != nil {
		return nil, errRemote
	}
	return unpackObjects(packBytes)
}

func (p *SmartHTTPPinner) pushPack(ctx context.Context, repoPrefix string, oldOID, newOID [sha1.Size]byte, packBytes []byte) error {
	var body bytes.Buffer
	cmd := fmt.Sprintf("%s %s %s\x00report-status agent=keelworks/v0.1.0-draft\n",
		hex.EncodeToString(oldOID[:]),
		hex.EncodeToString(newOID[:]),
		mainRef,
	)
	writePktString(&body, cmd)
	writePktFlush(&body)
	body.Write(packBytes)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, repoPrefix+"/git-receive-pack", &body)
	if err != nil {
		return errRemote
	}
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	req.Header.Set("Accept", "application/x-git-receive-pack-result")

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errRemote
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRefsBytes+1))
	if err != nil || len(raw) > maxRefsBytes {
		return errRemote
	}
	return checkReceiveStatus(raw)
}

func checkReceiveStatus(raw []byte) error {
	r := bytes.NewReader(raw)
	var unpackOK, refOK bool
	for r.Len() > 0 {
		line, flush, err := readPktLine(r)
		if err != nil {
			return errRemote
		}
		if flush {
			break
		}
		s := strings.TrimSuffix(string(line), "\n")
		switch {
		case s == "unpack ok":
			unpackOK = true
		case s == "ok "+mainRef:
			refOK = true
		case strings.HasPrefix(s, "ng "+mainRef+" "):
			return errConflict
		}
	}
	if !unpackOK || !refOK {
		return errRemote
	}
	return nil
}

func parentOrDeadline(parentCtx, callCtx context.Context) Status {
	if parentCtx.Err() == nil && errors.Is(callCtx.Err(), context.DeadlineExceeded) {
		return TimedOut
	}
	return Unavailable
}

func containsWord(s, want string) bool {
	for _, w := range strings.Fields(s) {
		if w == want {
			return true
		}
	}
	return false
}
