package service

import (
	"fmt"
	"io"
	"time"

	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/domain"
	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/media"
	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/storage"
)

type Service struct {
	repository *storage.Repository
}

func New(repository *storage.Repository) *Service {
	return &Service{repository: repository}
}

func (s *Service) Repository() *storage.Repository {
	return s.repository
}

func (s *Service) CreateTake(input domain.CreateTakeInput) (domain.TakeResult, error) {
	if err := domain.ValidateCreateTake(input); err != nil {
		return domain.TakeResult{}, err
	}
	if len(input.Audio) > domain.MaxAudioBytes {
		return domain.TakeResult{}, domain.NewError(
			"payload_too_large",
			413,
			"audio_base64 exceeds the %d byte limit",
			domain.MaxAudioBytes,
		)
	}
	if len(input.Video) > domain.MaxVideoBytes {
		return domain.TakeResult{}, domain.NewError(
			"payload_too_large",
			413,
			"video_base64 exceeds the %d byte limit",
			domain.MaxVideoBytes,
		)
	}
	audioClip, err := media.InspectAudio(input.Audio)
	if err != nil {
		return domain.TakeResult{}, domain.NewError("invalid_audio", 422, "%s", err)
	}
	videoClip, err := media.InspectVideo(input.Video)
	if err != nil {
		return domain.TakeResult{}, domain.NewError("invalid_video", 422, "%s", err)
	}
	now := time.Now().UTC()
	record := domain.MediaTake{
		ID:         input.TakeID,
		Name:       input.Name,
		Note:       input.Note,
		State:      domain.TakeStateAdmitted,
		Audio:      audioClip.Info,
		Video:      videoClip.Info,
		AdmittedAt: now,
	}
	record.Digest = media.CombineDigest(
		record.Name,
		record.Note,
		record.Audio.Digest,
		record.Video.Digest,
	)
	admitted, created, err := s.repository.AdmitTake(record, input.Audio, input.Video)
	if err != nil {
		return domain.TakeResult{}, err
	}
	return domain.TakeResult{Take: admitted, Created: created}, nil
}

func (s *Service) GetTake(id string) (domain.MediaTake, error) {
	if err := domain.ValidateIdentifier("take_id", id); err != nil {
		return domain.MediaTake{}, err
	}
	return s.repository.GetTake(id)
}

func (s *Service) Analyze(takeID string, input domain.CreateAnalysisInput) (domain.AnalysisResult, error) {
	if err := domain.ValidateIdentifier("take_id", takeID); err != nil {
		return domain.AnalysisResult{}, err
	}
	if err := domain.ValidateCreateAnalysis(input); err != nil {
		return domain.AnalysisResult{}, err
	}
	take, err := s.repository.GetTake(takeID)
	if err != nil {
		return domain.AnalysisResult{}, err
	}
	now := time.Now().UTC()
	record := domain.SyncAnalysis{
		ID:        input.AnalysisID,
		TakeID:    take.ID,
		State:     domain.AnalysisStateQueued,
		Policy:    domain.SyncPolicy{MinConfidence: input.MinConfidence, MaxOffsetMS: input.MaxOffsetMS},
		CreatedAt: now,
		UpdatedAt: now,
	}
	existing, created, err := s.repository.CreateAnalysis(record)
	if err != nil {
		return domain.AnalysisResult{}, err
	}
	if !created {
		return domain.AnalysisResult{Analysis: existing, Created: false}, nil
	}
	record.State = domain.AnalysisStateDetecting
	record.UpdatedAt = time.Now().UTC()
	if err := s.repository.UpdateAnalysis(record); err != nil {
		return domain.AnalysisResult{}, err
	}

	audio, video, err := s.repository.ReadTakeMedia(take.ID)
	if err != nil {
		return s.failAnalysis(record, "take_media_unavailable", err.Error())
	}
	measurement, err := media.AnalyzeSync(audio, video)
	if err != nil {
		return s.failAnalysis(record, "sync_detection_failed", err.Error())
	}
	record.AudioEventMS = intPointer(measurement.AudioEventMS)
	record.VideoEventMS = intPointer(measurement.VideoEventMS)
	record.OffsetMS = intPointer(measurement.OffsetMS)
	record.Confidence = measurement.Confidence
	record.DriftPPM = measurement.DriftPPM
	record.AudioProminence = measurement.AudioProminence
	record.VideoProminence = measurement.VideoProminence
	record.UpdatedAt = time.Now().UTC()

	if measurement.Confidence < record.Policy.MinConfidence {
		return s.failAnalysis(
			record,
			"low_confidence",
			fmt.Sprintf(
				"confidence %.3f is below the minimum %.3f",
				measurement.Confidence,
				record.Policy.MinConfidence,
			),
		)
	}
	if abs(measurement.OffsetMS) > record.Policy.MaxOffsetMS {
		return s.failAnalysis(
			record,
			"offset_exceeded",
			fmt.Sprintf(
				"offset %d ms exceeds the analysis limit of %d ms",
				abs(measurement.OffsetMS),
				record.Policy.MaxOffsetMS,
			),
		)
	}
	record.State = domain.AnalysisStateReady
	if err := s.repository.UpdateAnalysis(record); err != nil {
		return domain.AnalysisResult{}, err
	}
	return domain.AnalysisResult{Analysis: record, Created: true}, nil
}

