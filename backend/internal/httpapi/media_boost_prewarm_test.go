package httpapi

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/yexca/kikoto/backend/internal/config"
)

// TestBoostPrewarmCandidatesSelectsLocalAudioOnly checks the pass sees
// exactly the available local audio locations: no remote or cache rows,
// no unavailable rows, no non-audio items.
func TestBoostPrewarmCandidatesSelectsLocalAudioOnly(t *testing.T) {
	db := openMigratedTestDB(t)
	if _, err := db.Exec("INSERT INTO work (primary_code, title) VALUES ('RJ00000001', 'Prewarm Work')"); err != nil {
		t.Fatal(err)
	}
	var workID, sourceID int64
	if err := db.QueryRow("SELECT id FROM work WHERE primary_code = 'RJ00000001'").Scan(&workID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO file_source (code, display_name, source_type) VALUES ('local-test', 'Local Test', 'local')"); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT id FROM file_source WHERE code = 'local-test'").Scan(&sourceID); err != nil {
		t.Fatal(err)
	}
	insert := func(fingerprint string, hasAudio int, locationType, path, availability string) {
		t.Helper()
		result, err := db.Exec("INSERT INTO media_item (work_id, kind, title, fingerprint, has_audio) VALUES (?, 'audio', 'Track', ?, ?)", workID, fingerprint, hasAudio)
		if err != nil {
			t.Fatal(err)
		}
		itemID, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO media_file_location (media_item_id, file_source_id, location_type, path, availability) VALUES (?, ?, ?, ?, ?)", itemID, sourceID, locationType, path, availability); err != nil {
			t.Fatal(err)
		}
	}
	insert("want", 1, "local", "RJ00000001/track.wav", "available")
	insert("skip-unavailable", 1, "local", "RJ00000001/gone.wav", "missing")
	insert("skip-cache", 1, "cache", "RJ00000001/cached.wav", "available")
	insert("skip-image", 0, "local", "RJ00000001/cover.jpg", "available")

	server := NewServer(db, config.Config{})
	candidates, err := server.boostPrewarmCandidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].relativePath != "RJ00000001/track.wav" {
		t.Fatalf("candidates = %+v, want only the available local audio location", candidates)
	}
}

// TestBoostPrewarmPreparesBoostedEntry runs one pass over a quiet WAV and
// expects the boosted FLAC entry in the cache afterwards.
func TestBoostPrewarmPreparesBoostedEntry(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	dataRoot := t.TempDir()
	cacheRoot := t.TempDir()
	workDir := filepath.Join(dataRoot, "RJ00000002")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(workDir, "quiet.wav")
	command := exec.Command(ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100", "-t", "3", "-af", "volume=0.04", input)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create quiet WAV: %v (%s)", err, output)
	}
	db := openMigratedTestDB(t)
	if _, err := db.Exec("INSERT INTO work (primary_code, title) VALUES ('RJ00000002', 'Prewarm Work')"); err != nil {
		t.Fatal(err)
	}
	var workID, sourceID int64
	if err := db.QueryRow("SELECT id FROM work WHERE primary_code = 'RJ00000002'").Scan(&workID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO file_source (code, display_name, source_type) VALUES ('local-test', 'Local Test', 'local')"); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT id FROM file_source WHERE code = 'local-test'").Scan(&sourceID); err != nil {
		t.Fatal(err)
	}
	result, err := db.Exec("INSERT INTO media_item (work_id, kind, title, fingerprint, has_audio) VALUES (?, 'audio', 'Track', 'prewarm-track', 1)", workID)
	if err != nil {
		t.Fatal(err)
	}
	itemID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	locationResult, err := db.Exec("INSERT INTO media_file_location (media_item_id, file_source_id, location_type, path, availability) VALUES (?, ?, 'local', 'RJ00000002/quiet.wav', 'available')", itemID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	locationID, err := locationResult.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}

	// The test volume is small; the production floor would pause the pass.
	previousFloor := boostPrewarmMinFreeBytes
	boostPrewarmMinFreeBytes = 0
	defer func() { boostPrewarmMinFreeBytes = previousFloor }()

	server := NewServer(db, config.Config{DataRoot: dataRoot, CacheRoot: cacheRoot})
	server.runBoostPrewarm(context.Background())

	info, err := os.Stat(input)
	if err != nil {
		t.Fatal(err)
	}
	gainDB := server.storedBoostGainDB(context.Background(), locationID, input)
	if gainDB <= 0 {
		t.Fatalf("prewarm left the quiet track unmeasured (gain %v)", gainDB)
	}
	spec := audioTranscodeSpecFor(gainDB, true)
	output, err := safeCachePath(cacheRoot, audioTranscodeCachePathFor(input, info, spec))
	if err != nil {
		t.Fatal(err)
	}
	if !validCachedAudio(output, spec.maximumBytes) {
		t.Fatalf("prewarm did not prepare the boosted entry at %s", output)
	}
}

// TestBoostPrewarmDisabledSkipsPass checks that with the
// boost_prewarm_enabled setting off, a pass leaves every track
// unmeasured and prepares no cache entry.
func TestBoostPrewarmDisabledSkipsPass(t *testing.T) {
	dataRoot := t.TempDir()
	cacheRoot := t.TempDir()
	workDir := filepath.Join(dataRoot, "RJ00000003")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(workDir, "quiet.wav")
	if err := os.WriteFile(input, []byte("not audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	db := openMigratedTestDB(t)
	if _, err := db.Exec("INSERT INTO work (primary_code, title) VALUES ('RJ00000003', 'Prewarm Work')"); err != nil {
		t.Fatal(err)
	}
	var workID, sourceID int64
	if err := db.QueryRow("SELECT id FROM work WHERE primary_code = 'RJ00000003'").Scan(&workID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO file_source (code, display_name, source_type) VALUES ('local-test', 'Local Test', 'local')"); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT id FROM file_source WHERE code = 'local-test'").Scan(&sourceID); err != nil {
		t.Fatal(err)
	}
	result, err := db.Exec("INSERT INTO media_item (work_id, kind, title, fingerprint, has_audio) VALUES (?, 'audio', 'Track', 'prewarm-track', 1)", workID)
	if err != nil {
		t.Fatal(err)
	}
	itemID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	locationResult, err := db.Exec("INSERT INTO media_file_location (media_item_id, file_source_id, location_type, path, availability) VALUES (?, ?, 'local', 'RJ00000003/quiet.wav', 'available')", itemID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	locationID, err := locationResult.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO app_setting (key, value_json) VALUES (?, 'false')", boostPrewarmEnabledSetting); err != nil {
		t.Fatal(err)
	}

	server := NewServer(db, config.Config{DataRoot: dataRoot, CacheRoot: cacheRoot})
	server.runBoostPrewarm(context.Background())

	if gainDB := server.storedBoostGainDB(context.Background(), locationID, input); gainDB != 0 {
		t.Fatalf("disabled prewarm measured the track (gain %v)", gainDB)
	}
	cacheFiles := 0
	if err := filepath.WalkDir(cacheRoot, func(path string, entry os.DirEntry, err error) error {
		if err == nil && entry.Type().IsRegular() {
			cacheFiles++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if cacheFiles != 0 {
		t.Fatalf("disabled prewarm prepared %d cache files", cacheFiles)
	}
}
