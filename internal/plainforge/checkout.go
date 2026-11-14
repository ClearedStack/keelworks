package plainforge

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/ClearedStack/keelworks/internal/fault"
)

const (
	maxCheckoutFiles = 4096
	maxCheckoutBytes = 64 << 20
	maxTreeDepth     = 16
	maxTagDepth      = 16
)

// Checkout fetches commitHex of repository from Plainforge over Git smart HTTP
// and writes its working tree into destDir, which must not yet exist or must be
// empty. Symlinks, submodules, .git entries, and path-traversal names are
// refused rather than written.
func (p *SmartHTTPPinner) Checkout(ctx context.Context, repository, ref, commitHex, destDir string) error {
	if repository == "" || destDir == "" || !validCheckoutRef(ref) || !isLowerHex40(commitHex) {
		return fault.New(fault.Parse, "checkout parameters are invalid")
	}
	commitOID, ok := parseOID(commitHex)
	if !ok {
		return fault.New(fault.Parse, "checkout commit is malformed")
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return errors.New("checkout directory could not be created")
	}
	existing, err := os.ReadDir(destDir)
	if err != nil {
		return errors.New("checkout directory could not be read")
	}
	if len(existing) != 0 {
		return errors.New("checkout directory is not empty")
	}

	callCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	repoPrefix := p.base + "/" + repository + ".git"
	tips, byRef, err := p.discoverAllRefs(callCtx, repoPrefix)
	if err != nil {
		if parentOrDeadline(ctx, callCtx) == TimedOut {
			return fault.New(fault.Timeout, "Plainforge checkout timed out")
		}
		return err
	}

	var wantOID [sha1.Size]byte
	switch {
	case tips[commitOID]:
		wantOID = commitOID
	case byRef[ref] != ([sha1.Size]byte{}):
		wantOID = byRef[ref]
	default:
		return fault.New(fault.NeedsSubtool, "commit is not reachable from the advertised ref")
	}

	objs, err := p.fetchObjects(callCtx, repoPrefix, wantOID)
	if err != nil {
		if parentOrDeadline(ctx, callCtx) == TimedOut {
			return fault.New(fault.Timeout, "Plainforge checkout timed out")
		}
		return errors.New("Plainforge pack could not be fetched")
	}

	rootTreeOID, err := resolveCommitTree(objs, commitOID)
	if err != nil {
		return err
	}

	var st checkoutState
	return materializeTree(objs, rootTreeOID, destDir, 0, &st)
}

func (p *SmartHTTPPinner) discoverAllRefs(ctx context.Context, repoPrefix string) (map[[sha1.Size]byte]bool, map[string][sha1.Size]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, repoPrefix+"/info/refs?service=git-upload-pack", nil)
	if err != nil {
		return nil, nil, errRemote
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil, errors.New("repository does not exist in Plainforge")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, errRemote
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRefsBytes+1))
	if err != nil || len(data) > maxRefsBytes {
		return nil, nil, errRemote
	}

	r := bytes.NewReader(data)
	line, flush, err := readPktLine(r)
	if err != nil || flush || strings.TrimSuffix(string(line), "\n") != "# service=git-upload-pack" {
		return nil, nil, errRemote
	}
	if _, flush, err := readPktLine(r); err != nil || !flush {
		return nil, nil, errRemote
	}

	tips := make(map[[sha1.Size]byte]bool)
	byRef := make(map[string][sha1.Size]byte)
	var zero [sha1.Size]byte
	for first := true; ; first = false {
		line, flush, err := readPktLine(r)
		if err != nil {
			return nil, nil, errRemote
		}
		if flush {
			break
		}
		s := strings.TrimSuffix(string(line), "\n")
		if first {
			if before, _, hasNul := strings.Cut(s, "\x00"); hasNul {
				s = before
			}
		}
		hexOID, refName, ok := strings.Cut(s, " ")
		if !ok {
			return nil, nil, errRemote
		}
		oid, ok := parseOID(hexOID)
		if !ok {
			return nil, nil, errRemote
		}
		if oid == zero || strings.HasSuffix(refName, "^{}") {
			continue
		}
		if strings.HasPrefix(refName, "refs/") {
			tips[oid] = true
			byRef[refName] = oid
		}
	}
	return tips, byRef, nil
}

