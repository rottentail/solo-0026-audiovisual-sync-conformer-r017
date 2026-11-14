package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/domain"
)

type Repository struct {
	root  string
	locks *keyLocks
}

func New(root string) (*Repository, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve data root: %w", err)
	}
	for _, name := range []string{"takes", "analyses", "conforms", "conforms/assets", "tmp"} {
		if err := os.MkdirAll(filepath.Join(absolute, name), 0o750); err != nil {
			return nil, fmt.Errorf("create data namespace %s: %w", name, err)
		}
	}
	return &Repository{root: absolute, locks: newKeyLocks()}, nil
}

func (r *Repository) Root() string {
	return r.root
}

func (r *Repository) takeDir(id string) string {
	return filepath.Join(r.root, "takes", id)
}

func (r *Repository) takeRecordPath(id string) string {
	return filepath.Join(r.takeDir(id), "record.json")
}

func (r *Repository) analysisPath(id string) string {
	return filepath.Join(r.root, "analyses", id+".json")
}

func (r *Repository) conformPath(id string) string {
	return filepath.Join(r.root, "conforms", id+".json")
}

func (r *Repository) bundleDir(id string) string {
	return filepath.Join(r.root, "conforms", "assets", id)
}

func (r *Repository) GetTake(id string) (domain.MediaTake, error) {
	var record domain.MediaTake
	if err := readJSON(r.takeRecordPath(id), &record); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return domain.MediaTake{}, domain.NewError("take_not_found", 404, "take %s does not exist", id)
		}
		return domain.MediaTake{}, fmt.Errorf("read take: %w", err)
	}
	return record, nil
}

func (r *Repository) AdmitTake(
	record domain.MediaTake,
	audio []byte,
	video []byte,
) (domain.MediaTake, bool, error) {
	lock := r.locks.forKey("take:" + record.ID)
	lock.Lock()
	defer lock.Unlock()

	existing, err := r.GetTake(record.ID)
	switch {
	case err == nil:
		if existing.Name == record.Name &&
			existing.Note == record.Note &&
			existing.Audio.Digest == record.Audio.Digest &&
			existing.Video.Digest == record.Video.Digest {
			return existing, false, nil
		}
		return domain.MediaTake{}, false, domain.NewError(
			"take_conflict",
			409,
			"take %s already exists with different media or metadata",
			record.ID,
		)
	case !domain.IsErrorCode(err, "take_not_found"):
		return domain.MediaTake{}, false, err
	}

	tempDir, err := os.MkdirTemp(filepath.Join(r.root, "tmp"), "take-"+record.ID+"-*")
	if err != nil {
		return domain.MediaTake{}, false, fmt.Errorf("create take staging directory: %w", err)
	}
	defer os.RemoveAll(tempDir)
	if err := writeFileAtomic(filepath.Join(tempDir, "audio.wav"), audio); err != nil {
		return domain.MediaTake{}, false, err
	}
	if err := writeFileAtomic(filepath.Join(tempDir, "picture.y4m"), video); err != nil {
		return domain.MediaTake{}, false, err
	}
	recordData, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return domain.MediaTake{}, false, fmt.Errorf("encode take record: %w", err)
	}
	recordData = append(recordData, '\n')
	if err := writeFileAtomic(filepath.Join(tempDir, "record.json"), recordData); err != nil {
		return domain.MediaTake{}, false, err
	}
	if err := syncDirectory(tempDir); err != nil {
		return domain.MediaTake{}, false, fmt.Errorf("sync take staging directory: %w", err)
	}
	if err := os.Rename(tempDir, r.takeDir(record.ID)); err != nil {
		if errors.Is(err, os.ErrExist) {
			return domain.MediaTake{}, false, domain.NewError(
				"take_conflict",
				409,
				"take %s was created concurrently",
				record.ID,
			)
		}
		return domain.MediaTake{}, false, fmt.Errorf("commit take directory: %w", err)
	}
	return record, true, nil
}

func (r *Repository) ReadTakeMedia(id string) ([]byte, []byte, error) {
	audio, err := os.ReadFile(filepath.Join(r.takeDir(id), "audio.wav"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, domain.NewError("take_not_found", 404, "take %s does not exist", id)
		}
		return nil, nil, fmt.Errorf("read take audio: %w", err)
	}
	video, err := os.ReadFile(filepath.Join(r.takeDir(id), "picture.y4m"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, domain.NewError("take_media_missing", 500, "take %s picture is missing", id)
		}
		return nil, nil, fmt.Errorf("read take picture: %w", err)
	}
	return audio, video, nil
}

