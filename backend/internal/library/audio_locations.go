package library

import (
	"context"

	"github.com/yexca/kikoto/backend/internal/sqlutil"
)

// AudioLocation is one local audio file location in the library.
type AudioLocation struct {
	ID   int64
	Path string
}

// LocalAudioLocations lists the available local locations whose media item
// carries audio, oldest entries first so coverage grows in a stable order
// across runs.
func LocalAudioLocations(ctx context.Context, db *sqlutil.DB) ([]AudioLocation, error) {
	rows, err := db.QueryContext(ctx, `
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
	locations := []AudioLocation{}
	for rows.Next() {
		var location AudioLocation
		if err := rows.Scan(&location.ID, &location.Path); err != nil {
			return nil, err
		}
		locations = append(locations, location)
	}
	return locations, rows.Err()
}
