package httpapi

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yexca/kikoto/backend/internal/config"
)

func TestLoudnessBoostGainDB(t *testing.T) {
	tests := []struct {
		name       string
		integrated float64
		peak       float64
		want       float64
	}{
		{name: "quiet track boosts to target", integrated: -30, peak: -9, want: 8},
		{name: "peak limits the boost", integrated: -30, peak: -3, want: 2},
		{name: "boost is capped", integrated: -45, peak: -20, want: 12},
		{name: "at target stays", integrated: -18, peak: -6, want: 0},
		{name: "loud track stays", integrated: -10, peak: -1, want: 0},
		{name: "small boost rounds away", integrated: -18.4, peak: -6, want: 0},
		{name: "half step rounds", integrated: -24.3, peak: -12, want: 6.5},
		{name: "peak at full scale blocks", integrated: -30, peak: 0, want: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := loudnessBoostGainDB(mediaLoudnessDetail{IntegratedLUFS: test.integrated, PeakDBFS: test.peak, PeakKind: "true"})
			if got != test.want {
				t.Fatalf("loudnessBoostGainDB(%v, %v) = %v, want %v", test.integrated, test.peak, got, test.want)
			}
		})
	}
}

func TestLoudnessBoostGainDBUnmeasured(t *testing.T) {
	if got := loudnessBoostGainDB(mediaLoudnessDetail{IntegratedLUFS: math.Inf(-1), PeakDBFS: 0}); got != 0 {
		t.Fatalf("silent measurement boosted by %v", got)
	}
	if got := loudnessBoostGainDB(mediaLoudnessDetail{IntegratedLUFS: math.NaN(), PeakDBFS: math.NaN()}); got != 0 {
		t.Fatalf("invalid measurement boosted by %v", got)
	}
}

func TestServerBoostRequested(t *testing.T) {
	for _, target := range []string{"/api/media/1/stream?boost=auto", "/api/media/1/stream?boost=1", "/api/media/1/stream?boost=TRUE"} {
		if !serverBoostRequested(httptest.NewRequest(http.MethodGet, target, nil)) {
			t.Fatalf("%s did not request a boost", target)
		}
	}
	for _, target := range []string{"/api/media/1/stream", "/api/media/1/stream?boost=off", "/api/media/1/stream?boost="} {
		if serverBoostRequested(httptest.NewRequest(http.MethodGet, target, nil)) {
			t.Fatalf("%s requested a boost", target)
		}
	}
}

func TestCompatibleAudioFFmpegArgsBoost(t *testing.T) {
	plain := compatibleAudioFFmpegArgs("in.wav", "out.mp3", audioTranscodeMaximumBytes, 0)
	for _, arg := range plain {
		if arg == "-af" {
			t.Fatal("plain encode carries an audio filter")
		}
	}
	boosted := compatibleAudioFFmpegArgs("in.wav", "out.mp3", audioTranscodeMaximumBytes, 6)
	joined := strings.Join(boosted, " ")
	if !strings.Contains(joined, "-af volume=6.0dB") {
		t.Fatalf("boosted encode args = %v", boosted)
	}
}

func TestAudioTranscodeProfileSeparatesBoost(t *testing.T) {
	if audioTranscodeProfile(0) != audioTranscodeProfileVersion {
		t.Fatal("plain profile changed")
	}
	if audioTranscodeProfile(6) == audioTranscodeProfile(6.5) {
		t.Fatal("different gains share a profile")
	}
}

// A boosted compatible transcode of a quiet file measures louder by about
// the computed gain, which is the whole point of the feature.
func TestBoostedTranscodeRaisesLoudness(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "quiet.wav")
	command := exec.Command(ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100", "-t", "3", "-af", "volume=0.04", input)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create quiet WAV: %v (%s)", err, output)
	}
	server := NewServer(nil, config.Config{CacheRoot: dir})
	ctx := context.Background()
	before, err := server.measureMediaLoudness(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	gainDB := loudnessBoostGainDB(before)
	if gainDB <= 0 {
		t.Fatalf("quiet file (integrated %.1f LUFS, peak %.1f dBFS) got no boost", before.IntegratedLUFS, before.PeakDBFS)
	}
	info, err := os.Stat(input)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "boosted.mp3")
	if err := server.generateCompatibleAudio(ctx, input, info, output, audioTranscodeMaximumBytes, gainDB); err != nil {
		t.Fatal(err)
	}
	after, err := server.measureMediaLoudness(ctx, output)
	if err != nil {
		t.Fatal(err)
	}
	raised := after.IntegratedLUFS - before.IntegratedLUFS
	if math.Abs(raised-gainDB) > 1.5 {
		t.Fatalf("boost raised loudness by %.1f dB, want about %.1f dB (before %.1f, after %.1f LUFS)",
			raised, gainDB, before.IntegratedLUFS, after.IntegratedLUFS)
	}
}

func TestBoostedHLSProfile(t *testing.T) {
	boosted := boostedHLSProfile(&audioHLSProfile, 6)
	if boosted == &audioHLSProfile {
		t.Fatal("boosted profile aliases the plain profile")
	}
	if boosted.cacheDir == audioHLSProfile.cacheDir || !strings.Contains(boosted.cacheDir, "b060") {
		t.Fatalf("boosted cache dir = %q", boosted.cacheDir)
	}
	args := boosted.args(hlsPlaybackSource{Path: "in.flac"}, "out", 0)
	if joined := strings.Join(args, " "); !strings.Contains(joined, "-af volume=6.0dB") {
		t.Fatalf("boosted HLS args = %v", args)
	}
	videoAudio := boostedHLSProfile(&videoAudioHLSProfile, 6)
	args = videoAudio.args(hlsPlaybackSource{Path: "in.mkv", Duration: 60}, "out", 0)
	if joined := strings.Join(args, " "); !strings.Contains(joined, "-af apad,volume=6.0dB") {
		t.Fatalf("boosted video-audio HLS args = %v", args)
	}
}