func (r *Repository) GetAnalysis(id string) (domain.SyncAnalysis, error) {
	var record domain.SyncAnalysis
	if err := readJSON(r.analysisPath(id), &record); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return domain.SyncAnalysis{}, domain.NewError(
				"analysis_not_found",
				404,
				"analysis %s does not exist",
				id,
			)
		}
		return domain.SyncAnalysis{}, fmt.Errorf("read analysis: %w", err)
	}
	return record, nil
}

func (r *Repository) CreateAnalysis(record domain.SyncAnalysis) (domain.SyncAnalysis, bool, error) {
	lock := r.locks.forKey("analysis:" + record.ID)
	lock.Lock()
	defer lock.Unlock()
	existing, err := r.GetAnalysis(record.ID)
	switch {
	case err == nil:
		if existing.TakeID == record.TakeID && existing.Policy == record.Policy {
			return existing, false, nil
		}
		return domain.SyncAnalysis{}, false, domain.NewError(
			"analysis_conflict",
			409,
			"analysis %s already exists with a different take or policy",
			record.ID,
		)
	case !domain.IsErrorCode(err, "analysis_not_found"):
		return domain.SyncAnalysis{}, false, err
	}
	if err := r.writeAnalysis(record); err != nil {
		return domain.SyncAnalysis{}, false, err
	}
	return record, true, nil
}

func (r *Repository) UpdateAnalysis(record domain.SyncAnalysis) error {
	lock := r.locks.forKey("analysis:" + record.ID)
	lock.Lock()
	defer lock.Unlock()
	current, err := r.GetAnalysis(record.ID)
	if err != nil {
		return err
	}
	if !domain.CanAdvanceAnalysis(current.State, record.State) {
		return domain.NewError(
			"analysis_transition_invalid",
			409,
			"analysis cannot move from %s to %s",
			current.State,
			record.State,
		)
	}
	return r.writeAnalysis(record)
}

func (r *Repository) writeAnalysis(record domain.SyncAnalysis) error {
	return writeJSON(r.analysisPath(record.ID), record)
}

func (r *Repository) GetConform(id string) (domain.ConformJob, error) {
	var record domain.ConformJob
	if err := readJSON(r.conformPath(id), &record); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return domain.ConformJob{}, domain.NewError(
				"conform_not_found",
				404,
				"conform %s does not exist",
				id,
			)
		}
		return domain.ConformJob{}, fmt.Errorf("read conform: %w", err)
	}
	return record, nil
}

func (r *Repository) CreateConform(record domain.ConformJob) (domain.ConformJob, bool, error) {
	lock := r.locks.forKey("conform:" + record.ID)
	lock.Lock()
	defer lock.Unlock()
	existing, err := r.GetConform(record.ID)
	switch {
	case err == nil:
		if existing.AnalysisID == record.AnalysisID &&
			existing.MaxCorrectionMS == record.MaxCorrectionMS {
			return existing, false, nil
		}
		return domain.ConformJob{}, false, domain.NewError(
			"conform_conflict",
			409,
			"conform %s already exists with a different analysis or correction ceiling",
			record.ID,
		)
	case !domain.IsErrorCode(err, "conform_not_found"):
		return domain.ConformJob{}, false, err
	}
	if err := r.writeConform(record); err != nil {
		return domain.ConformJob{}, false, err
	}
	return record, true, nil
}

func (r *Repository) UpdateConform(record domain.ConformJob) error {
	lock := r.locks.forKey("conform:" + record.ID)
	lock.Lock()
	defer lock.Unlock()
	current, err := r.GetConform(record.ID)
	if err != nil {
		return err
	}
	if !domain.CanAdvanceConform(current.State, record.State) {
		return domain.NewError(
			"conform_transition_invalid",
			409,
			"conform cannot move from %s to %s",
			current.State,
			record.State,
		)
	}
	return r.writeConform(record)
}

// ConformStaging is a private temporary directory where a conform bundle is
// rendered before the atomic directory rename publishes it. Content inside
// the staging directory is never visible to readers, and Abort removes it
// unless a commit has consumed it.
type ConformStaging struct {
	dir string
}

// StageConform creates the private staging directory for a conform bundle.
func (r *Repository) StageConform(id string) (*ConformStaging, error) {
	tempDir, err := os.MkdirTemp(filepath.Join(r.root, "tmp"), "conform-"+id+"-*")
	if err != nil {
		return nil, fmt.Errorf("create bundle staging directory: %w", err)
	}
	return &ConformStaging{dir: tempDir}, nil
}

