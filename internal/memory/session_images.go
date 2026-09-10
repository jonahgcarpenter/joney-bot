package memory

import (
	"context"
	"encoding/base64"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

// SessionImages returns at most eight normalized delivered outputs, newest first.
// Ownership is derived from the source turn, including after account merges.
func (s *Store) SessionImages(ctx context.Context, userID, sessionID string, generation int) ([]requestctx.InputImage, error) {
	rows, err := s.sql.QueryContext(ctx, `SELECT i.id,i.mime_type,i.data,i.image_id,i.version,i.parent_source_image_id,i.version_highwater FROM session_images i
 JOIN session_turns t ON t.id=i.turn_id JOIN sessions s ON s.canonical_user_id=t.canonical_user_id AND s.session_id=t.session_id AND s.generation=t.session_generation
 WHERE t.canonical_user_id=? AND t.session_id=? AND t.session_generation=?
 AND t.delivered_at IS NOT NULL AND t.delivery_failed_at IS NULL AND s.is_active=1
 AND julianday(s.expires_at)>julianday(?) AND julianday(t.expires_at)>julianday(?)
 ORDER BY t.id DESC,i.ordinal DESC LIMIT 8`, userID, sessionID, generation, formatTime(time.Now().UTC()), formatTime(time.Now().UTC()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var images []requestctx.InputImage
	for rows.Next() {
		var image requestctx.InputImage
		var data []byte
		if err := rows.Scan(&image.ID, &image.MIMEType, &data, &image.ImageID, &image.Version, &image.ParentSourceImageID, &image.VersionHighwater); err != nil {
			return nil, err
		}
		image.Data = base64.StdEncoding.EncodeToString(data)
		image.Source = "generated"
		images = append(images, image)
	}
	return images, rows.Err()
}

// ReserveImageVersion advances retained logical-image counters under exact active
// session ownership. Counters live as long as an editable asset, including failures.
func (s *Store) ReserveImageVersion(ctx context.Context, userID, sessionID string, generation int, imageID string, minimum int) (int, error) {
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE canonical_user_id=? AND session_id=? AND generation=? AND is_active=1 AND julianday(expires_at)>julianday(?)`, userID, sessionID, generation, formatTime(time.Now().UTC())).Scan(&active); err != nil {
		return 0, err
	}
	var high int
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(i.version_highwater),0) FROM session_images i
 JOIN session_turns t ON t.id=i.turn_id JOIN sessions s ON s.canonical_user_id=t.canonical_user_id AND s.session_id=t.session_id AND s.generation=t.session_generation
 WHERE i.image_id=? AND t.canonical_user_id=? AND t.session_id=? AND t.session_generation=? AND s.is_active=1
 AND julianday(s.expires_at)>julianday(?)`, imageID, userID, sessionID, generation, formatTime(time.Now().UTC())).Scan(&high)
	if err != nil {
		return 0, err
	}
	if high < minimum {
		high = minimum
	}
	high++
	_, err = tx.ExecContext(ctx, `UPDATE session_images SET version_highwater=? WHERE image_id=? AND turn_id IN
 (SELECT id FROM session_turns WHERE canonical_user_id=? AND session_id=? AND session_generation=?)`, high, imageID, userID, sessionID, generation)
	if err != nil {
		return 0, err
	}
	return high, tx.Commit()
}
