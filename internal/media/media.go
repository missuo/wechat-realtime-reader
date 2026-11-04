// Package media decodes WeChat 4.x image attachments (*.dat) and locates
// them on disk. Files are only read, never modified.
//
// V1/V2 layout:
//
//	"\x07\x08V1\x08\x07" | "\x07\x08V2\x08\x07"   6-byte magic
//	aesLen  uint32 LE                               plaintext bytes under AES
//	xorLen  uint32 LE                               trailing bytes under XOR
//	1 byte  (unused)
//	AES-128-ECB(PKCS7(first aesLen bytes))         aesLen rounded up to 16 (+16 when aligned)
//	raw middle bytes
//	XOR(xorKey, last xorLen bytes)
//
// V1 uses a fixed AES key. V2 uses hex(md5(uin + wxid))[:16] and xorKey =
// uin & 0xff; DeriveKeys recovers both from one JPEG thumbnail.
// Pre-4.x files XOR the whole file with a single byte.
package media

import (
	"bytes"
	"crypto/aes"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
)

var (
	magicV1 = []byte("\x07\x08V1\x08\x07")
	magicV2 = []byte("\x07\x08V2\x08\x07")
	v1Key   = []byte("cfcd208495d565ef")
)

const headerLen = 15

// Keys decrypt V2 image files.
type Keys struct {
	AES []byte // 16 ASCII bytes
	XOR byte
}

var ErrNoKey = errors.New("image key unknown")

// Decode returns the image bytes and their MIME type.
func Decode(data []byte, k *Keys) ([]byte, string, error) {
	switch {
	case bytes.HasPrefix(data, magicV1):
		xor := byte(0)
		if k != nil {
			xor = k.XOR
		}
		return decodeV(data, v1Key, xor)
	case bytes.HasPrefix(data, magicV2):
		if k == nil || len(k.AES) != 16 {
			return nil, "", ErrNoKey
		}
		return decodeV(data, k.AES, k.XOR)
	}
	return decodeLegacy(data)
}

func decodeV(data, key []byte, xor byte) ([]byte, string, error) {
	if len(data) < headerLen {
		return nil, "", errors.New("dat: short file")
	}
	aesLen := int(binary.LittleEndian.Uint32(data[6:10]))
	xorLen := int(binary.LittleEndian.Uint32(data[10:14]))
	encLen := aesLen + 16 - aesLen%16 // PKCS7 always adds padding
	body := data[headerLen:]
	if encLen > len(body) || xorLen > len(body)-encLen {
		return nil, "", errors.New("dat: bad lengths")
	}
	c, err := aes.NewCipher(key)
	if err != nil {
		return nil, "", err
	}
	out := make([]byte, 0, len(body))
	blk := make([]byte, 16)
	for i := 0; i < encLen; i += 16 {
		c.Decrypt(blk, body[i:i+16])
		out = append(out, blk...)
	}
	pad := int(out[len(out)-1])
	if pad == 0 || pad > 16 || pad > len(out) {
		return nil, "", errors.New("dat: bad padding (wrong key?)")
	}
	out = out[:len(out)-pad]
	out = append(out, body[encLen:len(body)-xorLen]...)
	for _, b := range body[len(body)-xorLen:] {
		out = append(out, b^xor)
	}
	mime := Sniff(out)
	if mime == "" {
		return nil, "", errors.New("dat: unknown image format (wrong key?)")
	}
	return out, mime, nil
}

var legacyMagics = [][]byte{{0xff, 0xd8, 0xff}, {0x89, 'P', 'N', 'G'}, []byte("GIF8"), []byte("RIFF"), []byte("wxgf")}

func decodeLegacy(data []byte) ([]byte, string, error) {
	for _, m := range legacyMagics {
		if len(data) < len(m) {
			continue
		}
		x := data[0] ^ m[0]
		ok := true
		for i := 1; i < len(m); i++ {
			if data[i]^x != m[i] {
				ok = false
				break
			}
		}
		if ok {
			out := make([]byte, len(data))
			for i, b := range data {
				out[i] = b ^ x
			}
			return out, Sniff(out), nil
		}
	}
	return nil, "", errors.New("dat: unrecognized format")
}

// Sniff returns the MIME type of an image, "image/wxgf" for WeChat's HEVC
// container, or "" if unknown.
func Sniff(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte{0xff, 0xd8, 0xff}):
		return "image/jpeg"
	case bytes.HasPrefix(b, []byte{0x89, 'P', 'N', 'G'}):
		return "image/png"
	case bytes.HasPrefix(b, []byte("GIF8")):
		return "image/gif"
	case len(b) > 12 && bytes.HasPrefix(b, []byte("RIFF")) && string(b[8:12]) == "WEBP":
		return "image/webp"
	case len(b) > 12 && string(b[4:8]) == "ftyp":
		return "image/heic"
	case bytes.HasPrefix(b, []byte("wxgf")):
		return "image/wxgf"
	}
	return ""
}

// DeriveKeys recovers the V2 keys from a V2-encrypted JPEG thumbnail by
// searching uin values whose low byte equals the XOR key (2^24 candidates,
// a couple of seconds on all cores).
func DeriveKeys(sample []byte, wxid string) (*Keys, error) {
	if !bytes.HasPrefix(sample, magicV2) || len(sample) < headerLen+16 {
		return nil, errors.New("sample is not a V2 dat file")
	}
	// Thumbnails are JPEG and end with FF D9.
	xor := sample[len(sample)-1] ^ 0xd9
	if sample[len(sample)-2]^xor != 0xff {
		return nil, errors.New("sample does not look like a JPEG")
	}
	blk := sample[headerLen : headerLen+16]
	var found atomic.Pointer[Keys]
	var wg sync.WaitGroup
	n := runtime.NumCPU()
	for w := 0; w < n; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			out := make([]byte, 16)
			buf := make([]byte, 0, 64)
			var hx [32]byte
			for hi := uint64(w); hi < 1<<24; hi += uint64(n) {
				if hi&0xffff == 0 && found.Load() != nil {
					return
				}
				uin := hi<<8 | uint64(xor)
				buf = strconv.AppendUint(buf[:0], uin, 10)
				buf = append(buf, wxid...)
				sum := md5.Sum(buf)
				hex.Encode(hx[:], sum[:])
				c, _ := aes.NewCipher(hx[:16])
				c.Decrypt(out, blk)
				if out[0] == 0xff && out[1] == 0xd8 && out[2] == 0xff {
					k := &Keys{AES: append([]byte(nil), hx[:16]...), XOR: xor}
					if _, _, err := Decode(sample, k); err == nil {
						found.Store(k)
						return
					}
				}
			}
		}(w)
	}
	wg.Wait()
	if k := found.Load(); k != nil {
		return k, nil
	}
	return nil, errors.New("image key not found")
}

type keyFile struct {
	AESKey string `json:"aes_key"`
	XORKey int    `json:"xor_key"`
}

// LoadKeys reads image-key.json from the private config directory.
func LoadKeys(path string) (*Keys, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s: permissions too open", filepath.Base(path))
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var kf keyFile
	if err := json.Unmarshal(b, &kf); err != nil || len(kf.AESKey) != 16 {
		return nil, fmt.Errorf("%s: invalid", filepath.Base(path))
	}
	return &Keys{AES: []byte(kf.AESKey), XOR: byte(kf.XORKey)}, nil
}

// SaveKeys writes image-key.json with mode 0600.
func SaveKeys(path string, k *Keys) error {
	b, _ := json.MarshalIndent(keyFile{AESKey: string(k.AES), XORKey: int(k.XOR)}, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
