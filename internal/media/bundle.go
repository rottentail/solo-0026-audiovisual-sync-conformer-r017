package media

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/domain"
)

// ReviewBundleSpec carries everything needed to stream a review bundle. The
// audio and video clips alias the admitted take payloads, and the receipt is
// completed with the output digests as the entries are written.
type ReviewBundleSpec struct {
	Audio          AudioClip
	AppliedSamples int
	Video          VideoClip
	Receipt        domain.ConformReceipt
}

// ReviewBundleResult reports the digests and receipt bytes produced while
// streaming the bundle. ReceiptData is byte-identical to the receipt entry
// stored inside the ZIP.
type ReviewBundleResult struct {
	ReceiptData []byte
	AudioDigest string
	VideoDigest string
}

// WriteReviewBundle streams the deterministic review ZIP to w: the aligned
// audio, the normalized picture, and the completed receipt. Entries are
// encoded straight from the shared take payloads, so the bundle never
// materializes as a whole in memory. The output bytes are identical to
// building the same archive in a buffer.
func WriteReviewBundle(w io.Writer, spec ReviewBundleSpec) (ReviewBundleResult, error) {
	writer := zip.NewWriter(w)

	audioEntry, err := newBundleEntry(writer, "audio-aligned.wav")
	if err != nil {
		return ReviewBundleResult{}, err
	}
	audioHash := NewHashWriter(audioEntry)
	if err := WriteAlignedWAV(audioHash, spec.Audio, spec.AppliedSamples); err != nil {
		return ReviewBundleResult{}, fmt.Errorf("write ZIP entry: %w", err)
	}

	videoEntry, err := newBundleEntry(writer, "video-conformed.y4m")
	if err != nil {
		return ReviewBundleResult{}, err
	}
	videoHash := NewHashWriter(videoEntry)
	if err := WriteY4M(videoHash, spec.Video.Info, spec.Video.Frames); err != nil {
		return ReviewBundleResult{}, fmt.Errorf("write ZIP entry: %w", err)
	}

	receipt := spec.Receipt
	receipt.AudioDigest = audioHash.Digest()
	receipt.VideoDigest = videoHash.Digest()
	receiptData, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return ReviewBundleResult{}, fmt.Errorf("encode receipt: %w", err)
	}
	receiptData = append(receiptData, '\n')

	receiptEntry, err := newBundleEntry(writer, "conform-receipt.json")
	if err != nil {
		return ReviewBundleResult{}, err
	}
	if _, err := receiptEntry.Write(receiptData); err != nil {
		return ReviewBundleResult{}, fmt.Errorf("write ZIP entry: %w", err)
	}
	if err := writer.Close(); err != nil {
		return ReviewBundleResult{}, fmt.Errorf("close ZIP bundle: %w", err)
	}
	return ReviewBundleResult{
		ReceiptData: receiptData,
		AudioDigest: audioHash.Digest(),
		VideoDigest: videoHash.Digest(),
	}, nil
}

func newBundleEntry(writer *zip.Writer, name string) (io.Writer, error) {
	header := &zip.FileHeader{
		Name:     name,
		Method:   zip.Store,
		Modified: time.Unix(0, 0).UTC(),
	}
	header.SetMode(0o600)
	entry, err := writer.CreateHeader(header)
	if err != nil {
		return nil, fmt.Errorf("create ZIP entry: %w", err)
	}
	return entry, nil
}
