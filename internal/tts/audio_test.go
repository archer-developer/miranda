package tts

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAudioFileExt(t *testing.T) {
	require.Equal(t, "wav", audioFileExt("wav"))
	require.Equal(t, "wav", audioFileExt(""))
	require.Equal(t, "wav", audioFileExt("unknown"))
	require.Equal(t, "mp3", audioFileExt("mp3"))
}

// TestWrapWAV_HeaderFieldsDescribeGeminisDocumentedPCMShape checks every
// field of the 44-byte RIFF/WAVE header wrapWAV produces byte-by-byte,
// against the fixed offsets the format defines — rather than pulling in an
// external WAV decoder dependency just for a test, when this package's own
// only consumer of the format is the equally simple encoder being tested.
func TestWrapWAV_HeaderFieldsDescribeGeminisDocumentedPCMShape(t *testing.T) {
	pcm := []byte{0x01, 0x00, 0xFF, 0xFF} // two int16 samples: 1, -1

	got := wrapWAV(pcm)
	require.Len(t, got, 44+len(pcm))

	require.Equal(t, "RIFF", string(got[0:4]))
	require.Equal(t, uint32(36+len(pcm)), binary.LittleEndian.Uint32(got[4:8]))
	require.Equal(t, "WAVE", string(got[8:12]))
	require.Equal(t, "fmt ", string(got[12:16]))
	require.Equal(t, uint32(16), binary.LittleEndian.Uint32(got[16:20]), "fmt chunk size for PCM")
	require.Equal(t, uint16(1), binary.LittleEndian.Uint16(got[20:22]), "audio format: PCM")
	require.Equal(t, uint16(geminiPCMChannels), binary.LittleEndian.Uint16(got[22:24]))
	require.Equal(t, uint32(geminiPCMSampleRate), binary.LittleEndian.Uint32(got[24:28]))
	wantByteRate := uint32(geminiPCMSampleRate * geminiPCMChannels * geminiPCMBitsPerSample / 8)
	require.Equal(t, wantByteRate, binary.LittleEndian.Uint32(got[28:32]))
	wantBlockAlign := uint16(geminiPCMChannels * geminiPCMBitsPerSample / 8)
	require.Equal(t, wantBlockAlign, binary.LittleEndian.Uint16(got[32:34]))
	require.Equal(t, uint16(geminiPCMBitsPerSample), binary.LittleEndian.Uint16(got[34:36]))
	require.Equal(t, "data", string(got[36:40]))
	require.Equal(t, uint32(len(pcm)), binary.LittleEndian.Uint32(got[40:44]))
	require.Equal(t, pcm, got[44:])
}

func TestEncodeAudio_DispatchesOnFormat(t *testing.T) {
	pcm := []byte{0x01, 0x00, 0xFF, 0xFF, 0x02, 0x00, 0xFE, 0xFF}

	wavOut, err := encodeAudio(pcm, "wav")
	require.NoError(t, err)
	require.Equal(t, "RIFF", string(wavOut[0:4]))

	mp3Out, err := encodeAudio(pcm, "mp3")
	require.NoError(t, err)
	require.NotEmpty(t, mp3Out)
	require.NotEqual(t, "RIFF", string(mp3Out[0:4]))
}

func TestEncodeMP3_ProducesNonEmptyOutputForValidPCM(t *testing.T) {
	// A little more than one MP3 frame's worth of silence, so the encoder
	// has something to actually flush.
	pcm := make([]byte, 8192)
	out, err := encodeMP3(pcm)
	require.NoError(t, err)
	require.NotEmpty(t, out)
}

func TestEncodeMP3_RejectsOddLengthPCM(t *testing.T) {
	_, err := encodeMP3([]byte{0x01})
	require.Error(t, err)
}
