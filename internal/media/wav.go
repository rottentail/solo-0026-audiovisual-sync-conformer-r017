package media

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/domain"
)

// AudioClip describes a probed WAV payload. The PCM region is shared with the
// immutable source payload instead of copied, so probing does not allocate a
// second buffer proportional to the audio size.
type AudioClip struct {
	Info domain.AudioInfo
	pcm  []byte
}

// sampleCount reports the number of 16-bit samples across all channels.
func (clip AudioClip) sampleCount() int {
	return len(clip.pcm) / 2
}

// sampleAt returns the little-endian 16-bit sample at the given flat index.
func (clip AudioClip) sampleAt(index int) int16 {
	return int16(binary.LittleEndian.Uint16(clip.pcm[index*2 : index*2+2]))
}

type waveFormat struct {
	audioFormat   uint16
	channels      uint16
	sampleRate    uint32
	byteRate      uint32
	blockAlign    uint16
	bitsPerSample uint16
}

func InspectAudio(data []byte) (AudioClip, error) {
	if len(data) < 12 {
		return AudioClip{}, fmt.Errorf("audio payload is shorter than a RIFF header")
	}
	if string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return AudioClip{}, fmt.Errorf("audio payload is not RIFF/WAVE")
	}
	riffSize := int(binary.LittleEndian.Uint32(data[4:8]))
	if riffSize < 4 || riffSize+8 > len(data) {
		return AudioClip{}, fmt.Errorf("RIFF size does not fit the payload")
	}

	var format waveFormat
	var pcm []byte
	haveFormat := false
	offset := 12
	for offset < len(data) {
		if offset+8 > len(data) {
			return AudioClip{}, fmt.Errorf("truncated WAV chunk header")
		}
		chunkID := string(data[offset : offset+4])
		chunkSize := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		if chunkSize < 0 || offset+8+chunkSize > len(data) {
			return AudioClip{}, fmt.Errorf("WAV chunk %q exceeds payload", chunkID)
		}
		body := data[offset+8 : offset+8+chunkSize]
		switch chunkID {
		case "fmt ":
			if chunkSize < 16 {
				return AudioClip{}, fmt.Errorf("WAV format chunk is too short")
			}
			format = waveFormat{
				audioFormat:   binary.LittleEndian.Uint16(body[0:2]),
				channels:      binary.LittleEndian.Uint16(body[2:4]),
				sampleRate:    binary.LittleEndian.Uint32(body[4:8]),
				byteRate:      binary.LittleEndian.Uint32(body[8:12]),
				blockAlign:    binary.LittleEndian.Uint16(body[12:14]),
				bitsPerSample: binary.LittleEndian.Uint16(body[14:16]),
			}
			haveFormat = true
		case "data":
			pcm = body
		}
		offset += 8 + chunkSize
		if chunkSize%2 == 1 {
			offset++
		}
	}
	if !haveFormat {
		return AudioClip{}, fmt.Errorf("WAV format chunk is missing")
	}
	if pcm == nil {
		return AudioClip{}, fmt.Errorf("WAV data chunk is missing")
	}
	if format.audioFormat != 1 {
		return AudioClip{}, fmt.Errorf("only integer PCM audio is accepted")
	}
	if format.channels != 1 && format.channels != 2 {
		return AudioClip{}, fmt.Errorf("audio must have one or two channels")
	}
	if format.bitsPerSample != 16 {
		return AudioClip{}, fmt.Errorf("audio must use 16-bit samples")
	}
	if format.sampleRate < 8000 || format.sampleRate > 192000 {
		return AudioClip{}, fmt.Errorf("sample rate is outside the accepted range")
	}
	blockAlign := int(format.channels) * 2
	if int(format.blockAlign) != blockAlign {
		return AudioClip{}, fmt.Errorf("WAV block alignment is inconsistent")
	}
	if len(pcm) == 0 || len(pcm)%blockAlign != 0 {
		return AudioClip{}, fmt.Errorf("WAV sample bytes are incomplete")
	}

	info := describeAudio(
		int(format.sampleRate),
		int(format.channels),
		len(pcm)/blockAlign,
		pcm,
		len(data),
		DigestBytes(data),
	)
	return AudioClip{Info: info, pcm: pcm}, nil
}

func describeAudio(sampleRate, channels, frames int, pcm []byte, byteCount int, digest string) domain.AudioInfo {
	peak := 0.0
	var squareSum float64
	sampleCount := len(pcm) / 2
	for index := 0; index < sampleCount; index++ {
		value := float64(int16(binary.LittleEndian.Uint16(pcm[index*2 : index*2+2])))
		if value < 0 {
			value = -value
		}
		if value > peak {
			peak = value
		}
		squareSum += value * value
	}
	rms := 0.0
	if sampleCount > 0 {
		rms = math.Sqrt(squareSum / float64(sampleCount))
	}
	return domain.AudioInfo{
		SampleRate: sampleRate,
		Channels:   channels,
		BitDepth:   16,
		FrameCount: frames,
		DurationMS: frames * 1000 / sampleRate,
		PeakDBFS:   amplitudeDBFS(peak),
		RMSDBFS:    amplitudeDBFS(rms),
		Digest:     digest,
		ByteCount:  byteCount,
	}
}

func amplitudeDBFS(value float64) float64 {
	if value <= 0 {
		return -120
	}
	db := 20 * math.Log10(value/32768)
	if db < -120 {
		return -120
	}
	return math.Round(db*100) / 100
}

func EncodePCM16(sampleRate, channels int, samples []int16) ([]byte, error) {
	if sampleRate < 8000 || sampleRate > 192000 {
		return nil, fmt.Errorf("unsupported sample rate")
	}
	if channels != 1 && channels != 2 {
		return nil, fmt.Errorf("unsupported channel count")
	}
	if len(samples)%channels != 0 {
		return nil, fmt.Errorf("sample count is not channel aligned")
	}
	dataSize := len(samples) * 2
	payload := make([]byte, 44+dataSize)
	copy(payload[:44], pcm16Header(sampleRate, channels, dataSize))
	for index, sample := range samples {
		binary.LittleEndian.PutUint16(payload[44+index*2:46+index*2], uint16(sample))
	}
	return payload, nil
}

// pcm16Header returns the canonical 44-byte RIFF/WAVE header the service emits
// for 16-bit PCM. Every encoder path uses it so output bytes stay identical.
func pcm16Header(sampleRate, channels, dataSize int) []byte {
	blockAlign := channels * 2
	header := make([]byte, 44)
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], uint32(36+dataSize))
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1)
	binary.LittleEndian.PutUint16(header[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(header[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(header[28:32], uint32(sampleRate*blockAlign))
	binary.LittleEndian.PutUint16(header[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(header[34:36], 16)
	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], uint32(dataSize))
	return header
}
