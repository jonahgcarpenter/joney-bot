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
	rows, err := s.sql.QueryContext(ctx, `SELECT i.id,i.mime_type,i.data FROM session_images i
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
		if err := rows.Scan(&image.ID, &image.MIMEType, &data); err != nil {
			return nil, err
		}
		image.Data = base64.StdEncoding.EncodeToString(data)
		image.Source = "generated"
		images = append(images, image)
	}
	return images, rows.Err()
}
