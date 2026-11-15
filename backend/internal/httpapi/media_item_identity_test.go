package httpapi

import (
	"context"
	"testing"

	"github.com/yexca/kikoto/backend/internal/localfs"
)

// A fetched work folder is also visible to the local scan. The scan must
// adopt the item the remote track sync recorded for a file instead of
// inserting a second item under the local fingerprint form.
func TestDetectedMediaUpsertAdoptsRemoteFingerprintedItem(t *testing.T) {
	db := openMigratedTestDB(t)
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	folder := localfs.WorkFolder{Code: "RJ00000041", Title: "Fetched work", RelPath: "RJ00000041"}
	workID, err := upsertDetectedWork(context.Background(), tx, folder)
	if err != nil {
		t.Fatal(err)
	}
	remoteItem, err := tx.Exec(`
		INSERT INTO media_item (work_id, kind, title, track_no, size_bytes, fingerprint)
		VALUES (?, 'audio', 'track.mp3', 1, 100, 'remote:2:RJ00000041:track.mp3')
	`, workID)
	if err != nil {
		t.Fatal(err)
	}
	remoteItemID, err := remoteItem.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`
		INSERT INTO media_item (work_id, kind, title, track_no, size_bytes, fingerprint)
		VALUES (?, 'audio', 'other.mp3', 2, 100, 'remote:2:RJ00000041:other.mp3')
	`, workID); err != nil {
		t.Fatal(err)
	}

	file := localfs.LocalFile{RelPath: "RJ00000041/track.mp3", WorkRelPath: "track.mp3", Title: "track", SizeBytes: 200}
	itemID, err := upsertDetectedMediaItem(context.Background(), tx, workID, folder, file, "audio", 1)
	if err != nil {
		t.Fatal(err)
	}
	if itemID != remoteItemID {
		t.Fatalf("scan upsert = item %d, want adopted remote item %d", itemID, remoteItemID)
	}
	var items, size int
	if err := tx.QueryRow("SELECT COUNT(*) FROM media_item WHERE work_id = ?", workID).Scan(&items); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow("SELECT size_bytes FROM media_item WHERE id = ?", remoteItemID).Scan(&size); err != nil {
		t.Fatal(err)
	}
	if items != 2 || size != 200 {
		t.Fatalf("after adoption = %d items, adopted size %d, want 2 items and 200", items, size)
	}
}

// The mirror direction: remote track sync must adopt the item a local scan
// recorded for the same file instead of inserting a remote-fingerprinted
// duplicate.
func TestRemoteMediaUpsertAdoptsLocalFingerprintedItem(t *testing.T) {
	db := openMigratedTestDB(t)
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	folder := localfs.WorkFolder{Code: "RJ00000042", Title: "Scanned work", RelPath: "RJ00000042"}
	workID, err := upsertDetectedWork(context.Background(), tx, folder)
	if err != nil {
		t.Fatal(err)
	}
	localItem, err := tx.Exec(`
		INSERT INTO media_item (work_id, kind, title, track_no, size_bytes, fingerprint)
		VALUES (?, 'audio', 'track', 1, 100, 'local:RJ00000042:track.mp3')
	`, workID)
	if err != nil {
		t.Fatal(err)
	}
	localItemID, err := localItem.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}

	itemID, err := upsertRemoteMediaItem(context.Background(), tx, workID, nil, "audio", "track.mp3", 0, nil, true, int64(300), "remote:2:RJ00000042:track.mp3")
	if err != nil {
		t.Fatal(err)
	}
	if itemID != localItemID {
		t.Fatalf("remote upsert = item %d, want adopted local item %d", itemID, localItemID)
	}
	var items, size int
	if err := tx.QueryRow("SELECT COUNT(*) FROM media_item WHERE work_id = ?", workID).Scan(&items); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow("SELECT size_bytes FROM media_item WHERE id = ?", localItemID).Scan(&size); err != nil {
		t.Fatal(err)
	}
	if items != 1 || size != 300 {
		t.Fatalf("after adoption = %d items, adopted size %d, want 1 item and 300", items, size)
	}
}
