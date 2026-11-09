package media

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Library finds and decodes attachments under xwechat_files/<acct>/msg.
type Library struct {
	attachDir string // .../msg/attach
	keyPath   string // private image-key.json
	cacheDir  string // converted wxgf → jpeg
	wxid      string
	ffmpeg    string
	convSem   chan struct{}

	mu   sync.Mutex
	keys *Keys
	err  error // sticky derivation failure
}

// NewLibrary: accountDir is the parent of db_storage.
func NewLibrary(accountDir, keyPath, cacheDir, wxid string) *Library {
	l := &Library{attachDir: filepath.Join(accountDir, "msg", "attach"), keyPath: keyPath, cacheDir: cacheDir, wxid: wxid, convSem: make(chan struct{}, 2)}
	l.ffmpeg = findFFmpeg()
	if k, err := LoadKeys(keyPath); err == nil {
		l.keys = k
	}
	return l
}

// EnsureKeys loads or derives the V2 image keys.
func (l *Library) EnsureKeys() (*Keys, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.keys != nil {
		return l.keys, nil
	}
	if l.err != nil {
		return nil, l.err
	}
	sample, err := l.findSample()
	if err != nil {
		l.err = err
		return nil, err
	}
	k, err := DeriveKeys(sample, l.wxid)
	if err != nil {
		l.err = err
		return nil, err
	}
	if err := SaveKeys(l.keyPath, k); err != nil {
		return nil, err
	}
	l.keys = k
	return k, nil
}

var errStop = errors.New("stop")

// findSample returns a V2 thumbnail, newest month first.
func (l *Library) findSample() ([]byte, error) {
	var sample []byte
	matches, _ := filepath.Glob(filepath.Join(l.attachDir, "*", time.Now().Format("2006-01"), "Img", "*_t.dat"))
	older, _ := filepath.Glob(filepath.Join(l.attachDir, "*", "*", "Img", "*_t.dat"))
	for _, p := range append(matches, older...) {
		b, err := os.ReadFile(p)
		if err == nil && bytes.HasPrefix(b, magicV2) && len(b) > 64 && b[len(b)-2]^(b[len(b)-1]^0xd9) == 0xff {
			sample = b
			break
		}
	}
	if sample == nil {
		return nil, errors.New("no V2 thumbnail found to derive the image key")
	}
	return sample, nil
}

// Size selects which stored variant to prefer.
type Size int

const (
	Thumb Size = iota
	Full
)

var idRe = regexp.MustCompile(`^[0-9a-zA-Z_]{8,80}$`)

var ErrNotFound = errors.New("image not downloaded to this Mac")

// Image returns a browser-displayable image for a message attachment.
// ids are file stems to try (the hex id from the message, then
// "<local_id>_<create_time>"); t is the message time. thumbOnly reports
// that a Full request fell back to the thumbnail because WeChat has not
// downloaded (or could not convert) the original.
func (l *Library) Image(ctx context.Context, chat string, ids []string, t time.Time, size Size) (img []byte, mime string, thumbOnly bool, err error) {
	sum := md5.Sum([]byte(chat))
	chatDir := filepath.Join(l.attachDir, hex.EncodeToString(sum[:]))

	suffixes := []string{"_t.dat", ".dat", "_h.dat"}
	if size == Full {
		suffixes = []string{"_h.dat", ".dat", "_t.dat"}
	}
	var dirs []string
	for _, d := range []time.Time{t, t.AddDate(0, -1, 0), t.AddDate(0, 1, 0)} {
		dirs = append(dirs, filepath.Join(chatDir, d.Format("2006-01"), "Img"))
	}
	var paths []string
	for _, id := range ids {
		if !idRe.MatchString(id) {
			continue
		}
		if paths = l.candidates(dirs, chatDir, id, suffixes); len(paths) > 0 {
			break
		}
	}
	if len(paths) == 0 {
		return nil, "", false, ErrNotFound
	}
	keys, _ := l.EnsureKeys()
	var lastErr error = ErrNotFound
	var thumb []byte
	var thumbMime string
	var hevc []string
	var hevcData [][]byte
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			lastErr = err
			continue
		}
		dec, typ, err := Decode(data, keys)
		switch {
		case err != nil:
			lastErr = err
		case typ == "image/wxgf":
			// Browsers cannot show WeChat's HEVC container; convert later.
			hevc, hevcData = append(hevc, p), append(hevcData, dec)
		case typ == "image/heic":
			lastErr = errors.New("HEIC image")
		case size == Full && strings.HasSuffix(p, "_t.dat"):
			thumb, thumbMime = dec, typ // last resort for a full request
		default:
			return dec, typ, false, nil
		}
	}
	// Full size: a converted HEVC original beats the thumbnail.
	for i, p := range hevc {
		jpg, err := l.convertWXGF(ctx, p, hevcData[i])
		if err == nil {
			return jpg, "image/jpeg", false, nil
		}
		lastErr = err
	}
	if thumb != nil {
		return thumb, thumbMime, true, nil
	}
	return nil, "", false, lastErr
}

func findFFmpeg() string {
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	for _, p := range []string{"/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffmpeg"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// convertWXGF turns a wxgf (HEVC) image into JPEG via ffmpeg, caching the
// result in the private vault cache.
func (l *Library) convertWXGF(ctx context.Context, src string, data []byte) ([]byte, error) {
	if l.ffmpeg == "" {
		return nil, errors.New("wxgf image needs ffmpeg to display (brew install ffmpeg)")
	}
	st, err := os.Stat(src)
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256(fmt.Appendf(nil, "%s|%d|%d", src, st.Size(), st.ModTime().UnixNano()))
	cachePath := filepath.Join(l.cacheDir, "img", hex.EncodeToString(h[:2]), hex.EncodeToString(h[:])+".jpg")
	if b, err := os.ReadFile(cachePath); err == nil && len(b) > 0 {
		return b, nil
	}
	i := bytes.Index(data, []byte{0, 0, 0, 1})
	if i < 0 {
		return nil, errors.New("wxgf: no HEVC stream")
	}
	select {
	case l.convSem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-l.convSem }()
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, l.ffmpeg, "-hide_banner", "-loglevel", "error", "-f", "hevc", "-i", "pipe:0",
		"-frames:v", "1", "-c:v", "mjpeg", "-q:v", "3", "-f", "image2", "pipe:1")
	cmd.Stdin = bytes.NewReader(data[i:])
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil || Sniff(out.Bytes()) != "image/jpeg" {
		return nil, fmt.Errorf("wxgf conversion failed: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err == nil {
		tmp := cachePath + ".tmp"
		if os.WriteFile(tmp, out.Bytes(), 0o600) == nil {
			os.Rename(tmp, cachePath)
		}
	}
	return out.Bytes(), nil
}

func (l *Library) candidates(dirs []string, chatDir, id string, suffixes []string) []string {
	var out []string
	for _, s := range suffixes {
		for _, d := range dirs {
			p := filepath.Join(d, id+s)
			if _, err := os.Stat(p); err == nil {
				out = append(out, p)
				break
			}
		}
	}
	if len(out) > 0 {
		return out
	}
	// Month folder unknown (clock skew, forwarded media): search the chat.
	found := map[string]string{}
	filepath.WalkDir(chatDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			for _, s := range suffixes {
				if d.Name() == id+s {
					found[s] = p
				}
			}
		}
		return nil
	})
	for _, s := range suffixes {
		if p, ok := found[s]; ok {
			out = append(out, p)
		}
	}
	return out
}
