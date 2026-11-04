// Package crypto implements page-level authenticated decryption for
// WeChat 4.x (WCDB / SQLCipher 4 compatible) databases.
//
// Page layout (4096 bytes, reserve 80):
//
//	[ salt(16) only on page 1 | ciphertext ... | IV(16) | HMAC-SHA512(64) ]
//
// The HMAC covers ciphertext||IV||little-endian(pgno) and is keyed with
// PBKDF2-HMAC-SHA512(encKey, salt XOR 0x3a, 2 rounds, 32 bytes).
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
)

const (
	PageSize    = 4096
	SaltSize    = 16
	IVSize      = 16
	HMACSize    = 64
	Reserve     = 80 // IV + HMAC rounded to AES block size
	KDFRounds   = 256000
	MACRounds   = 2
	KeySize     = 32
	sqliteMagic = "SQLite format 3\x00"
)

// TagSize is the number of trailing bytes per page that uniquely identify
// its ciphertext (IV + HMAC). Used to detect changed pages cheaply.
const TagSize = IVSize + HMACSize

var (
	ErrHMAC     = errors.New("page HMAC mismatch")
	ErrShortKey = errors.New("key must be 32 bytes (64 hex chars)")
)

// Cipher holds the derived keys for one database (one salt).
type Cipher struct {
	encKey []byte
	macKey []byte
	block  cipher.Block
}

// Salt returns the 16-byte salt stored at the start of page 1.
func Salt(page1 []byte) []byte { return page1[:SaltSize] }

// KeyMode says how the configured 32-byte key relates to the AES key.
type KeyMode int

const (
	// KeyDerived: the configured key is already PBKDF2(raw, salt) output.
	KeyDerived KeyMode = iota
	// KeyRaw: the configured key is the PBKDF2 password; derive it.
	KeyRaw
)

func (m KeyMode) String() string {
	if m == KeyRaw {
		return "raw"
	}
	return "derived"
}

// ParseHexKey decodes a 64-char hex key.
func ParseHexKey(s string) ([]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("decode key: %w", err)
	}
	if len(b) != KeySize {
		return nil, ErrShortKey
	}
	return b, nil
}

// NewCipher builds a cipher for the given key and salt.
func NewCipher(key, salt []byte, mode KeyMode) (*Cipher, error) {
	if len(key) != KeySize {
		return nil, ErrShortKey
	}
	enc := key
	if mode == KeyRaw {
		var err error
		enc, err = pbkdf2.Key(sha512.New, string(key), salt, KDFRounds, KeySize)
		if err != nil {
			return nil, err
		}
	}
	macSalt := make([]byte, len(salt))
	for i := range salt {
		macSalt[i] = salt[i] ^ 0x3a
	}
	mac, err := pbkdf2.Key(sha512.New, string(enc), macSalt, MACRounds, KeySize)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(enc)
	if err != nil {
		return nil, err
	}
	return &Cipher{encKey: enc, macKey: mac, block: block}, nil
}

// Open tries both key modes against page 1 and returns the first that
// authenticates.
func Open(key, page1 []byte) (*Cipher, KeyMode, error) {
	if len(page1) < PageSize {
		return nil, 0, errors.New("page 1 too short")
	}
	salt := Salt(page1)
	for _, mode := range []KeyMode{KeyDerived, KeyRaw} {
		c, err := NewCipher(key, salt, mode)
		if err != nil {
			return nil, 0, err
		}
		if c.Verify(page1, 1) == nil {
			return c, mode, nil
		}
	}
	return nil, 0, ErrHMAC
}

func dataStart(pgno uint32) int {
	if pgno == 1 {
		return SaltSize
	}
	return 0
}

// Verify checks the page HMAC without decrypting.
func (c *Cipher) Verify(page []byte, pgno uint32) error {
	return c.verify(hmac.New(sha512.New, c.macKey), page, pgno)
}

func (c *Cipher) verify(h hash.Hash, page []byte, pgno uint32) error {
	if len(page) != PageSize {
		return fmt.Errorf("page %d: bad size %d", pgno, len(page))
	}
	start := dataStart(pgno)
	end := PageSize - Reserve + IVSize
	h.Reset()
	h.Write(page[start:end])
	var n [4]byte
	binary.LittleEndian.PutUint32(n[:], pgno)
	h.Write(n[:])
	var sum [HMACSize]byte
	if !hmac.Equal(h.Sum(sum[:0]), page[end:end+HMACSize]) {
		return fmt.Errorf("page %d: %w", pgno, ErrHMAC)
	}
	return nil
}

// Decryptor is a per-goroutine helper reusing an HMAC state.
type Decryptor struct {
	c *Cipher
	h hash.Hash
}

func (c *Cipher) Decryptor() *Decryptor {
	return &Decryptor{c: c, h: hmac.New(sha512.New, c.macKey)}
}

// Verify authenticates a page without decrypting it.
func (d *Decryptor) Verify(src []byte, pgno uint32) error { return d.c.verify(d.h, src, pgno) }

// DecryptPage authenticates then decrypts src into dst (both PageSize).
// The output is a valid plaintext SQLite page; the reserve area keeps the
// original IV+HMAC so it can be used as a change tag later. For page 1 the
// SQLite header magic replaces the salt and the WAL flags are cleared so
// the snapshot opens as an ordinary rollback-journal database.
func (d *Decryptor) DecryptPage(dst, src []byte, pgno uint32) error {
	if err := d.c.verify(d.h, src, pgno); err != nil {
		return err
	}
	start := dataStart(pgno)
	end := PageSize - Reserve
	iv := src[end : end+IVSize]
	cipher.NewCBCDecrypter(d.c.block, iv).CryptBlocks(dst[start:end], src[start:end])
	copy(dst[end:], src[end:])
	if pgno == 1 {
		copy(dst[:SaltSize], sqliteMagic)
		// bytes 18/19: file format write/read version; 2 = WAL. Force 1 so
		// the immutable snapshot never looks for a -wal file.
		dst[18], dst[19] = 1, 1
	}
	return nil
}

// Tag returns the trailing IV+HMAC slice of an encrypted (or snapshot) page.
func Tag(page []byte) []byte { return page[PageSize-TagSize:] }

// EncryptPage is the inverse of DecryptPage. The reader never writes
// WeChat data; this exists to build synthetic fixtures in tests.
func (c *Cipher) EncryptPage(dst, plain []byte, pgno uint32, salt, iv []byte) {
	start := dataStart(pgno)
	end := PageSize - Reserve
	copy(dst[end:end+IVSize], iv)
	cipher.NewCBCEncrypter(c.block, iv).CryptBlocks(dst[start:end], plain[start:end])
	if pgno == 1 {
		copy(dst, salt)
	}
	h := hmac.New(sha512.New, c.macKey)
	h.Write(dst[start : end+IVSize])
	var n [4]byte
	binary.LittleEndian.PutUint32(n[:], pgno)
	h.Write(n[:])
	h.Sum(dst[end+IVSize : end+IVSize])
}
