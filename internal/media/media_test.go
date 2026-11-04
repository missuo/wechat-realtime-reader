package media

import (
	"bytes"
	"crypto/aes"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// encodeV2 builds a synthetic V2 .dat file (test-only).
func encodeV2(plain []byte, aesKey []byte, xor byte, aesLen, xorLen int) []byte {
	head := plain[:aesLen]
	pad := 16 - len(head)%16
	padded := append(bytes.Clone(head), bytes.Repeat([]byte{byte(pad)}, pad)...)
	c, _ := aes.NewCipher(aesKey)
	enc := make([]byte, len(padded))
	for i := 0; i < len(padded); i += 16 {
		c.Encrypt(enc[i:i+16], padded[i:i+16])
	}
	out := append([]byte{}, magicV2...)
	out = binary.LittleEndian.AppendUint32(out, uint32(aesLen))
	out = binary.LittleEndian.AppendUint32(out, uint32(xorLen))
	out = append(out, 1)
	out = append(out, enc...)
	out = append(out, plain[aesLen:len(plain)-xorLen]...)
	for _, b := range plain[len(plain)-xorLen:] {
		out = append(out, b^xor)
	}
	return out
}

func fakeJPEG(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * 7)
	}
	copy(b, []byte{0xff, 0xd8, 0xff, 0xe0})
	b[n-2], b[n-1] = 0xff, 0xd9
	return b
}

func keyFor(uin uint64, wxid string) []byte {
	s := md5.Sum([]byte(strconv.FormatUint(uin, 10) + wxid))
	return []byte(hex.EncodeToString(s[:])[:16])
}

func TestDecodeV2(t *testing.T) {
	img := fakeJPEG(3000)
	k := &Keys{AES: []byte("0123456789abcdef"), XOR: 0x26}
	for _, lens := range [][2]int{{1024, 1976}, {1024, 1000}, {100, 50}} {
		dat := encodeV2(img, k.AES, k.XOR, lens[0], lens[1])
		got, mime, err := Decode(dat, k)
		if err != nil || mime != "image/jpeg" || !bytes.Equal(got, img) {
			t.Fatalf("lens %v: err=%v mime=%s equal=%v", lens, err, mime, bytes.Equal(got, img))
		}
	}
	if _, _, err := Decode(encodeV2(img, k.AES, k.XOR, 1024, 1976), &Keys{AES: []byte("fedcba9876543210"), XOR: 0x26}); err == nil {
		t.Fatal("wrong key must fail")
	}
	if _, _, err := Decode(encodeV2(img, k.AES, k.XOR, 1024, 1976), nil); err != ErrNoKey {
		t.Fatalf("missing key: %v", err)
	}
}

func TestDecodeLegacyXOR(t *testing.T) {
	img := fakeJPEG(500)
	dat := make([]byte, len(img))
	for i, b := range img {
		dat[i] = b ^ 0x5a
	}
	got, mime, err := Decode(dat, nil)
	if err != nil || mime != "image/jpeg" || !bytes.Equal(got, img) {
		t.Fatalf("legacy: %v %s", err, mime)
	}
}

func TestDeriveKeys(t *testing.T) {
	const wxid = "wxid_testaccount"
	uin := uint64(3)<<8 | 0x26 // small high bits: found quickly
	key := keyFor(uin, wxid)
	dat := encodeV2(fakeJPEG(4000), key, byte(uin&0xff), 1024, 2900)
	k, err := DeriveKeys(dat, wxid)
	if err != nil {
		t.Fatal(err)
	}
	if string(k.AES) != string(key) || k.XOR != 0x26 {
		t.Fatalf("derived wrong key")
	}
}

func TestVideoLookup(t *testing.T) {
	acct := t.TempDir()
	lib := NewLibrary(acct, filepath.Join(acct, "k.json"), filepath.Join(acct, "cache"), "wxid_x")
	id := "5168e6dd3cea1bfd438c8a3c5becabf5"
	when := time.Date(2026, 11, 4, 14, 59, 0, 0, time.Local)

	if _, err := lib.VideoPath(id, when); err != ErrVideoNotFound {
		t.Fatalf("missing video: got %v", err)
	}
	// Stored in the previous month folder: still found.
	dir := filepath.Join(acct, "msg", "video", "2026-10")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, id+"_raw.mp4"), []byte("raw"), 0o600)
	os.WriteFile(filepath.Join(dir, id+".mp4"), []byte("mp4"), 0o600)
	os.WriteFile(filepath.Join(dir, id+"_thumb.jpg"), fakeJPEG(64), 0o600)
	p, err := lib.VideoPath(id, when)
	if err != nil || filepath.Base(p) != id+".mp4" {
		t.Fatalf("VideoPath = %q, %v", p, err)
	}
	if _, mime, err := lib.VideoCover(id, when); err != nil || mime != "image/jpeg" {
		t.Fatalf("VideoCover = %q, %v", mime, err)
	}
	// Far-away month: found by the fallback glob.
	if _, err := lib.VideoPath(id, when.AddDate(0, 5, 0)); err != nil {
		t.Fatalf("glob fallback: %v", err)
	}
	if _, err := lib.VideoPath("../../etc/passwd", when); err != ErrVideoNotFound {
		t.Fatalf("bad id accepted: %v", err)
	}
}
