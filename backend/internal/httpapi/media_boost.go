package httpapi

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/yexca/kikoto/backend/internal/localfs"
)

// Server-side loudness boost, a Sara fork feature. A client that cannot raise
// a quiet track itself (the mobile apps play through the system volume, and
// iOS ignores element volume) asks with ?boost=auto, and the server folds the
// raise into the transcode it already performs, so the delivered audio is
// louder on every client. The gain mirrors the client normalization in
// frontend/src/player/loudnessNormalization.ts: the same target, the same
// peak headroom, the same cap. Only boosts apply; cuts stay a client concern.
const (
	// The integrated loudness a boosted track is brought towards.
	serverBoostTargetLUFS = -18.0
	// A boost leaves this much headroom below the measured peak.
	serverBoostPeakHeadroomDB = 1.0
	// A boost never raises a track more than this.
	serverBoostMaxDB = 12.0
	// A boost below this is not worth a separate transcode.
	serverBoostMinDB = 0.5
	// Gains are rounded to this step, so nearby tracks share one cache entry.
	serverBoostStepDB = 0.5
)

// serverBoostRequested reports whether the request asks for a server-side
// loudness boost of a local file.
func serverBoostRequested(r *http.Request) bool {
	value := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("boost")))
	return value == "auto" || value == "1" || value == "true" || value == "yes"
}

// loudnessBoostGainDB is the boost, in decibels, that brings a measured track
// towards the target loudness without pushing its peak into clipping.
// Anything that is not a worthwhile boost answers 0.
func loudnessBoostGainDB(loudness mediaLoudnessDetail) float64 {
	if !isFinite(loudness.IntegratedLUFS) || !isFinite(loudness.PeakDBFS) {
		return 0
	}
	gainDB := serverBoostTargetLUFS - loudness.IntegratedLUFS
	if gainDB <= 0 {
		return 0
	}
	gainDB = math.Min(gainDB, -serverBoostPeakHeadroomDB-loudness.PeakDBFS)
	gainDB = math.Min(gainDB, serverBoostMaxDB)
	if gainDB < serverBoostMinDB {
		return 0
	}
	return math.Round(gainDB/serverBoostStepDB) * serverBoostStepDB
}

func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

// boostGainSuffix names one gain step inside cache keys and revisions, so a
// boosted transcode never mixes with the plain one or another gain.
func boostGainSuffix(gainDB float64) string {
	return fmt.Sprintf("b%03d", int(math.Round(gainDB*10)))
}

// boostVolumeFilter is the FFmpeg filter that applies a boost gain.
func boostVolumeFilter(gainDB float64) string {
	return fmt.Sprintf("volume=%.1fdB", gainDB)
}

// storedBoostGainDB answers the boost for the file version on disk, from its
// stored loudness measurement. A file without a measurement answers 0 and is
// measured in the background, so a later play of the same file can boost.
func (s *Server) storedBoostGainDB(ctx context.Context, locationID int64, path string) float64 {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return 0
	}
	version := localfs.FileVersion(info)
	stored, ok, err := s.storedMediaLoudness(ctx, locationID, version)
	if err != nil {
		return 0
	}
	if !ok {
		s.measureLoudnessInBackground(locationID, path)
		return 0
	}
	return loudnessBoostGainDB(stored)
}

// measureLoudnessInBackground measures one file off the request path, so a
// stream request never waits for a whole-file decode. The measurement itself
// deduplicates concurrent runs and remembers failures.
func (s *Server) measureLoudnessInBackground(locationID int64, path string) {
	if s.cfg.IsDemo() {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), loudnessMeasureTimeout)
		defer cancel()
		_, _ = s.mediaLoudness(ctx, locationID, path)
	}()
}

// boostedHLSProfile returns a copy of an audio HLS encode profile whose
// segments carry a loudness boost, under a cache directory of its own.
func boostedHLSProfile(profile *hlsEncodeProfile, gainDB float64) *hlsEncodeProfile {
	suffix := boostGainSuffix(gainDB)
	filter := boostVolumeFilter(gainDB)
	boosted := *profile
	boosted.name = profile.name + "-boost" + suffix
	boosted.cacheDir = profile.cacheDir + "-boost" + suffix
	switch profile {
	case &audioHLSProfile:
		boosted.args = func(source hlsPlaybackSource, outputDir string, startIndex int) []string {
			return audioHLSFFmpegArgs(source.Path, outputDir, startIndex, "-af", filter)
		}
	case &videoAudioHLSProfile:
		boosted.args = func(source hlsPlaybackSource, outputDir string, startIndex int) []string {
			return boostedVideoAudioHLSFFmpegArgs(source, outputDir, startIndex, filter)
		}
	}
	return &boosted
}

// boostedVideoAudioHLSFFmpegArgs is videoAudioHLSFFmpegArgs with a loudness
// boost folded into the padding filter chain.
func boostedVideoAudioHLSFFmpegArgs(source hlsPlaybackSource, outputDir string, startIndex int, filter string) []string {
	start := float64(startIndex) * hlsSegmentDurationSeconds
	return audioHLSFFmpegArgs(source.Path, outputDir, startIndex,
		"-af", "apad,"+filter, "-t", strconv.FormatFloat(source.Duration-start, 'f', 3, 64))
}
