package media

import (
	"bytes"
	"fmt"
	"io"
	"math"

	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/domain"
)

// CheckAlignment probes the audio payload, enforces the correction ceiling,
// and returns the inspected clip together with the frame delta that alignment
// applies. It performs every validation AlignAudioTo performs before any byte
// is written, so callers can surface failures before staging output.
func CheckAlignment(
	audioData []byte,
	measuredOffsetMS int,
	maxCorrectionMS int,
) (AudioClip, int, error) {
	clip, err := InspectAudio(audioData)
	if err != nil {
		return AudioClip{}, 0, fmt.Errorf("audio probe failed: %w", err)
	}
	if absInt(measuredOffsetMS) > maxCorrectionMS {
		return AudioClip{}, 0, domain.NewError(
			"correction_limit_exceeded",
			422,
			"measured correction of %d ms exceeds the ceiling of %d ms",
			absInt(measuredOffsetMS),
			maxCorrectionMS,
		)
	}
	frameDelta := int(math.Round(float64(measuredOffsetMS) * float64(clip.Info.SampleRate) / 1000))
	return clip, frameDelta, nil
}

// WriteAlignedWAV streams the aligned WAV encoding of an inspected clip to w.
// The sample shift is applied directly on the shared PCM bytes, so no
// intermediate sample buffer or output copy is allocated.
func WriteAlignedWAV(w io.Writer, clip AudioClip, frameDelta int) error {
	pcm := clip.pcm
	dataSize := len(pcm)
	if _, err := w.Write(pcm16Header(clip.Info.SampleRate, clip.Info.Channels, dataSize)); err != nil {
		return err
	}
	byteDelta := frameDelta * clip.Info.Channels * 2
	switch {
	case byteDelta > 0:
		if byteDelta < dataSize {
			if _, err := w.Write(pcm[byteDelta:]); err != nil {
				return err
			}
			return writeZeros(w, byteDelta)
		}
		return writeZeros(w, dataSize)
	case byteDelta < 0:
		if -byteDelta < dataSize {
			if err := writeZeros(w, -byteDelta); err != nil {
				return err
			}
			_, err := w.Write(pcm[:dataSize+byteDelta])
			return err
		}
		return writeZeros(w, dataSize)
	default:
		_, err := w.Write(pcm)
		return err
	}
}

// writeZeros writes count zero bytes using a small bounded scratch buffer.
func writeZeros(w io.Writer, count int) error {
	scratch := make([]byte, 64<<10)
	remaining := count
	for remaining > 0 {
		chunk := remaining
		if chunk > len(scratch) {
			chunk = len(scratch)
		}
		if _, err := w.Write(scratch[:chunk]); err != nil {
			return err
		}
		remaining -= chunk
	}
	return nil
}

// AlignAudioTo validates the correction and streams the aligned WAV to w,
// returning the applied frame delta.
func AlignAudioTo(w io.Writer, audioData []byte, measuredOffsetMS int, maxCorrectionMS int) (int, error) {
	clip, frameDelta, err := CheckAlignment(audioData, measuredOffsetMS, maxCorrectionMS)
	if err != nil {
		return 0, err
	}
	if err := WriteAlignedWAV(w, clip, frameDelta); err != nil {
		return 0, err
	}
	return frameDelta, nil
}

// AlignAudio aligns the audio payload by the inverse of the measured offset
// and returns the finished WAV bytes with their digest.
func AlignAudio(audioData []byte, measuredOffsetMS int, maxCorrectionMS int) (domain.AlignedAudio, error) {
	var output bytes.Buffer
	applied, err := AlignAudioTo(&output, audioData, measuredOffsetMS, maxCorrectionMS)
	if err != nil {
		return domain.AlignedAudio{}, err
	}
	wav := output.Bytes()
	return domain.AlignedAudio{
		WAV:            wav,
		AppliedSamples: applied,
		Digest:         DigestBytes(wav),
	}, nil
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
