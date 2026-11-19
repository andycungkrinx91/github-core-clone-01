package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
)

const (
	packObjCommit   = 1
	packObjTree     = 2
	packObjBlob     = 3
	packObjTag      = 4
	packObjOfsDelta = 6
	packObjRefDelta = 7
)

var typeCodeToName = map[int]string{
	packObjCommit: "commit",
	packObjTree:   "tree",
	packObjBlob:   "blob",
	packObjTag:    "tag",
}

var typeNameToCode = map[string]int{
	"commit": packObjCommit,
	"tree":   packObjTree,
	"blob":   packObjBlob,
	"tag":    packObjTag,
}

var zlibWriterPool = sync.Pool{
	New: func() any {
		w, _ := zlib.NewWriterLevel(io.Discard, zlib.BestSpeed)
		return w
	},
}

var inflateBufPool = sync.Pool{
	New: func() any {
		return bytes.NewBuffer(make([]byte, 0, 4096))
	},
}

const maxDeltaTargetBytes = 64 << 20

// Minimal valid empty zlib stream used to seed pooled readers.
var emptyZlibStream = func() []byte {
	var b bytes.Buffer
	w := zlib.NewWriter(&b)
	_ = w.Close()
	return b.Bytes()
}()

var zlibReaderPool = sync.Pool{
	New: func() any {
		r, _ := zlib.NewReader(bytes.NewReader(emptyZlibStream))
		return r
	},
}

// sliceByteReader implements io.Reader and io.ByteReader over a byte slice
// so compress/zlib does not wrap it in a bufio.Reader and stops at the exact end of each stream.
type sliceByteReader struct {
	data []byte
	pos  int
}

func (s *sliceByteReader) Read(p []byte) (int, error) {
	if s.pos >= len(s.data) {
		return 0, io.EOF
	}
	n := copy(p, s.data[s.pos:])
	s.pos += n
	return n, nil
}

func (s *sliceByteReader) ReadByte() (byte, error) {
	if s.pos >= len(s.data) {
		return 0, io.EOF
	}
	b := s.data[s.pos]
	s.pos++
	return b, nil
}

func applyGitDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarInt := func() (int, error) {
		val := 0
		shift := 0
		for {
			if pos >= len(delta) {
				return 0, fmt.Errorf("truncated delta varint")
			}
			b := delta[pos]
			pos++
			val |= int(b&0x7f) << shift
			if b&0x80 == 0 {
				return val, nil
			}
			shift += 7
			if shift > 35 {
				return 0, fmt.Errorf("delta varint overflow")
			}
		}
	}

	srcSize, err := readVarInt()
	if err != nil {
		return nil, err
	}
	if srcSize != len(base) {
		return nil, fmt.Errorf("delta source size mismatch: expected %d, got %d", srcSize, len(base))
	}
	dstSize, err := readVarInt()
	if err != nil {
		return nil, err
	}
	if dstSize < 0 || dstSize > maxDeltaTargetBytes {
		return nil, fmt.Errorf("delta target size out of bounds: %d", dstSize)
	}

	out := make([]byte, 0, dstSize)
	for pos < len(delta) {
		cmd := delta[pos]
		pos++
		if cmd&0x80 != 0 {
			var cpOff, cpSize int
			if cmd&0x01 != 0 {
				if pos >= len(delta) {
					return nil, fmt.Errorf("truncated delta copy offset")
				}
				cpOff |= int(delta[pos])
				pos++
			}
			if cmd&0x02 != 0 {
				if pos >= len(delta) {
					return nil, fmt.Errorf("truncated delta copy offset")
				}
				cpOff |= int(delta[pos]) << 8
				pos++
			}
			if cmd&0x04 != 0 {
				if pos >= len(delta) {
					return nil, fmt.Errorf("truncated delta copy offset")
				}
				cpOff |= int(delta[pos]) << 16
				pos++
			}
			if cmd&0x08 != 0 {
				if pos >= len(delta) {
					return nil, fmt.Errorf("truncated delta copy offset")
				}
				cpOff |= int(delta[pos]) << 24
				pos++
			}
			if cmd&0x10 != 0 {
				if pos >= len(delta) {
					return nil, fmt.Errorf("truncated delta copy size")
				}
				cpSize |= int(delta[pos])
				pos++
			}
			if cmd&0x20 != 0 {
				if pos >= len(delta) {
					return nil, fmt.Errorf("truncated delta copy size")
				}
				cpSize |= int(delta[pos]) << 8
				pos++
			}
			if cmd&0x40 != 0 {
				if pos >= len(delta) {
					return nil, fmt.Errorf("truncated delta copy size")
				}
				cpSize |= int(delta[pos]) << 16
				pos++
			}
			if cpSize == 0 {
				cpSize = 0x10000
			}
			if cpOff < 0 || cpOff+cpSize > len(base) {
				return nil, fmt.Errorf("delta copy out of bounds: %d+%d > %d", cpOff, cpSize, len(base))
			}
			out = append(out, base[cpOff:cpOff+cpSize]...)
		} else if cmd > 0 {
			n := int(cmd)
			if pos+n > len(delta) {
				return nil, fmt.Errorf("delta insert out of bounds")
			}
			out = append(out, delta[pos:pos+n]...)
			pos += n
		} else {
			return nil, fmt.Errorf("invalid delta opcode 0")
		}
	}
	if len(out) != dstSize {
		return nil, fmt.Errorf("delta result size mismatch: expected %d, got %d", dstSize, len(out))
	}
	return out, nil
}