func resolveCommitTree(objs map[[sha1.Size]byte]gitObject, startOID [sha1.Size]byte) ([sha1.Size]byte, error) {
	var zero [sha1.Size]byte
	cur := startOID
	for depth := 0; depth <= maxTagDepth; depth++ {
		obj, ok := objs[cur]
		if !ok {
			return zero, errors.New("requested commit was not in the pack")
		}
		switch obj.kind {
		case objCommit:
			treeOID, err := parseCommitTree(obj.data)
			if err != nil {
				return zero, errors.New("commit object is malformed")
			}
			return treeOID, nil
		case objTag:
			targetOID, err := parseTagTarget(obj.data)
			if err != nil {
				return zero, err
			}
			cur = targetOID
		default:
			return zero, errors.New("requested object is neither a commit nor a tag")
		}
	}
	return zero, fault.New(fault.NeedsSubtool, "annotated tag is nested too deeply")
}

func parseTagTarget(raw []byte) ([sha1.Size]byte, error) {
	var zero [sha1.Size]byte
	line, _, ok := strings.Cut(string(raw), "\n")
	if !ok {
		return zero, errors.New("tag object is malformed")
	}
	hexOID, ok := strings.CutPrefix(line, "object ")
	if !ok {
		return zero, errors.New("tag object is missing its target")
	}
	oid, ok := parseOID(hexOID)
	if !ok {
		return zero, errors.New("tag target OID is malformed")
	}
	return oid, nil
}

type checkoutState struct {
	files int
	bytes int64
}

func materializeTree(objs map[[sha1.Size]byte]gitObject, treeOID [sha1.Size]byte, dir string, depth int, st *checkoutState) error {
	if depth > maxTreeDepth {
		return fault.New(fault.NeedsSubtool, "repository tree is nested too deeply")
	}
	treeObj, ok := objs[treeOID]
	if !ok || treeObj.kind != objTree {
		return errors.New("tree object is missing from the pack")
	}
	entries, err := parseTree(treeObj.data)
	if err != nil {
		return errors.New("tree object is malformed")
	}
	for _, e := range entries {
		if !validEntryName(e.name) {
			return fault.New(fault.Parse, "repository tree contains an unsafe path name")
		}
		dst := filepath.Join(dir, e.name)
		switch e.mode {
		case "40000", "040000":
			if err := os.Mkdir(dst, 0o755); err != nil {
				return errors.New("subdirectory could not be created")
			}
			if err := materializeTree(objs, e.oid, dst, depth+1, st); err != nil {
				return err
			}
		case "100644", "100755":
			st.files++
			if st.files > maxCheckoutFiles {
				return fault.New(fault.NeedsSubtool, "repository has too many files to check out")
			}
			blobObj, ok := objs[e.oid]
			if !ok || blobObj.kind != objBlob {
				return errors.New("blob object is missing from the pack")
			}
			st.bytes += int64(len(blobObj.data))
			if st.bytes > maxCheckoutBytes {
				return fault.New(fault.NeedsSubtool, "repository working tree is too large")
			}
			perm := os.FileMode(0o644)
			if e.mode == "100755" {
				perm = 0o755
			}
			if err := writeNewFile(dst, blobObj.data, perm); err != nil {
				return err
			}
		default:
			return fault.New(fault.NeedsSubtool, "repository tree contains an unsupported entry mode")
		}
	}
	return nil
}

func writeNewFile(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return errors.New("file could not be created in checkout directory")
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		return errors.New("file could not be written in checkout directory")
	}
	return nil
}

func validCheckoutRef(ref string) bool {
	return strings.HasPrefix(ref, "refs/heads/") || strings.HasPrefix(ref, "refs/tags/")
}

func isLowerHex40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func validEntryName(name string) bool {
	if name == "" || name == "." || name == ".." || name == ".git" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == 0 || c == '/' || c == '\\' {
			return false
		}
	}
	return true
}
