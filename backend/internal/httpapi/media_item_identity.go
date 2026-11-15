package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// mediaItemFingerprintSuffix returns the identity shared by every fingerprint
// form of one work file: "<work code>:<path>". Local scans record
// "local:<code>:<path>", while remote track sync records
// "remote:<file source id>:<code>:<path>". Both forms describe the same file
// of the same work, so an upsert resolves an existing item under either form
// before inserting a second item for the file.
func mediaItemFingerprintSuffix(fingerprint string) (string, bool) {
	if suffix, ok := strings.CutPrefix(fingerprint, "local:"); ok {
		return suffix, true
	}
	if rest, ok := strings.CutPrefix(fingerprint, "remote:"); ok {
		_, suffix, found := strings.Cut(rest, ":")
		return suffix, found
	}
	return "", false
}

// selectMediaItemIDByFingerprint resolves the media item an upsert should
// update: an exact fingerprint match first, then an item of the same work
// recorded under another fingerprint form for the same file. It returns 0
// when the work has no item for the file yet.
func selectMediaItemIDByFingerprint(ctx context.Context, tx *sql.Tx, workID int64, fingerprint string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `
		SELECT id FROM media_item WHERE fingerprint = ? ORDER BY id ASC LIMIT 1
	`, fingerprint).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	suffix, ok := mediaItemFingerprintSuffix(fingerprint)
	if !ok {
		return 0, nil
	}
	if !strings.HasPrefix(fingerprint, "local:") {
		// The local form of a remote fingerprint is one exact string.
		err := tx.QueryRowContext(ctx, `
			SELECT id FROM media_item WHERE fingerprint = ? ORDER BY id ASC LIMIT 1
		`, "local:"+suffix).Scan(&id)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, fingerprint
		FROM media_item
		WHERE work_id = ? AND fingerprint LIKE 'remote:%'
		ORDER BY id ASC
	`, workID)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var candidateID int64
		var candidate string
		if err := rows.Scan(&candidateID, &candidate); err != nil {
			return 0, err
		}
		if candidateSuffix, ok := mediaItemFingerprintSuffix(candidate); ok && candidateSuffix == suffix {
			return candidateID, nil
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return 0, nil
}