type pendingRefDelta struct {
	offset  int
	baseOID string
	delta   []byte
}

type packEntryHeader struct {
	objType int
	size    int
	ofsBase int
	refBase string
	nextPos int
}

func readPackEntryHeader(pack []byte, pos, payloadEnd int) (packEntryHeader, error) {
	entryOffset := pos
	c := pack[pos]
	pos++
	objType := int((c >> 4) & 0x07)
	size := int(c & 0x0f)
	shift := 4
	for c&0x80 != 0 {
		if pos >= payloadEnd {
			return packEntryHeader{}, fmt.Errorf("truncated object header")
		}
		c = pack[pos]
		pos++
		size |= int(c&0x7f) << shift
		shift += 7
	}

	var ofsBase int
	var refBase string
	switch objType {
	case packObjOfsDelta:
		if pos >= payloadEnd {
			return packEntryHeader{}, fmt.Errorf("truncated ofs-delta header")
		}
		b := pack[pos]
		pos++
		rel := int(b & 0x7f)
		for b&0x80 != 0 {
			if pos >= payloadEnd {
				return packEntryHeader{}, fmt.Errorf("truncated ofs-delta varint")
			}
			b = pack[pos]
			pos++
			rel = ((rel + 1) << 7) | int(b&0x7f)
		}
		ofsBase = entryOffset - rel
		if ofsBase < 12 {
			return packEntryHeader{}, fmt.Errorf("invalid ofs-delta base offset %d", ofsBase)
		}
	case packObjRefDelta:
		if pos+20 > payloadEnd {
			return packEntryHeader{}, fmt.Errorf("truncated ref-delta base oid")
		}
		refBase = hex.EncodeToString(pack[pos : pos+20])
		pos += 20
	case packObjCommit, packObjTree, packObjBlob, packObjTag:
	default:
		return packEntryHeader{}, fmt.Errorf("unknown pack object type %d", objType)
	}
	return packEntryHeader{
		objType: objType,
		size:    size,
		ofsBase: ofsBase,
		refBase: refBase,
		nextPos: pos,
	}, nil
}

func resolvePendingRefDeltasLocked(repo *Repository, byOffset map[int]GitObject, refDeltas []pendingRefDelta) error {
	for len(refDeltas) > 0 {
		progressed := false
		rem := refDeltas[:0]
		for _, rd := range refDeltas {
			baseObj, ok := repo.Objects[rd.baseOID]
			if !ok {
				rem = append(rem, rd)
				continue
			}
			resolved, err := applyGitDelta(baseObj.Data, rd.delta)
			if err != nil {
				return err
			}
			obj := GitObject{Type: baseObj.Type, Data: resolved}
			oid := hashGitObject(obj.Type, obj.Data)
			repo.Objects[oid] = obj
			byOffset[rd.offset] = obj
			progressed = true
		}
		if !progressed {
			return fmt.Errorf("unresolved ref-delta base %s", rem[0].baseOID)
		}
		refDeltas = rem
	}
	return nil
}

