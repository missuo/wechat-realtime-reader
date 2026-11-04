// Package wal parses a SQLite write-ahead log and returns the set of pages
// visible as of the last committed transaction. Frames are not decrypted
// here; their payloads are passed through as-is.
//
// Format reference: https://www.sqlite.org/fileformat.html#the_write_ahead_log
package wal

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	HeaderSize      = 32
	FrameHeaderSize = 24
	magicLE         = 0x377f0682
	magicBE         = 0x377f0683
	supportedVer    = 3007000
)

// Result describes the committed state contained in a WAL.
type Result struct {
	// Pages maps page number → byte offset of the page payload (inside the
	// WAL buffer) for the newest committed frame of that page.
	Pages map[uint32]int
	// DBPages is the database size in pages after the last commit, or 0
	// when there is no committed transaction.
	DBPages uint32
	// Frames is the number of valid committed frames.
	Frames int
	// Salt1/Salt2 identify the WAL generation.
	Salt1, Salt2 uint32
	// Uncommitted counts valid trailing frames not followed by a commit.
	Uncommitted int
}

var ErrBadHeader = errors.New("wal: bad header")

// Parse inspects a WAL buffer. An empty or header-only WAL yields an empty
// result. A corrupt header is an error; frames after the first invalid one
// (stale generation, torn write, bad checksum) are ignored, exactly as
// SQLite recovery would.
func Parse(buf []byte, pageSize int) (*Result, error) {
	r := &Result{Pages: map[uint32]int{}}
	if len(buf) < HeaderSize {
		return r, nil
	}
	magic := binary.BigEndian.Uint32(buf[0:4])
	var nativeBE bool
	switch magic {
	case magicLE:
		nativeBE = false
	case magicBE:
		nativeBE = true
	default:
		return nil, fmt.Errorf("%w: magic %#x", ErrBadHeader, magic)
	}
	if v := binary.BigEndian.Uint32(buf[4:8]); v != supportedVer {
		return nil, fmt.Errorf("%w: version %d", ErrBadHeader, v)
	}
	if ps := int(binary.BigEndian.Uint32(buf[8:12])); ps != pageSize {
		return nil, fmt.Errorf("%w: page size %d, want %d", ErrBadHeader, ps, pageSize)
	}
	r.Salt1 = binary.BigEndian.Uint32(buf[16:20])
	r.Salt2 = binary.BigEndian.Uint32(buf[20:24])

	s1, s2 := checksum(nativeBE, buf[0:24], 0, 0)
	if s1 != binary.BigEndian.Uint32(buf[24:28]) || s2 != binary.BigEndian.Uint32(buf[28:32]) {
		return nil, fmt.Errorf("%w: header checksum", ErrBadHeader)
	}

	frameSize := FrameHeaderSize + pageSize
	pending := map[uint32]int{}
	pendingFrames := 0
	for off := HeaderSize; off+frameSize <= len(buf); off += frameSize {
		fh := buf[off : off+FrameHeaderSize]
		pgno := binary.BigEndian.Uint32(fh[0:4])
		commit := binary.BigEndian.Uint32(fh[4:8])
		if pgno == 0 ||
			binary.BigEndian.Uint32(fh[8:12]) != r.Salt1 ||
			binary.BigEndian.Uint32(fh[12:16]) != r.Salt2 {
			break
		}
		s1, s2 = checksum(nativeBE, fh[0:8], s1, s2)
		s1, s2 = checksum(nativeBE, buf[off+FrameHeaderSize:off+frameSize], s1, s2)
		if s1 != binary.BigEndian.Uint32(fh[16:20]) || s2 != binary.BigEndian.Uint32(fh[20:24]) {
			break
		}
		pending[pgno] = off + FrameHeaderSize
		pendingFrames++
		if commit != 0 {
			for p, o := range pending {
				r.Pages[p] = o
			}
			r.Frames += pendingFrames
			clear(pending)
			pendingFrames = 0
			r.DBPages = commit
		}
	}
	r.Uncommitted = pendingFrames
	// Pages beyond the committed db size are truncated away.
	if r.DBPages > 0 {
		for p := range r.Pages {
			if p > r.DBPages {
				delete(r.Pages, p)
			}
		}
	}
	return r, nil
}

// checksum implements SQLite's WAL checksum over 8-byte words.
func checksum(bigEndian bool, b []byte, s1, s2 uint32) (uint32, uint32) {
	get := binary.LittleEndian.Uint32
	if bigEndian {
		get = binary.BigEndian.Uint32
	}
	for i := 0; i+8 <= len(b); i += 8 {
		s1 += get(b[i:]) + s2
		s2 += get(b[i+4:]) + s1
	}
	return s1, s2
}