// CreateBundle opens the staged bundle archive for exclusive writing.
func (s *ConformStaging) CreateBundle() (*os.File, error) {
	handle, err := os.OpenFile(filepath.Join(s.dir, "bundle.zip"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create staged bundle: %w", err)
	}
	return handle, nil
}

// WriteReceipt stores the staged receipt next to the staged bundle.
func (s *ConformStaging) WriteReceipt(data []byte) error {
	return writeFileAtomic(filepath.Join(s.dir, "receipt.json"), data)
}

// Abort removes the staging directory unless it was committed. It is safe to
// call after a successful commit.
func (s *ConformStaging) Abort() {
	if s.dir == "" {
		return
	}
	os.RemoveAll(s.dir)
	s.dir = ""
}

// CommitConform publishes the staged bundle directory with one atomic rename
// and marks the job completed. The staged content becomes visible only as a
// whole; a failed commit leaves no readable bundle behind.
func (r *Repository) CommitConform(
	record domain.ConformJob,
	staging *ConformStaging,
) (domain.ConformJob, error) {
	lock := r.locks.forKey("conform:" + record.ID)
	lock.Lock()
	defer lock.Unlock()
	current, err := r.GetConform(record.ID)
	if err != nil {
		return domain.ConformJob{}, err
	}
	if !domain.CanAdvanceConform(current.State, domain.ConformStateCompleted) {
		return domain.ConformJob{}, domain.NewError(
			"conform_transition_invalid",
			409,
			"conform cannot complete from %s",
			current.State,
		)
	}
	if err := syncDirectory(staging.dir); err != nil {
		return domain.ConformJob{}, fmt.Errorf("sync bundle staging directory: %w", err)
	}
	finalDir := r.bundleDir(record.ID)
	if err := os.Rename(staging.dir, finalDir); err != nil {
		if errors.Is(err, os.ErrExist) {
			return domain.ConformJob{}, domain.NewError(
				"conform_bundle_conflict",
				409,
				"bundle %s already exists",
				record.ID,
			)
		}
		return domain.ConformJob{}, fmt.Errorf("commit bundle directory: %w", err)
	}
	staging.dir = ""
	record.State = domain.ConformStateCompleted
	record.UpdatedAt = time.Now().UTC()
	if err := r.writeConform(record); err != nil {
		os.RemoveAll(finalDir)
		return domain.ConformJob{}, err
	}
	return record, nil
}

// OpenConformBundle verifies the stored bundle against its completed record
// and returns an open handle positioned at the start of the archive. The
// digest and byte-count checks are exactly the ones previously applied to the
// fully buffered bundle, but the file is streamed instead of read into
// memory. The caller owns the returned handle.
func (r *Repository) OpenConformBundle(id string) (*os.File, domain.ConformReceipt, int64, error) {
	record, err := r.GetConform(id)
	if err != nil {
		return nil, domain.ConformReceipt{}, 0, err
	}
	if record.State != domain.ConformStateCompleted {
		return nil, domain.ConformReceipt{}, 0, domain.NewError(
			"conform_not_ready",
			409,
			"conform %s is not completed",
			id,
		)
	}
	bundlePath := filepath.Join(r.bundleDir(id), "bundle.zip")
	handle, err := os.Open(bundlePath)
	if err != nil {
		return nil, domain.ConformReceipt{}, 0, fmt.Errorf("read conform bundle: %w", err)
	}
	size, digest, err := digestStream(handle)
	if err != nil {
		handle.Close()
		return nil, domain.ConformReceipt{}, 0, fmt.Errorf("read conform bundle: %w", err)
	}
	if size != int64(record.BundleBytes) || digest != record.BundleDigest {
		handle.Close()
		return nil, domain.ConformReceipt{}, 0, domain.NewError(
			"bundle_integrity_failed",
			500,
			"stored bundle does not match its completed record",
		)
	}
	if _, err := handle.Seek(0, io.SeekStart); err != nil {
		handle.Close()
		return nil, domain.ConformReceipt{}, 0, fmt.Errorf("read conform bundle: %w", err)
	}
	var receipt domain.ConformReceipt
	if err := readJSON(filepath.Join(r.bundleDir(id), "receipt.json"), &receipt); err != nil {
		handle.Close()
		return nil, domain.ConformReceipt{}, 0, fmt.Errorf("read conform receipt: %w", err)
	}
	return handle, receipt, size, nil
}

func (r *Repository) writeConform(record domain.ConformJob) error {
	return writeJSON(r.conformPath(record.ID), record)
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode JSON record: %w", err)
	}
	data = append(data, '\n')
	return writeFileAtomic(path, data)
}

func readJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode JSON record: %w", err)
	}
	return nil
}
