package store

import "testing"

func pbBytes(field byte, data []byte) []byte {
	return append([]byte{field<<3 | 2, byte(len(data))}, data...)
}

func TestVoiceTranscript(t *testing.T) {
	text := "我问一下他吧，然后我去给他退一下。"
	inner := append([]byte{0x08, 0x02}, pbBytes(2, []byte(text))...)
	// Shape seen in WeChat 4.x: 1:3, 2:4, 5:{1:2, 2:text}, 11:0
	packed := append([]byte{0x08, 0x03, 0x10, 0x04}, pbBytes(5, inner)...)
	packed = append(packed, 0x58, 0x00)

	if got := voiceTranscript(packed); got != text {
		t.Fatalf("transcript = %q", got)
	}

	pending := append([]byte{0x08, 0x00}, pbBytes(2, []byte(text))...)
	if got := voiceTranscript(pbBytes(5, pending)); got != "" {
		t.Fatalf("unfinished transcript returned %q", got)
	}
	for _, b := range [][]byte{nil, {0x08, 0x01}, {0x08, 0x03, 0x10, 0x04}, {0x2a, 0x7f, 0x08}, {0xff, 0xff, 0xff}} {
		if got := voiceTranscript(b); got != "" {
			t.Fatalf("voiceTranscript(%x) = %q", b, got)
		}
	}
}

func TestTrimSilkHeader(t *testing.T) {
	if got := string(trimSilkHeader([]byte("\x02#!SILK_V3abc"))); got != "abc" {
		t.Fatalf("got %q", got)
	}
	if got := string(trimSilkHeader([]byte("abc"))); got != "abc" {
		t.Fatalf("got %q", got)
	}
}
