package plainforge

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
)

type objKind int

const (
	objCommit   objKind = 1
	objTree     objKind = 2
	objBlob     objKind = 3
	objTag      objKind = 4
	objOfsDelta objKind = 6
	objRefDelta objKind = 7
)

func (k objKind) name() string {
	switch k {
	case objCommit:
		return "commit"
	case objTree:
		return "tree"
	case objBlob:
		return "blob"
	case objTag:
		return "tag"
	}
	return ""
}

type gitObject struct {
	kind objKind
	data []byte
}

func hashGitObject(kind objKind, data []byte) ([sha1.Size]byte, gitObject) {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", kind.name(), len(data))
	h.Write(data)
	var oid [sha1.Size]byte
	copy(oid[:], h.Sum(nil))
	cp := append([]byte(nil), data...)
	return oid, gitObject{kind: kind, data: cp}
}

func parseOID(s string) ([sha1.Size]byte, bool) {
	var oid [sha1.Size]byte
	if len(s) != 40 {
		return oid, false
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return oid, false
	}
	copy(oid[:], b)
	return oid, true
}

func writePktString(w *bytes.Buffer, s string) {
	fmt.Fprintf(w, "%04x%s", len(s)+4, s)
}

func writePktFlush(w *bytes.Buffer) {
	w.WriteString("0000")
}

func readPktLine(r io.Reader) ([]byte, bool, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, false, err
	}
	n, err := strconv.ParseUint(string(hdr[:]), 16, 16)
	if err != nil {
		return nil, false, errRemote
	}
	if n == 0 {
		return nil, true, nil
	}
	if n < 4 {
		return nil, false, errRemote
	}
	buf := make([]byte, n-4)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, false, err
	}
	return buf, false, nil
}

type treeEntry struct {
	mode string
	name string
	oid  [sha1.Size]byte
}

func parseCommitTree(commit []byte) ([sha1.Size]byte, error) {
	var zero [sha1.Size]byte
	line, _, ok := strings.Cut(string(commit), "\n")
	if !ok {
		return zero, errBadManifest
	}
	hexOID, ok := strings.CutPrefix(line, "tree ")
	if !ok {
		return zero, errBadManifest
	}
	oid, ok := parseOID(hexOID)
	if !ok {
		return zero, errBadManifest
	}
	return oid, nil
}

func parseTree(raw []byte) ([]treeEntry, error) {
	var entries []treeEntry
	for len(raw) > 0 {
		nul := bytes.IndexByte(raw, 0)
		if nul < 0 || len(raw) < nul+1+sha1.Size {
			return nil, errBadManifest
		}
		mode, name, ok := strings.Cut(string(raw[:nul]), " ")
		if !ok || mode == "" || name == "" {
			return nil, errBadManifest
		}
		var oid [sha1.Size]byte
		copy(oid[:], raw[nul+1:nul+1+sha1.Size])
		entries = append(entries, treeEntry{mode: mode, name: name, oid: oid})
		raw = raw[nul+1+sha1.Size:]
	}
	return entries, nil
}

func encodeTree(entries []treeEntry) []byte {
	var out bytes.Buffer
	for _, e := range entries {
		out.WriteString(e.mode)
		out.WriteByte(' ')
		out.WriteString(e.name)
		out.WriteByte(0)
		out.Write(e.oid[:])
	}
	return out.Bytes()
}

