package httpapi

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/yexca/kikoto/backend/internal/localfs"
)

// Boost prewarming, a Sara fork feature. Once an hour the server walks the
// local audio library and prepares the boosted delivery of every track
// whose loudness calls for one, so the first play of a boosted track is
// served from the transcode cache instead of waiting for its encode.
// Tracks without a measurement are measured first, in the same pass; the
// measurement slot and the transcode slots keep both off the playback
// critical path. A run is bounded in time and in files, and stops early
// when the cache volume runs low on space; the next run resumes where
// this one stopped, because a prepared entry is visible in the cache.
const (
	boostPrewarmInterval    = time.Hour
	boostPrewarmInitialWait = 10 * time.Minute
	boostPrewarmRunBudget   = 45 * time.Minute
	boostPrewarmMaxFiles    = 60
)

// Preparation stops while the cache volume has less than this free, so
// prewarming can never fill the disk under a running server. A variable
// so tests on small volumes can lower it.
var boostPrewarmMinFreeBytes = int64(5 << 30)

type boostPrewarmCandidate struct {
	locationID   int64
	relativePath string
}

// boostPrewarmCandidates lists the local audio locations a prewarm pass
// considers, oldest library entries first so coverage grows in a stable
// order across runs.
func (s *Server) boostPrewarmCandidates(ctx context.Context) ([]boostPrewarmCandidate, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT mfl.id, mfl.path
		FROM media_file_location mfl
		JOIN media_item mi ON mi.id = mfl.media_item_id
		WHERE mfl.location_type = 'local' AND mfl.availability = 'available' AND mi.has_audio = 1
		ORDER BY mfl.id
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	candidates := []boostPrewarmCandidate{}
	for rows.Next() {
		var candidate boostPrewarmCandidate
		if err := rows.Scan(&candidate.locationID, &candidate.relativePath); err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

// prewarmBoostGainDB answers the boost for one candidate, measuring the
// file first when its current version has no stored measurement. Unlike
// storedBoostGainDB it never defers the measurement to a background
// goroutine: the pass itself is the background.
func (s *Server) prewarmBoostGainDB(ctx context.Context, locationID int64, path string, info os.FileInfo) float64 {
	version := localfs.FileVersion(info)
	stored, ok, err := s.storedMediaLoudness(ctx, locationID, version)
	if err != nil {
		return 0
	}
	if ok {
		return loudnessBoostGainDB(stored)
	}
	measured, err := s.mediaLoudness(ctx, locationID, path)
	if err != nil {
		return 0
	}
	return loudnessBoostGainDB(measured)
}

// runBoostPrewarm performs one prewarm pass. It is safe to call on a
// timer: preparation locks make it join any identical encode already in
// flight, and every failure leaves the track for a later pass.
func (s *Server) runBoostPrewarm(ctx context.Context) {
	if s.cfg.IsDemo() || s.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, boostPrewarmRunBudget)
	defer cancel()
	candidates, err := s.boostPrewarmCandidates(ctx)
	if err != nil {
		slog.Error("boost prewarm candidate scan", "error", err)
		return
	}
	prepared := 0
	for _, candidate := range candidates {
		if prepared >= boostPrewarmMaxFiles || ctx.Err() != nil {
			break
		}
		path, err := safeResolvedPath(s.cfg.DataRoot, candidate.relativePath)
		if err != nil {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		gainDB := s.prewarmBoostGainDB(ctx, candidate.locationID, path, info)
		if gainDB <= 0 {
			continue
		}
		spec := audioTranscodeSpecFor(gainDB, s.losslessBoostSource(ctx, path))
		output, err := safeCachePath(s.cfg.CacheRoot, audioTranscodeCachePathFor(path, info, spec))
		if err != nil {
			continue
		}
		if validCachedAudio(output, spec.maximumBytes) {
			continue
		}
		if !boostPrewarmDiskHeadroom(s.cfg.CacheRoot) {
			slog.Warn("boost prewarm paused: cache volume is low on space")
			return
		}
		file, err := s.openCompatibleAudioSpec(ctx, path, info, spec)
		if err != nil {
			slog.Warn("boost prewarm preparation failed", "path", path, "error", err)
			continue
		}
		_ = file.Close()
		prepared++
	}
	if prepared > 0 {
		slog.Info("boost prewarm pass complete", "prepared", prepared)
	}
}

// boostPrewarmDiskHeadroom reports whether the cache volume still has
// room for another prepared encode.
func boostPrewarmDiskHeadroom(cacheRoot string) bool {
	snapshot, err := queryDiskSpace(cacheRoot)
	if err != nil {
		return false
	}
	return snapshot.Available >= uint64(boostPrewarmMinFreeBytes)
}
