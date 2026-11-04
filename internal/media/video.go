package media

import (
	"errors"
	"os"
	"path/filepath"
	"time"
)

// WeChat 4.x stores video attachments unencrypted under
// msg/video/<YYYY-MM>/<media_id>{.mp4,_raw.mp4,_thumb.jpg,.jpg}.
// The month folder normally matches the message time; neighbouring months
// and finally every month are searched to cover clock skew and late
// downloads.

var ErrVideoNotFound = errors.New("video not downloaded to this Mac")

func (l *Library) videoDir() string { return filepath.Join(filepath.Dir(l.attachDir), "video") }

// findVideoFile returns the first existing <id><suffix> for the given
// suffixes, preferring earlier suffixes and the message's own month.
func (l *Library) findVideoFile(id string, t time.Time, suffixes []string) (string, error) {
	if !idRe.MatchString(id) {
		return "", ErrVideoNotFound
	}
	root := l.videoDir()
	for _, s := range suffixes {
		for _, d := range []time.Time{t, t.AddDate(0, -1, 0), t.AddDate(0, 1, 0)} {
			p := filepath.Join(root, d.Format("2006-01"), id+s)
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Size() > 0 {
				return p, nil
			}
		}
	}
	for _, s := range suffixes {
		m, _ := filepath.Glob(filepath.Join(root, "*", id+s))
		for _, p := range m {
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Size() > 0 {
				return p, nil
			}
		}
	}
	return "", ErrVideoNotFound
}

// VideoPath returns the local mp4 for a video message. The compressed
// .mp4 is preferred (it is what WeChat plays); _raw.mp4 is the fallback.
func (l *Library) VideoPath(id string, t time.Time) (string, error) {
	return l.findVideoFile(id, t, []string{".mp4", "_raw.mp4"})
}

// VideoCover returns the cover image of a video message (JPEG).
func (l *Library) VideoCover(id string, t time.Time) ([]byte, string, error) {
	p, err := l.findVideoFile(id, t, []string{"_thumb.jpg", ".jpg"})
	if err != nil {
		return nil, "", err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, "", err
	}
	mime := Sniff(b)
	if mime == "" {
		return nil, "", errors.New("video cover: unknown image format")
	}
	return b, mime, nil
}