func unpackObjects(pack []byte) (map[[sha1.Size]byte]gitObject, error) {
	if len(pack) < 12+sha1.Size || string(pack[:4]) != "PACK" {
		return nil, errRemote
	}
	sum := sha1.Sum(pack[:len(pack)-sha1.Size])
	if !bytes.Equal(sum[:], pack[len(pack)-sha1.Size:]) {
		return nil, errRemote
	}
	version := binary.BigEndian.Uint32(pack[4:8])
	count := binary.BigEndian.Uint32(pack[8:12])
	if version != 2 || count > 16384 {
		return nil, errRemote
	}

	body := pack[:len(pack)-sha1.Size]
	r := bytes.NewReader(body[12:])

	type pendingDelta struct {
		offset  int64
		kind    objKind
		baseOID [sha1.Size]byte
		baseOff int64
		delta   []byte
	}

	byOID := make(map[[sha1.Size]byte]gitObject, count)
	byOff := make(map[int64]gitObject, count)
	var pending []pendingDelta

	for i := uint32(0); i < count; i++ {
		entryOff := 12 + (int64(len(body)-12) - int64(r.Len()))
		b, err := r.ReadByte()
		if err != nil {
			return nil, errRemote
		}
		kind := objKind((b >> 4) & 7)
		size := uint64(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			if shift > 56 {
				return nil, errRemote
			}
			b, err = r.ReadByte()
			if err != nil {
				return nil, errRemote
			}
			size |= uint64(b&0x7f) << shift
			shift += 7
		}
		if size > maxPackBytes {
			return nil, errRemote
		}

		var baseOID [sha1.Size]byte
		var baseOff int64
		switch kind {
		case objCommit, objTree, objBlob, objTag:
		case objOfsDelta:
			c, err := r.ReadByte()
			if err != nil {
				return nil, errRemote
			}
			dist := int64(c & 0x7f)
			for c&0x80 != 0 {
				c, err = r.ReadByte()
				if err != nil {
					return nil, errRemote
				}
				dist = ((dist + 1) << 7) | int64(c&0x7f)
			}
			if dist <= 0 || dist > entryOff {
				return nil, errRemote
			}
			baseOff = entryOff - dist
		case objRefDelta:
			if _, err := io.ReadFull(r, baseOID[:]); err != nil {
				return nil, errRemote
			}
		default:
			return nil, errRemote
		}

		zr, err := zlib.NewReader(r)
		if err != nil {
			return nil, errRemote
		}
		inflated, err := io.ReadAll(io.LimitReader(zr, int64(size)+1))
		_ = zr.Close()
		if err != nil || uint64(len(inflated)) != size {
			return nil, errRemote
		}

		if kind == objOfsDelta || kind == objRefDelta {
			pending = append(pending, pendingDelta{
				offset:  entryOff,
				kind:    kind,
				baseOID: baseOID,
				baseOff: baseOff,
				delta:   inflated,
			})
			continue
		}
		oid, obj := hashGitObject(kind, inflated)
		byOID[oid] = obj
		byOff[entryOff] = obj
	}
	if r.Len() != 0 {
		return nil, errRemote
	}

	// Resolve any deltas in order (looping until fixed point for delta chains).
	for len(pending) > 0 {
		progressed := false
		var rest []pendingDelta
		for _, pd := range pending {
			var base gitObject
			var ok bool
			if pd.kind == objOfsDelta {
				base, ok = byOff[pd.baseOff]
			} else {
				base, ok = byOID[pd.baseOID]
			}
			if !ok {
				rest = append(rest, pd)
				continue
			}
			resolved, err := applyDelta(base.data, pd.delta)
			if err != nil {
				return nil, errRemote
			}
			oid, obj := hashGitObject(base.kind, resolved)
			byOID[oid] = obj
			byOff[pd.offset] = obj
			progressed = true
		}
		if !progressed {
			return nil, errRemote
		}
		pending = rest
	}
	return byOID, nil
}

func applyDelta(base, delta []byte) ([]byte, error) {
	r := bytes.NewReader(delta)
	srcSize, err := readDeltaVarint(r)
	if err != nil || srcSize != uint64(len(base)) {
		return nil, errRemote
	}
	dstSize, err := readDeltaVarint(r)
	if err != nil || dstSize > maxPackBytes {
		return nil, errRemote
	}
	out := make([]byte, 0, dstSize)
	for r.Len() > 0 {
		cmd, err := r.ReadByte()
		if err != nil {
			return nil, errRemote
		}
		switch {
		case cmd&0x80 != 0:
			var cpOff, cpSize uint32
			for i := uint(0); i < 4; i++ {
				if cmd&(1<<i) != 0 {
					b, err := r.ReadByte()
					if err != nil {
						return nil, errRemote
					}
					cpOff |= uint32(b) << (8 * i)
				}
			}
			for i := uint(0); i < 3; i++ {
				if cmd&(0x10<<i) != 0 {
					b, err := r.ReadByte()
					if err != nil {
						return nil, errRemote
					}
					cpSize |= uint32(b) << (8 * i)
				}
			}
			if cpSize == 0 {
				cpSize = 0x10000
			}
			if uint64(cpOff)+uint64(cpSize) > uint64(len(base)) || uint64(len(out))+uint64(cpSize) > dstSize {
				return nil, errRemote
			}
			out = append(out, base[cpOff:cpOff+cpSize]...)
		case cmd > 0:
			if uint64(len(out))+uint64(cmd) > dstSize || r.Len() < int(cmd) {
				return nil, errRemote
			}
			chunk := make([]byte, cmd)
			if _, err := io.ReadFull(r, chunk); err != nil {
				return nil, errRemote
			}
			out = append(out, chunk...)
		default:
			return nil, errRemote
		}
	}
	if uint64(len(out)) != dstSize {
		return nil, errRemote
	}
	return out, nil
}

func readDeltaVarint(r *bytes.Reader) (uint64, error) {
	var val uint64
	var shift uint
	for {
		if shift > 56 {
			return 0, errRemote
		}
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		val |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return val, nil
		}
		shift += 7
	}
}

func buildPack(objs []gitObject) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("PACK")
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], 2)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(objs)))
	buf.Write(hdr[:])

	for _, o := range objs {
		size := uint64(len(o.data))
		first := byte((o.kind&7)<<4) | byte(size&0x0f)
		size >>= 4
		for size > 0 {
			buf.WriteByte(first | 0x80)
			first = byte(size & 0x7f)
			size >>= 7
		}
		buf.WriteByte(first)

		zw := zlib.NewWriter(&buf)
		if _, err := zw.Write(o.data); err != nil {
			return nil, err
		}
		if err := zw.Close(); err != nil {
			return nil, err
		}
	}

	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes(), nil
}
