package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"testing"
)

// encryptPage builds a synthetic encrypted page (test-only, synthetic keys).
func encryptPage(t *testing.T, c *Cipher, plain []byte, pgno uint32, salt []byte) []byte {
	t.Helper()
	out := make([]byte, PageSize)
	start := dataStart(pgno)
	end := PageSize - Reserve
	iv := out[end : end+IVSize]
	rand.Read(iv)
	cipher.NewCBCEncrypter(c.block, iv).CryptBlocks(out[start:end], plain[start:end])
	if pgno == 1 {
		copy(out, salt)
	}
	h := hmac.New(sha512.New, c.macKey)
	h.Write(out[start : end+IVSize])
	var n [4]byte
	binary.LittleEndian.PutUint32(n[:], pgno)
	h.Write(n[:])
	copy(out[end+IVSize:], h.Sum(nil))
	return out
}

func setup(t *testing.T) (*Cipher, []byte, []byte) {
	key := make([]byte, KeySize)
	salt := make([]byte, SaltSize)
	rand.Read(key)
	rand.Read(salt)
	c, err := NewCipher(key, salt, KeyDerived)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := aes.NewCipher(key); err != nil {
		t.Fatal(err)
	}
	return c, key, salt
}

func plainPage(fill byte) []byte {
	p := bytes.Repeat([]byte{fill}, PageSize)
	copy(p, "SQLite format 3\x00")
	p[18], p[19] = 2, 2
	return p
}

func TestRoundTripAndOpen(t *testing.T) {
	c, key, salt := setup(t)
	p1 := plainPage(0x11)
	enc1 := encryptPage(t, c, p1, 1, salt)

	got, mode, err := Open(key, enc1)
	if err != nil || mode != KeyDerived {
		t.Fatalf("open: %v mode=%v", err, mode)
	}
	dst := make([]byte, PageSize)
	if err := got.Decryptor().DecryptPage(dst, enc1, 1); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dst[20:PageSize-Reserve], p1[20:PageSize-Reserve]) || dst[16] != p1[16] || dst[17] != p1[17] {
		t.Fatal("page 1 payload mismatch")
	}
	if string(dst[:16]) != "SQLite format 3\x00" || dst[18] != 1 || dst[19] != 1 {
		t.Fatal("page 1 header not rebuilt as rollback-journal db")
	}
	if !bytes.Equal(Tag(dst), Tag(enc1)) {
		t.Fatal("tag must be preserved in snapshot reserve area")
	}
}

func TestTamperDetection(t *testing.T) {
	c, _, salt := setup(t)
	enc := encryptPage(t, c, plainPage(0x22), 7, salt)
	d := c.Decryptor()
	dst := make([]byte, PageSize)

	for name, mutate := range map[string]func([]byte){
		"ciphertext": func(p []byte) { p[100] ^= 1 },
		"iv":         func(p []byte) { p[PageSize-Reserve+1] ^= 1 },
		"hmac":       func(p []byte) { p[PageSize-1] ^= 1 },
	} {
		p := bytes.Clone(enc)
		mutate(p)
		if err := d.DecryptPage(dst, p, 7); !errors.Is(err, ErrHMAC) {
			t.Errorf("%s tamper not detected: %v", name, err)
		}
	}
	if err := d.DecryptPage(dst, enc, 8); !errors.Is(err, ErrHMAC) {
		t.Error("wrong page number not detected")
	}
	if err := d.DecryptPage(dst, enc, 7); err != nil {
		t.Errorf("valid page rejected: %v", err)
	}
}

func TestWrongKey(t *testing.T) {
	c, _, salt := setup(t)
	enc1 := encryptPage(t, c, plainPage(0x33), 1, salt)
	other := make([]byte, KeySize)
	rand.Read(other)
	if _, _, err := Open(other, enc1); !errors.Is(err, ErrHMAC) {
		t.Fatalf("wrong key accepted: %v", err)
	}
}