func (s *Service) failAnalysis(
	record domain.SyncAnalysis,
	code string,
	message string,
) (domain.AnalysisResult, error) {
	record.State = domain.AnalysisStateFailed
	record.ErrorCode = code
	record.ErrorMessage = message
	record.UpdatedAt = time.Now().UTC()
	if err := s.repository.UpdateAnalysis(record); err != nil {
		return domain.AnalysisResult{}, err
	}
	return domain.AnalysisResult{Analysis: record, Created: true}, nil
}

func (s *Service) GetAnalysis(id string) (domain.SyncAnalysis, error) {
	if err := domain.ValidateIdentifier("analysis_id", id); err != nil {
		return domain.SyncAnalysis{}, err
	}
	return s.repository.GetAnalysis(id)
}

func (s *Service) CreateConform(
	analysisID string,
	input domain.CreateConformInput,
) (domain.ConformResult, error) {
	if err := domain.ValidateIdentifier("analysis_id", analysisID); err != nil {
		return domain.ConformResult{}, err
	}
	if err := domain.ValidateCreateConform(input); err != nil {
		return domain.ConformResult{}, err
	}
	analysis, err := s.repository.GetAnalysis(analysisID)
	if err != nil {
		return domain.ConformResult{}, err
	}
	if analysis.State != domain.AnalysisStateReady || analysis.OffsetMS == nil {
		return domain.ConformResult{}, domain.NewError(
			"analysis_not_ready",
			409,
			"analysis %s is not ready for a conform",
			analysisID,
		)
	}
	record := domain.ConformJob{
		ID:               input.ConformID,
		AnalysisID:       analysis.ID,
		TakeID:           analysis.TakeID,
		State:            domain.ConformStateQueued,
		MaxCorrectionMS:  input.MaxCorrectionMS,
		MeasuredOffsetMS: *analysis.OffsetMS,
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
	}
	existing, created, err := s.repository.CreateConform(record)
	if err != nil {
		return domain.ConformResult{}, err
	}
	if !created {
		return domain.ConformResult{Conform: existing, Created: false}, nil
	}
	if abs(record.MeasuredOffsetMS) > record.MaxCorrectionMS {
		record.State = domain.ConformStateFailed
		record.ErrorCode = "correction_limit_exceeded"
		record.ErrorMessage = fmt.Sprintf(
			"measured correction of %d ms exceeds the ceiling of %d ms",
			abs(record.MeasuredOffsetMS),
			record.MaxCorrectionMS,
		)
		record.UpdatedAt = time.Now().UTC()
		if err := s.repository.UpdateConform(record); err != nil {
			return domain.ConformResult{}, err
		}
		return domain.ConformResult{Conform: record, Created: true}, domain.NewError(
			record.ErrorCode,
			422,
			record.ErrorMessage,
		)
	}
	record.State = domain.ConformStateRendering
	record.UpdatedAt = time.Now().UTC()
	if err := s.repository.UpdateConform(record); err != nil {
		return domain.ConformResult{}, err
	}
	return s.renderConform(record)
}

