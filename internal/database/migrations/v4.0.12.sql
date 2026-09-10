-- Normalized generated assets inherit ownership and lifecycle from their turn.
CREATE TABLE session_images (
 id TEXT PRIMARY KEY NOT NULL CHECK(length(id) BETWEEN 1 AND 64),
 turn_id INTEGER NOT NULL REFERENCES session_turns(id) ON DELETE CASCADE,
 ordinal INTEGER NOT NULL CHECK(ordinal BETWEEN 0 AND 3),
 mime_type TEXT NOT NULL CHECK(mime_type IN ('image/png','image/jpeg')),
 data BLOB NOT NULL CHECK(typeof(data)='blob' AND length(data) BETWEEN 1 AND 286720),
 UNIQUE(turn_id, ordinal)
);
CREATE TRIGGER session_images_bound AFTER INSERT ON session_images
BEGIN
 DELETE FROM session_images WHERE id IN (
  SELECT i.id FROM session_images i JOIN session_turns t ON t.id=i.turn_id
  JOIN session_turns source ON source.id=NEW.turn_id
  WHERE t.canonical_user_id=source.canonical_user_id AND t.session_id=source.session_id
  ORDER BY t.id DESC, i.ordinal DESC LIMIT -1 OFFSET 8
 );
END;
CREATE TRIGGER session_images_merge_bound AFTER UPDATE OF canonical_user_id,session_id ON session_turns
WHEN NEW.canonical_user_id IS NOT OLD.canonical_user_id OR NEW.session_id IS NOT OLD.session_id
BEGIN
 DELETE FROM session_images WHERE id IN (
  SELECT i.id FROM session_images i JOIN session_turns t ON t.id=i.turn_id
  WHERE t.canonical_user_id=NEW.canonical_user_id AND t.session_id=NEW.session_id
  ORDER BY t.id DESC,i.ordinal DESC LIMIT -1 OFFSET 8
 );
END;
