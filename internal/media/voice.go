package media

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"github.com/wdvxdr1123/go-silk/sdk"
	"modernc.org/libc"
)

// WeChat voice messages are Skype SILK v3 streams:
//
//	0x02 "#!SILK_V3"                       header (the 0x02 is WeChat's)
//	{ int16 LE nBytes, nBytes payload }*   one packet per 20 ms frame
//	optional 0xFFFF terminator
//
// They decode at 24 kHz mono, which is what WeChat records.

const (
	silkRate = 24000
	// Upper bounds from the SILK SDK: a packet holds at most 5 frames of
	// 20 ms at 48 kHz, and a payload at most 1024 bytes.
	maxSamplesPerPacket = 48000 / 1000 * 20 * 5
	maxPayload          = 1024
)

var ErrNotSILK = errors.New("not a SILK voice stream")

// DecodeSILK turns a WeChat SILK stream into 16-bit mono PCM at 24 kHz.
func DecodeSILK(src []byte) ([]byte, error) {
	switch {
	case bytes.HasPrefix(src, []byte("\x02#!SILK_V3")):
		src = src[10:]
	case bytes.HasPrefix(src, []byte("#!SILK_V3")):
		src = src[9:]
	default:
		return nil, ErrNotSILK
	}
	if len(src) < 2 {
		return nil, errors.New("silk: no audio decoded")
	}

	// The SDK is C transpiled to Go and does pointer arithmetic on raw
	// addresses, so everything it touches lives in libc (C) memory: the GC
	// neither moves nor scans it, and checkptr accepts it.
	tls := libc.NewTLS()
	defer tls.Close()
	var allocs []uintptr
	defer func() {
		for _, p := range allocs {
			libc.Xfree(tls, p)
		}
	}()
	cbytes := func(b []byte) (uintptr, error) {
		p, err := libc.CString(string(b)) // copies len(b) bytes, zeros included
		if err == nil {
			allocs = append(allocs, p)
		}
		return p, err
	}
	readI32 := func(p uintptr) int32 { return int32(binary.LittleEndian.Uint32(libc.GoBytes(p, 4))) }

	stream, err := cbytes(src)
	if err != nil {
		return nil, err
	}
	scratch, err := cbytes(make([]byte, 8)) // decoder size, then sample count
	if err != nil {
		return nil, err
	}
	sdk.SDK_Get_Decoder_Size(tls, scratch)
	decSize := readI32(scratch)
	if decSize <= 0 || decSize > 1<<20 {
		return nil, errors.New("silk: bad decoder size")
	}
	dec, err := cbytes(make([]byte, decSize))
	if err != nil {
		return nil, err
	}
	init := sdk.SDK_DecControlStruct{FAPI_sampleRate: silkRate, FframesPerPacket: 1}
	ctl, err := cbytes(unsafe.Slice((*byte)(unsafe.Pointer(&init)), unsafe.Sizeof(init)))
	if err != nil {
		return nil, err
	}
	moreFrames := ctl + unsafe.Offsetof(init.FmoreInternalDecoderFrames)
	pcm, err := cbytes(make([]byte, maxSamplesPerPacket*2))
	if err != nil {
		return nil, err
	}
	nOut := scratch // int16 output

	if sdk.SDK_InitDecoder(tls, dec) != 0 {
		return nil, errors.New("silk: decoder init failed")
	}
	out := make([]byte, 0, len(src)*12)
	for off := 0; off+2 <= len(src); {
		n := int(int16(binary.LittleEndian.Uint16(src[off:])))
		off += 2
		if n <= 0 || n > maxPayload || n > len(src)-off {
			break // 0xFFFF terminator or truncated tail
		}
		in := stream + uintptr(off)
		off += n
		for frames := 0; ; frames++ {
			libc.Xmemset(tls, nOut, 0, 2)
			// Like the reference decoder, keep the output even when the
			// return code is non-zero: it flags recoverable conditions and
			// the frame is still decoded (or concealed).
			sdk.SDK_Decode(tls, dec, ctl, 0, in, int32(n), pcm, nOut)
			if ns := int(int16(binary.LittleEndian.Uint16(libc.GoBytes(nOut, 2)))); ns > 0 && ns <= maxSamplesPerPacket {
				out = append(out, libc.GoBytes(pcm, ns*2)...) // PCM is little-endian int16
			}
			if readI32(moreFrames) == 0 || frames >= 5 {
				break
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("silk: no audio decoded")
	}
	return out, nil
}

// WAV wraps 16-bit mono PCM in a RIFF header.
func WAV(pcm []byte, rate int) []byte {
	h := make([]byte, 44, 44+len(pcm))
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+len(pcm)))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1) // PCM
	binary.LittleEndian.PutUint16(h[22:], 1) // mono
	binary.LittleEndian.PutUint32(h[24:], uint32(rate))
	binary.LittleEndian.PutUint32(h[28:], uint32(rate*2))
	binary.LittleEndian.PutUint16(h[32:], 2)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(len(pcm)))
	return append(h, pcm...)
}

// VoiceWAV decodes a SILK stream to a WAV file in the private cache and
// returns its path and duration. Results are cached by content hash.
func (l *Library) VoiceWAV(silk []byte) (string, time.Duration, error) {
	sum := sha256.Sum256(silk)
	name := hex.EncodeToString(sum[:])
	p := filepath.Join(l.cacheDir, "voice", name[:2], name+".wav")
	if st, err := os.Stat(p); err == nil && st.Size() > 44 {
		return p, pcmDuration(st.Size() - 44), nil
	}
	l.convSem <- struct{}{}
	pcm, err := DecodeSILK(silk)
	<-l.convSem
	if err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", 0, err
	}
	f, err := os.CreateTemp(filepath.Dir(p), name+".tmp-*")
	if err != nil {
		return "", 0, err
	}
	_, werr := f.Write(WAV(pcm, silkRate))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(f.Name(), p)
	}
	if werr != nil {
		os.Remove(f.Name())
		return "", 0, werr
	}
	return p, pcmDuration(int64(len(pcm))), nil
}

func pcmDuration(n int64) time.Duration {
	return time.Duration(n/2) * time.Second / silkRate
}