func (s *Service) renderConform(record domain.ConformJob) (domain.ConformResult, error) {
	audio, video, err := s.repository.ReadTakeMedia(record.TakeID)
	if err != nil {
		return s.failConform(record, "take_media_unavailable", err.Error())
	}
	audioClip, frameDelta, err := media.CheckAlignment(audio, record.MeasuredOffsetMS, record.MaxCorrectionMS)
	if err != nil {
		return s.failConform(record, domain.AsError(err).Code, domain.AsError(err).Message)
	}
	videoClip, err := media.InspectVideo(video)
	if err != nil {
		return s.failConform(record, "video_conform_failed", err.Error())
	}
	take, err := s.repository.GetTake(record.TakeID)
	if err != nil {
		return s.failConform(record, "take_media_unavailable", err.Error())
	}
	record.AppliedOffsetMS = record.MeasuredOffsetMS
	record.AppliedSamples = frameDelta
	receipt := domain.ConformReceipt{
		SchemaVersion:    1,
		ConformID:        record.ID,
		AnalysisID:       record.AnalysisID,
		TakeID:           record.TakeID,
		MeasuredOffsetMS: record.MeasuredOffsetMS,
		AppliedOffsetMS:  record.AppliedOffsetMS,
		AppliedSamples:   record.AppliedSamples,
		InputTakeDigest:  take.Digest,
		CreatedAt:        record.CreatedAt,
	}
	staging, err := s.repository.StageConform(record.ID)
	if err != nil {
		return s.failConform(record, "bundle_commit_failed", err.Error())
	}
	defer staging.Abort()
	result, bundleDigest, bundleBytes, err := s.writeBundle(staging, media.ReviewBundleSpec{
		Audio:          audioClip,
		AppliedSamples: frameDelta,
		Video:          videoClip,
		Receipt:        receipt,
	})
	if err != nil {
		return s.failConform(record, "bundle_build_failed", err.Error())
	}
	record.AudioOutputDigest = result.AudioDigest
	record.VideoOutputDigest = result.VideoDigest
	record.BundleDigest = bundleDigest
	record.BundleBytes = int(bundleBytes)
	record.State = domain.ConformStateCommitting
	record.UpdatedAt = time.Now().UTC()
	if err := s.repository.UpdateConform(record); err != nil {
		return domain.ConformResult{}, err
	}
	completed, err := s.repository.CommitConform(record, staging)
	if err != nil {
		return s.failConform(record, "bundle_commit_failed", err.Error())
	}
	return domain.ConformResult{Conform: completed, Created: true}, nil
}

// writeBundle streams the review ZIP into the staging directory while
// hashing the archive, then stores the receipt beside it. Nothing is visible
// outside the staging directory until the atomic commit.
func (s *Service) writeBundle(
	staging *storage.ConformStaging,
	spec media.ReviewBundleSpec,
) (media.ReviewBundleResult, string, int64, error) {
	handle, err := staging.CreateBundle()
	if err != nil {
		return media.ReviewBundleResult{}, "", 0, err
	}
	digest := media.NewHashWriter(handle)
	result, err := media.WriteReviewBundle(digest, spec)
	if err != nil {
		handle.Close()
		return media.ReviewBundleResult{}, "", 0, err
	}
	if err := handle.Sync(); err != nil {
		handle.Close()
		return media.ReviewBundleResult{}, "", 0, fmt.Errorf("flush staged bundle: %w", err)
	}
	if err := handle.Close(); err != nil {
		return media.ReviewBundleResult{}, "", 0, fmt.Errorf("close staged bundle: %w", err)
	}
	if err := staging.WriteReceipt(result.ReceiptData); err != nil {
		return media.ReviewBundleResult{}, "", 0, err
	}
	return result, digest.Digest(), digest.Bytes(), nil
}

func (s *Service) failConform(
	record domain.ConformJob,
	code string,
	message string,
) (domain.ConformResult, error) {
	record.State = domain.ConformStateFailed
	record.ErrorCode = code
	record.ErrorMessage = message
	record.UpdatedAt = time.Now().UTC()
	if err := s.repository.UpdateConform(record); err != nil {
		return domain.ConformResult{}, err
	}
	return domain.ConformResult{Conform: record, Created: true}, domain.NewError(code, 422, message)
}

func (s *Service) GetConform(id string) (domain.ConformJob, error) {
	if err := domain.ValidateIdentifier("conform_id", id); err != nil {
		return domain.ConformJob{}, err
	}
	return s.repository.GetConform(id)
}

// OpenConformBundle verifies the stored bundle and returns an open stream for
// it. The integrity check is identical to the previous buffered read, but the
// archive is served from disk without an in-memory copy.
func (s *Service) OpenConformBundle(id string) (io.ReadCloser, domain.ConformReceipt, int64, error) {
	if err := domain.ValidateIdentifier("conform_id", id); err != nil {
		return nil, domain.ConformReceipt{}, 0, err
	}
	return s.repository.OpenConformBundle(id)
}

func intPointer(value int) *int {
	return &value
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
