package httpapi

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

const (
	audioTranscodeProfileVersion = "mp3-v1"
	// A boosted encode of a lossless source (media_boost.go) keeps the
	// source's own fidelity instead of folding the gain into an MP3.
	audioTranscodeLosslessProfileVersion = "flac-v1"
	audioTranscodeMaximumBytes           = int64(512 << 20)
	// Stay below the partial-file retention age so cleanup cannot mistake an
	// active encoder output for a file abandoned by a previous server process.
	audioTranscodeTimeout = 4 * time.Minute
	// FFmpeg's -fs stops successfully near the limit. Reject that boundary
	// rather than publishing an apparently successful, truncated audio file.
	audioTranscodeLimitMargin = int64(64 << 10)
	// A lossless encode is bounded by its source instead of a fixed cap: a
	// FLAC of a PCM source stays below the source size, and float sources
	// land near it, so the source size plus this headroom bounds the output.
	audioTranscodeLosslessMaximumBytes = int64(4 << 30)
	audioTranscodeLosslessHeadroom     = int64(64 << 20)
)

var errAudioTranscodeUnavailable = errors.New("audio transcoding is unavailable")

// audioTranscodeSpec describes one encode variant of a source file: the
// codec it is delivered in and the size bound its cache entry must stay
// under. A boosted variant (media_boost.go) is a different profile, so its
// cache entry never mixes with the plain encode or another gain.
type audioTranscodeSpec struct {
	name         string
	extension    string
	contentType  string
	maximumBytes int64
	gainDB       float64
}

func audioTranscodeProfile(gainDB float64) string {
	return audioTranscodeSpecFor(gainDB, false).name
}

// audioTranscodeSpecFor resolves the encode variant for one delivery. The
// lossless variant serves a boost of a lossless source; every other encode
// is the MP3 variant.
func audioTranscodeSpecFor(gainDB float64, lossless bool) audioTranscodeSpec {
	if lossless && gainDB > 0 {
		return audioTranscodeSpec{
			name:         audioTranscodeLosslessProfileVersion + "-boost" + boostGainSuffix(gainDB),
			extension:    ".flac",
			contentType:  "audio/flac",
			maximumBytes: audioTranscodeLosslessMaximumBytes,
			gainDB:       gainDB,
		}
	}
	name := audioTranscodeProfileVersion
	if gainDB > 0 {
		name += "-boost" + boostGainSuffix(gainDB)
	}
	return audioTranscodeSpec{
		name:         name,
		extension:    ".mp3",
		contentType:  "audio/mpeg",
		maximumBytes: audioTranscodeMaximumBytes,
		gainDB:       gainDB,
	}
}

func audioTranscodeCachePath(input string, info os.FileInfo, gainDB float64) string {
	return audioTranscodeCachePathFor(input, info, audioTranscodeSpecFor(gainDB, false))
}

func audioTranscodeCachePathFor(input string, info os.FileInfo, spec audioTranscodeSpec) string {
	revision := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%d", spec.name, input, info.Size(), info.ModTime().UnixNano())))
	return filepath.ToSlash(filepath.Join(transcodeCacheRootRelative, "audio", fmt.Sprintf("%x%s", revision, spec.extension)))
}

func (s *Server) serveCompatibleAudio(w http.ResponseWriter, r *http.Request, input string, inputInfo os.FileInfo, gainDB float64) {
	s.serveCompatibleAudioSpec(w, r, input, inputInfo, audioTranscodeSpecFor(gainDB, false))
}

func (s *Server) serveCompatibleAudioSpec(w http.ResponseWriter, r *http.Request, input string, inputInfo os.FileInfo, spec audioTranscodeSpec) {
	file, err := s.openCompatibleAudioSpec(r.Context(), input, inputInfo, spec)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		slog.Warn("compatible audio preparation failed", "path", input, "error", err)
		if errors.Is(err, errRealtimeResourceBusy) {
			w.Header().Set("Retry-After", "1")
			writeAPIError(w, http.StatusServiceUnavailable, "media_transcode_busy", "media playback is temporarily busy", true)
			return
		}
		if errors.Is(err, errAudioTranscodeUnavailable) {
			writeAPIError(w, http.StatusServiceUnavailable, "media_transcode_unavailable", "audio playback preparation failed", true)
			return
		}
		writeAPIError(w, http.StatusInsufficientStorage, "transcode_cache_unavailable", "audio playback cache is unavailable", true)
		return
	}
	defer func() { _ = file.Close() }()
	w.Header().Set("Content-Type", spec.contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("ETag", strconv.Quote(filepath.Base(file.Name())))
	// The cache mtime is LRU bookkeeping. Only the source revision determines
	// HTTP validators, so touching a reusable file does not invalidate If-Range.
	http.ServeContent(w, r, filepath.Base(file.Name()), inputInfo.ModTime(), file)
}

func (s *Server) openCompatibleAudio(ctx context.Context, input string, inputInfo os.FileInfo, gainDB float64) (*os.File, error) {
	return s.openCompatibleAudioSpec(ctx, input, inputInfo, audioTranscodeSpecFor(gainDB, false))
}

