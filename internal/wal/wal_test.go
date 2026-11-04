package wal

import (
	"encoding/binary"
	"testing"
)

const tps = 512

// build creates a synthetic WAL. frames: (pgno, commit, fill byte).
func build(bigEndian bool, salt1, salt2 uint32, frames [][3]uint32) []byte {
	h := make([]byte, HeaderSize)
	magic := uint32(magicLE)
	if bigEndian {
		magic = magicBE
	}
	binary.BigEndian.PutUint32(h[0:], magic)
	binary.BigEndian.PutUint32(h[4:], supportedVer)
	binary.BigEndian.PutUint32(h[8:], tps)
	binary.BigEndian.PutUint32(h[16:], salt1)
	binary.BigEndian.PutUint32(h[20:], salt2)
	s1, s2 := checksum(bigEndian, h[:24], 0, 0)
	binary.BigEndian.PutUint32(h[24:], s1)
	binary.BigEndian.PutUint32(h[28:], s2)
	buf := h
	for _, f := range frames {
		fh := make([]byte, FrameHeaderSize)
		binary.BigEndian.PutUint32(fh[0:], f[0])
		binary.BigEndian.PutUint32(fh[4:], f[1])
		binary.BigEndian.PutUint32(fh[8:], salt1)
		binary.BigEndian.PutUint32(fh[12:], salt2)
		page := make([]byte, tps)
		for i := range page {
			page[i] = byte(f[2])
		}
		s1, s2 = checksum(bigEndian, fh[:8], s1, s2)
		s1, s2 = checksum(bigEndian, page, s1, s2)
		binary.BigEndian.PutUint32(fh[16:], s1)
		binary.BigEndian.PutUint32(fh[20:], s2)
		buf = append(buf, fh...)
		buf = append(buf, page...)
	}
	return buf
}

func pageByte(buf []byte, r *Result, pgno uint32) byte { return buf[r.Pages[pgno]] }

func TestCommittedOnly(t *testing.T) {
	for _, be := range []bool{false, true} {
		buf := build(be, 1, 2, [][3]uint32{
			{1, 0, 0xa1}, {2, 3, 0xa2}, // txn 1, db = 3 pages
			{2, 0, 0xb2}, {3, 4, 0xb3}, // txn 2, db = 4 pages
			{1, 0, 0xc1}, // uncommitted
		})
		r, err := Parse(buf, tps)
		if err != nil {
			t.Fatal(err)
		}
		if r.DBPages != 4 || r.Frames != 4 || r.Uncommitted != 1 {
			t.Fatalf("be=%v got dbpages=%d frames=%d uncommitted=%d", be, r.DBPages, r.Frames, r.Uncommitted)
		}
		if pageByte(buf, r, 1) != 0xa1 || pageByte(buf, r, 2) != 0xb2 || pageByte(buf, r, 3) != 0xb3 {
			t.Fatalf("be=%v wrong page versions", be)
		}
	}
}

func TestStopsAtCorruptFrame(t *testing.T) {
	buf := build(false, 1, 2, [][3]uint32{{1, 1, 0xa1}, {1, 1, 0xb1}})
	buf[HeaderSize+FrameHeaderSize+tps+FrameHeaderSize+10] ^= 0xff // corrupt 2nd frame payload
	r, err := Parse(buf, tps)
	if err != nil {
		t.Fatal(err)
	}
	if r.Frames != 1 || pageByte(buf, r, 1) != 0xa1 {
		t.Fatalf("corrupt frame should be ignored: frames=%d", r.Frames)
	}
}

func TestStaleGenerationIgnored(t *testing.T) {
	// A WAL reset rewrites the header with new salts; leftover frames from
	// the previous generation must not be replayed.
	old := build(false, 1, 2, [][3]uint32{{1, 1, 0xa1}, {2, 2, 0xa2}})
	fresh := build(false, 9, 9, [][3]uint32{{1, 1, 0xf1}})
	buf := append(fresh, old[len(fresh):]...)
	r, err := Parse(buf, tps)
	if err != nil {
		t.Fatal(err)
	}
	if r.Frames != 1 || pageByte(buf, r, 1) != 0xf1 {
		t.Fatalf("stale frames replayed: frames=%d", r.Frames)
	}
	if _, ok := r.Pages[2]; ok {
		t.Fatal("page 2 from old generation must be ignored")
	}
}

func TestTruncatingCommitDropsPages(t *testing.T) {
	buf := build(false, 1, 2, [][3]uint32{{5, 5, 0xa5}, {1, 2, 0xa1}})
	r, err := Parse(buf, tps)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Pages[5]; ok || r.DBPages != 2 {
		t.Fatalf("page beyond committed size kept: dbpages=%d", r.DBPages)
	}
}

func TestBadHeader(t *testing.T) {
	buf := build(false, 1, 2, nil)
	buf[25] ^= 1
	if _, err := Parse(buf, tps); err == nil {
		t.Fatal("expected header checksum error")
	}
	if _, err := Parse(build(false, 1, 2, nil), 4096); err == nil {
		t.Fatal("expected page size error")
	}
	if r, err := Parse(nil, tps); err != nil || len(r.Pages) != 0 {
		t.Fatal("empty WAL should be empty result")
	}
}
