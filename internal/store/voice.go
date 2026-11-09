package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Voice messages carry WeChat's speech-to-text result (when the user has
// transcribed them, manually or automatically) in packed_info_data:
//
//	field 5 (bytes) { field 1 (varint) status, 2 = done; field 2 (string) text }
//
// The audio itself lives in message/media_N.db, table VoiceInfo, as
// "\x02#!SILK_V3" streams keyed by (chat_name_id, create_time, local_id).

const transcriptDone = 2

// voiceTranscript returns the transcript stored in packed_info_data, or "".
func voiceTranscript(packed []byte) string {
	inner, ok := pbField(packed, 5)
	if !ok {
		return ""
	}
	status, _ := pbVarint(inner, 1)
	text, ok := pbField(inner, 2)
	if !ok || status != transcriptDone || !utf8.Valid(text) {
		return ""
	}
	return strings.TrimSpace(string(text))
}

// pbField returns the first length-delimited field num of a protobuf
// message, skipping other fields. Malformed input yields ok=false.
func pbField(b []byte, num uint64) ([]byte, bool) {
	var out []byte
	found := false
	pbWalk(b, func(f uint64, wire int, v uint64, data []byte) bool {
		if f == num && wire == 2 {
			out, found = data, true
			return false
		}
		return true
	})
	return out, found
}

// pbVarint returns the first varint field num.
func pbVarint(b []byte, num uint64) (uint64, bool) {
	var out uint64
	found := false
	pbWalk(b, func(f uint64, wire int, v uint64, _ []byte) bool {
		if f == num && wire == 0 {
			out, found = v, true
			return false
		}
		return true
	})
	return out, found
}

func pbWalk(b []byte, fn func(field uint64, wire int, v uint64, data []byte) bool) {
	for len(b) > 0 {
		key, n := uvarint(b)
		if n <= 0 {
			return
		}
		b = b[n:]
		field, wire := key>>3, int(key&7)
		switch wire {
		case 0:
			v, n := uvarint(b)
			if n <= 0 {
				return
			}
			b = b[n:]
			if !fn(field, wire, v, nil) {
				return
			}
		case 1:
			if len(b) < 8 {
				return
			}
			b = b[8:]
		case 2:
			l, n := uvarint(b)
			if n <= 0 || l > uint64(len(b)-n) {
				return
			}
			data := b[n : n+int(l)]
			b = b[n+int(l):]
			if !fn(field, wire, 0, data) {
				return
			}
		case 5:
			if len(b) < 4 {
				return
			}
			b = b[4:]
		default:
			return
		}
	}
}

func uvarint(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		v |= uint64(b[i]&0x7f) << (7 * i)
		if b[i] < 0x80 {
			return v, i + 1
		}
	}
	return 0, 0
}

// ErrVoiceNotFound means no audio is stored for the message.
var ErrVoiceNotFound = errors.New("voice audio not found on this Mac")

var mediaDBRe = regexp.MustCompile(`^media_(\d+)\.db$`)

// mediaDBs lists voice databases, newest shard first.
func (s *Store) mediaDBs() []string {
	ents, _ := os.ReadDir(filepath.Join(s.dir, "message"))
	var out []string
	for _, e := range ents {
		if mediaDBRe.MatchString(e.Name()) {
			out = append(out, "message/"+e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}

// VoiceSILK returns the raw SILK stream of a voice message, identified by
// chat, message local_id and send time.
func (s *Store) VoiceSILK(ctx context.Context, chat string, localID int64, t time.Time) ([]byte, error) {
	dbs := s.mediaDBs()
	if len(dbs) == 0 {
		return nil, fmt.Errorf("message/media_0.db: %w", ErrNotReady)
	}
	for _, rel := range dbs {
		h, err := s.get(rel)
		if err != nil {
			return nil, err
		}
		id, ok := h.ids[chat]
		if !ok {
			continue
		}
		rows, err := h.db.QueryContext(ctx, `SELECT voice_data FROM VoiceInfo
			WHERE chat_name_id = ? AND local_id = ? AND create_time = ?
			ORDER BY CAST(data_index AS INTEGER)`, id, localID, t.Unix())
		if err != nil {
			return nil, err
		}
		var out []byte
		for rows.Next() {
			var b []byte
			if err := rows.Scan(&b); err != nil {
				rows.Close()
				return nil, err
			}
			if len(out) > 0 {
				// A continuation chunk must not restart the stream.
				b = trimSilkHeader(b)
			}
			out = append(out, b...)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	return nil, ErrVoiceNotFound
}

func trimSilkHeader(b []byte) []byte {
	for _, h := range [][]byte{[]byte("\x02#!SILK_V3"), []byte("#!SILK_V3")} {
		if bytes.HasPrefix(b, h) {
			return b[len(h):]
		}
	}
	return b
}