func (s *Server) openCompatibleAudioSpec(ctx context.Context, input string, inputInfo os.FileInfo, spec audioTranscodeSpec) (*os.File, error) {
	relPath := audioTranscodeCachePathFor(input, inputInfo, spec)
	output, err := safeCachePath(s.cfg.CacheRoot, relPath)
	if err != nil {
		return nil, err
	}
	// This preparation gate is separate from the published-file lock used by
	// eviction. Identical requests share one quota reservation and encoder.
	releasePreparation, err := s.audioTranscodeLocks.acquire(ctx, output)
	if err != nil {
		return nil, err
	}
	defer releasePreparation()
	s.transcodeCacheActivityMu.RLock()
	defer s.transcodeCacheActivityMu.RUnlock()
	releasePath, err := s.acquireCachePathLock(ctx, relPath)
	if err != nil {
		return nil, err
	}
	if validCachedAudio(output, spec.maximumBytes) {
		defer releasePath()
		return openCompleteAudio(output)
	}
	releasePath()
	failureKey := mediaPreparationFailureKey("audio-transcode", input, inputInfo)
	if s.mediaPreparationFailures.failedRecently(failureKey) {
		return nil, fmt.Errorf("%w: preparation failed recently", errAudioTranscodeUnavailable)
	}

	// Reserve before locking the path: LRU eviction must not wait for a path
	// held by a request that is itself waiting for the quota lock.
	releaseReservation, err := s.reserveTranscodeCache(ctx, spec.maximumBytes)
	if err != nil {
		return nil, err
	}
	defer releaseReservation()
	releasePath, err = s.acquireCachePathLock(ctx, relPath)
	if err != nil {
		return nil, err
	}
	defer releasePath()
	if !validCachedAudio(output, spec.maximumBytes) {
		if _, _, err := s.removeCacheFileUnlocked(relPath); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
			return nil, err
		}
		if err := s.generateCompatibleAudio(ctx, input, inputInfo, output, spec); err != nil {
			if errors.Is(err, errAudioTranscodeUnavailable) {
				s.mediaPreparationFailures.remember(failureKey)
			}
			return nil, err
		}
	}
	// Open before releasing publication/eviction locks, then return the handle.
	// A slow HTTP client must not hold the preparation gate or quota reservation.
	return openCompleteAudio(output)
}

func validCachedAudio(path string, maximumBytes int64) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0 && info.Size() < maximumBytes-audioTranscodeLimitMargin
}

func openCompleteAudio(path string) (*os.File, error) {
	file, err := openTranscodeCacheFile(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("completed audio is unavailable")
	}
	if time.Since(info.ModTime()) > 5*time.Minute {
		now := time.Now()
		_ = os.Chtimes(path, now, now)
	}
	return file, nil
}

func (s *Server) generateCompatibleAudio(ctx context.Context, input string, inputInfo os.FileInfo, output string, spec audioTranscodeSpec) error {
	maximumBytes := spec.maximumBytes
	if spec.extension == ".flac" {
		// The output tracks the source size, so reserve and bound by it
		// rather than by the profile ceiling.
		maximumBytes = inputInfo.Size() + audioTranscodeLosslessHeadroom
		if maximumBytes > spec.maximumBytes {
			maximumBytes = spec.maximumBytes
		}
	}
	encodeContext, cancel := context.WithTimeout(ctx, audioTranscodeTimeout)
	defer cancel()
	release, err := s.acquireRealtimeTranscode(encodeContext)
	if err != nil {
		return err
	}
	defer release()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return fmt.Errorf("%w: ffmpeg executable was not found", errAudioTranscodeUnavailable)
	}
	temporary, err := os.CreateTemp(filepath.Dir(output), ".audio-transcode-*.part")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	defer func() { _ = os.Remove(temporaryPath) }()
	command := exec.CommandContext(encodeContext, ffmpeg, compatibleAudioFFmpegArgsFor(input, temporaryPath, maximumBytes, spec)...)
	command.Stdout = io.Discard
	diagnostics := &transcodeDiagnosticOutput{}
	command.Stderr = diagnostics
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: command failed: %v: %s", errAudioTranscodeUnavailable, err, diagnostics.String())
	}
	info, err := os.Stat(temporaryPath)
	if err != nil {
		return err
	}
	if info.Size() <= 0 || info.Size() >= maximumBytes-audioTranscodeLimitMargin {
		return fmt.Errorf("%w: output is empty or reached its byte limit", errAudioTranscodeUnavailable)
	}
	currentInput, err := os.Stat(input)
	if err != nil || !currentInput.Mode().IsRegular() || currentInput.Size() != inputInfo.Size() || !currentInput.ModTime().Equal(inputInfo.ModTime()) {
		return fmt.Errorf("%w: source changed during preparation", errAudioTranscodeUnavailable)
	}
	file, err := os.OpenFile(temporaryPath, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(temporaryPath, output)
}

func compatibleAudioFFmpegArgs(input string, output string, maximumBytes int64, gainDB float64) []string {
	return compatibleAudioFFmpegArgsFor(input, output, maximumBytes, audioTranscodeSpecFor(gainDB, false))
}

func compatibleAudioFFmpegArgsFor(input string, output string, maximumBytes int64, spec audioTranscodeSpec) []string {
	args := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-threads", "2", "-filter_threads", "1", "-max_alloc", "268435456",
		"-protocol_whitelist", "file,pipe", "-i", input,
		"-map", "0:a:0", "-map_metadata", "-1", "-vn", "-sn", "-dn",
	}
	if spec.gainDB > 0 {
		args = append(args, "-af", boostVolumeFilter(spec.gainDB))
	}
	if spec.extension == ".flac" {
		// Keep the source sample format family, rate, and channels: the
		// encode is lossless apart from the gain itself.
		return append(args,
			"-c:a", "flac", "-threads:a", "1", "-compression_level", "5",
			"-fs", strconv.FormatInt(maximumBytes-audioTranscodeLimitMargin/2, 10),
			"-f", "flac", output,
		)
	}
	return append(args,
		"-c:a", "libmp3lame", "-threads:a", "1", "-q:a", "4",
		"-ac", "2", "-ar", "44100", "-write_xing", "1",
		"-fs", strconv.FormatInt(maximumBytes-audioTranscodeLimitMargin/2, 10),
		"-f", "mp3", output,
	)
}
