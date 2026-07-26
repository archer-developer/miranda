package tts

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/braheezy/shine-mp3/pkg/mp3"
)

// Gemini's speech-generation API (responseModalities: ["AUDIO"]) documents
// its inlineData.mimeType as e.g. "audio/L16;rate=24000" — 16-bit signed
// little-endian PCM, mono, 24kHz — and that shape doesn't vary with model or
// voice choice, so these are fixed constants rather than something parsed
// out of the response's mimeType field.
const (
	geminiPCMSampleRate    = 24000
	geminiPCMBitsPerSample = 16
	geminiPCMChannels      = 1
)

// pcmDurationMS returns how long pcm plays for, in milliseconds, given the
// fixed sample rate/bit depth/channel count Gemini's speech API always
// returns (see the constants above) — exact, since it's derived directly
// from the raw sample count rather than parsed back out of an encoded
// container. Used to know how long to let the station play a rendered
// chunk instead of polling for it: the station's alice_state attribute
// (reliable for media_content_type "text"/"dialog" announcements) never
// transitions at all for the "radio_play" mechanism URL playback goes
// through — confirmed by direct observation against a real device — so
// there is no HA-exposed signal to poll for this playback path.
func pcmDurationMS(pcm []byte) int {
	bytesPerSample := geminiPCMBitsPerSample / 8
	samples := len(pcm) / (bytesPerSample * geminiPCMChannels)
	return samples * 1000 / geminiPCMSampleRate
}

// geminiPlaybackSafetyMargin pads pcmDurationMS's estimate to account for
// the real-world latency between firing play_media and the station actually
// starting to render audio (HA fetching/proxying the file, the station's
// own buffering) — better to wait slightly too long than to fire the next
// chunk's play_media while this one's tail is still audible.
const geminiPlaybackSafetyMargin = 700 * time.Millisecond

// audioFileExt maps a config.GeminiTTSConfig.AudioFormat value to the file
// extension cache.go/httpserve.go store/serve it under. Unrecognized or
// empty values fall back to "wav", matching encodeAudio's default.
func audioFileExt(format string) string {
	if format == "mp3" {
		return "mp3"
	}
	return "wav"
}

// encodeAudio wraps raw PCM (see the constants above) in the container
// config.GeminiTTSConfig.AudioFormat selects. Which container a real Yandex
// Station actually accepts over media_player.play_media's URL playback is
// an open question the AlexxIT/YandexStation integration's own docs don't
// settle (they list AAC/FLAC/MP3 for URL playback, not WAV) — so both paths
// are implemented, and switching is a one-line config change, not a code
// change.
func encodeAudio(pcm []byte, format string) ([]byte, error) {
	if format == "mp3" {
		return encodeMP3(pcm)
	}
	return wrapWAV(pcm), nil
}

// wrapWAV prepends a standard 44-byte RIFF/WAVE header describing 16-bit
// PCM mono 24kHz audio to pcm. This is the simplest possible container and
// needs zero new dependencies, since the bytes Gemini returns already *are*
// that PCM payload — wrapWAV only ever describes them, never re-encodes
// them.
func wrapWAV(pcm []byte) []byte {
	const (
		riffHeaderChunkSize = 36 // everything after this field, before "data"'s length prefix
		fmtChunkSize        = 16 // fmt subchunk size for uncompressed PCM
		pcmAudioFormat      = 1  // WAVE_FORMAT_PCM
	)
	byteRate := geminiPCMSampleRate * geminiPCMChannels * geminiPCMBitsPerSample / 8
	blockAlign := geminiPCMChannels * geminiPCMBitsPerSample / 8

	var buf bytes.Buffer
	buf.Grow(44 + len(pcm))
	buf.WriteString("RIFF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(riffHeaderChunkSize+len(pcm)))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(fmtChunkSize))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(pcmAudioFormat))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(geminiPCMChannels))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(geminiPCMSampleRate))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(byteRate))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(blockAlign))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(geminiPCMBitsPerSample))
	buf.WriteString("data")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(pcm)))
	buf.Write(pcm)
	return buf.Bytes()
}

// encodeMP3 encodes raw 16-bit little-endian PCM to MP3 via shine-mp3 — the
// only pure-Go, cgo-free MP3 encoder available (CLAUDE.md forbids cgo,
// CGO_ENABLED=0 always).
func encodeMP3(pcm []byte) ([]byte, error) {
	if len(pcm)%2 != 0 {
		return nil, fmt.Errorf("tts: mp3 encode: odd-length PCM buffer (%d bytes)", len(pcm))
	}
	samples := make([]int16, len(pcm)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(pcm[i*2:]))
	}

	var buf bytes.Buffer
	enc := mp3.NewEncoder(geminiPCMSampleRate, geminiPCMChannels)
	if err := enc.Write(&buf, samples); err != nil {
		return nil, fmt.Errorf("tts: mp3 encode: %w", err)
	}
	return buf.Bytes(), nil
}