func unpackPackfileLocked(repo *Repository, pack []byte) error {
	if len(pack) == 0 {
		return nil
	}
	if len(pack) < 32 {
		return fmt.Errorf("packfile too short (%d bytes)", len(pack))
	}
	if string(pack[:4]) != "PACK" {
		return fmt.Errorf("invalid packfile header")
	}
	version := binary.BigEndian.Uint32(pack[4:8])
	if version != 2 && version != 3 {
		return fmt.Errorf("unsupported pack version %d", version)
	}
	numObjs := int(binary.BigEndian.Uint32(pack[8:12]))

	payloadEnd := len(pack) - 20
	sum := sha1.Sum(pack[:payloadEnd])
	if !bytes.Equal(sum[:], pack[payloadEnd:]) {
		return fmt.Errorf("pack checksum mismatch")
	}

	byOffset := make(map[int]GitObject, numObjs)
	var refDeltas []pendingRefDelta

	pos := 12
	zr := zlibReaderPool.Get().(io.ReadCloser)
	defer zlibReaderPool.Put(zr)
	inflateBuf := inflateBufPool.Get().(*bytes.Buffer)
	defer func() {
		if inflateBuf.Cap() <= 1<<20 {
			inflateBuf.Reset()
			inflateBufPool.Put(inflateBuf)
		}
	}()

	for i := 0; i < numObjs; i++ {
		if pos >= payloadEnd {
			return fmt.Errorf("packfile truncated at object %d/%d", i, numObjs)
		}
		entryOffset := pos
		hdr, err := readPackEntryHeader(pack, pos, payloadEnd)
		if err != nil {
			return err
		}

		sr := &sliceByteReader{data: pack[:payloadEnd], pos: hdr.nextPos}
		if err := zr.(zlib.Resetter).Reset(sr, nil); err != nil {
			return fmt.Errorf("zlib init error at object %d: %w", i, err)
		}
		inflateBuf.Reset()
		if hdr.size > 0 && hdr.size <= maxDeltaTargetBytes {
			inflateBuf.Grow(hdr.size)
		}
		if _, err := io.Copy(inflateBuf, zr); err != nil {
			return fmt.Errorf("zlib inflate error at object %d: %w", i, err)
		}
		pos = sr.pos
		raw := make([]byte, inflateBuf.Len())
		copy(raw, inflateBuf.Bytes())

		switch hdr.objType {
		case packObjCommit, packObjTree, packObjBlob, packObjTag:
			obj := GitObject{Type: typeCodeToName[hdr.objType], Data: raw}
			repo.Objects[hashGitObject(obj.Type, obj.Data)] = obj
			byOffset[entryOffset] = obj
		case packObjOfsDelta:
			baseObj, ok := byOffset[hdr.ofsBase]
			if !ok {
				return fmt.Errorf("missing ofs-delta base at offset %d", hdr.ofsBase)
			}
			resolved, err := applyGitDelta(baseObj.Data, raw)
			if err != nil {
				return err
			}
			obj := GitObject{Type: baseObj.Type, Data: resolved}
			repo.Objects[hashGitObject(obj.Type, obj.Data)] = obj
			byOffset[entryOffset] = obj
		case packObjRefDelta:
			if baseObj, ok := repo.Objects[hdr.refBase]; ok {
				resolved, err := applyGitDelta(baseObj.Data, raw)
				if err != nil {
					return err
				}
				obj := GitObject{Type: baseObj.Type, Data: resolved}
				repo.Objects[hashGitObject(obj.Type, obj.Data)] = obj
				byOffset[entryOffset] = obj
			} else {
				refDeltas = append(refDeltas, pendingRefDelta{offset: entryOffset, baseOID: hdr.refBase, delta: raw})
			}
		}
	}

	return resolvePendingRefDeltasLocked(repo, byOffset, refDeltas)
}

