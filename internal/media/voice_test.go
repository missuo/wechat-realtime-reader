package media

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testdata/tone-440hz-1s.silk: 1 s of a 440 Hz tone, encoded at 24 kHz by
// go-silk's encoder in WeChat mode (its encoder is not checkptr-clean, so
// it is not run under -race here).
func toneSILK(t *testing.T) []byte {
	b, err := os.ReadFile(filepath.Join("testdata", "tone-440hz-1s.silk"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDecodeSILK(t *testing.T) {
	enc := toneSILK(t)
	if enc[0] != 0x02 {
		t.Fatalf("expected WeChat-style header, got %q", enc[:10])
	}
	// WeChat streams may end with a 0xFFFF terminator and trailing junk.
	stream := append(append([]byte{}, enc...), 0xff, 0xff, 1, 2, 3)
	pcm, err := DecodeSILK(stream)
	if err != nil {
		t.Fatal(err)
	}
	if d := pcmDuration(int64(len(pcm))); d < 900*time.Millisecond || d > 1100*time.Millisecond {
		t.Fatalf("decoded %s, want ~1s", d)
	}
	var energy float64
	for i := 0; i+1 < len(pcm); i += 2 {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[i:])))
		energy += v * v
	}
	if rms := math.Sqrt(energy / float64(len(pcm)/2)); rms < 1000 {
		t.Fatalf("decoded audio is near-silent (rms %.0f)", rms)
	}

	if _, err := DecodeSILK([]byte("RIFF....WAVE")); err != ErrNotSILK {
		t.Fatalf("non-SILK input: %v", err)
	}
	if _, err := DecodeSILK(enc[:10]); err == nil {
		t.Fatal("header-only stream decoded")
	}
}

func TestVoiceWAVCache(t *testing.T) {
	enc := toneSILK(t)
	dir := t.TempDir()
	lib := NewLibrary(dir, filepath.Join(dir, "k.json"), filepath.Join(dir, "cache"), "wxid_x")
	p, d, err := lib.VoiceWAV(enc)
	if err != nil || d < 900*time.Millisecond {
		t.Fatalf("VoiceWAV = %q, %s, %v", p, d, err)
	}
	b, _ := os.ReadFile(p)
	if string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		t.Fatal("not a WAV file")
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("cache file mode %v", st.Mode().Perm())
	}
	p2, d2, err := lib.VoiceWAV(enc)
	if err != nil || p2 != p || d2 != d {
		t.Fatalf("cached call = %q, %s, %v", p2, d2, err)
	}
}