func buildPackfile(objects []GitObject) []byte {
	var buf bytes.Buffer
	buf.Grow(32 + len(objects)*64)

	var hdr [12]byte
	copy(hdr[0:4], "PACK")
	binary.BigEndian.PutUint32(hdr[4:8], 2)
	binary.BigEndian.PutUint32(hdr[8:12], uint32(len(objects)))
	buf.Write(hdr[:])

	zw := zlibWriterPool.Get().(*zlib.Writer)
	defer zlibWriterPool.Put(zw)

	for _, obj := range objects {
		code := typeNameToCode[obj.Type]
		size := len(obj.Data)
		first := byte((code << 4) | (size & 0x0f))
		size >>= 4
		if size > 0 {
			first |= 0x80
		}
		buf.WriteByte(first)
		for size > 0 {
			b := byte(size & 0x7f)
			size >>= 7
			if size > 0 {
				b |= 0x80
			}
			buf.WriteByte(b)
		}
		zw.Reset(&buf)
		_, _ = zw.Write(obj.Data)
		_ = zw.Close()
	}

	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

// Pkt-line helpers.
const hexDigits = "0123456789abcdef"

func appendPktLine(dst []byte, payload []byte) []byte {
	n := len(payload) + 4
	dst = append(dst,
		hexDigits[(n>>12)&0xf],
		hexDigits[(n>>8)&0xf],
		hexDigits[(n>>4)&0xf],
		hexDigits[n&0xf],
	)
	return append(dst, payload...)
}

func appendPktString(dst []byte, s string) []byte {
	n := len(s) + 4
	dst = append(dst,
		hexDigits[(n>>12)&0xf],
		hexDigits[(n>>8)&0xf],
		hexDigits[(n>>4)&0xf],
		hexDigits[n&0xf],
	)
	return append(dst, s...)
}

func appendSidebandPack(dst []byte, pack []byte) []byte {
	const maxChunk = 65515 // 65520 - 4 (len) - 1 (band)
	for off := 0; off < len(pack); off += maxChunk {
		end := off + maxChunk
		if end > len(pack) {
			end = len(pack)
		}
		chunk := pack[off:end]
		n := len(chunk) + 5
		dst = append(dst,
			hexDigits[(n>>12)&0xf],
			hexDigits[(n>>8)&0xf],
			hexDigits[(n>>4)&0xf],
			hexDigits[n&0xf],
			1,
		)
		dst = append(dst, chunk...)
	}
	return dst
}

type pktItem struct {
	Kind string // "data", "flush", "delim", "response-end"
	Data []byte
}

// parsePktStream parses pkt-lines until EOF or stopAtFlush (when raw pack data follows flush).
func parsePktStream(body []byte, stopAtFlush bool) ([]pktItem, []byte, error) {
	items := make([]pktItem, 0, 16)
	pos := 0
	for pos < len(body) {
		if pos+4 > len(body) {
			return nil, nil, fmt.Errorf("truncated pkt-line header")
		}
		length, err := parseHex4(body[pos : pos+4])
		if err != nil {
			return nil, nil, err
		}
		pos += 4
		switch length {
		case 0:
			items = append(items, pktItem{Kind: "flush"})
			if stopAtFlush {
				return items, body[pos:], nil
			}
		case 1:
			items = append(items, pktItem{Kind: "delim"})
		case 2:
			items = append(items, pktItem{Kind: "response-end"})
		case 3:
			return nil, nil, fmt.Errorf("invalid pkt-line length 0003")
		default:
			if length > 65520 {
				return nil, nil, fmt.Errorf("pkt-line length %d exceeds max", length)
			}
			payloadLen := length - 4
			if pos+payloadLen > len(body) {
				return nil, nil, fmt.Errorf("truncated pkt-line payload")
			}
			items = append(items, pktItem{Kind: "data", Data: body[pos : pos+payloadLen]})
			pos += payloadLen
		}
	}
	return items, nil, nil
}

func parseHex4(b []byte) (int, error) {
	v := 0
	for i := 0; i < 4; i++ {
		c := b[i]
		var d int
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'a' && c <= 'f':
			d = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = int(c-'A') + 10
		default:
			return 0, fmt.Errorf("invalid pkt-line hex byte %q", c)
		}
		v = (v << 4) | d
	}
	return v, nil
}
