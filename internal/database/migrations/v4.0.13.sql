-- Each legacy asset starts an independent logical image at version one.
ALTER TABLE session_images ADD COLUMN image_id TEXT NOT NULL DEFAULT '';
ALTER TABLE session_images ADD COLUMN version INTEGER NOT NULL DEFAULT 1 CHECK(version > 0);
ALTER TABLE session_images ADD COLUMN parent_source_image_id TEXT NOT NULL DEFAULT '';
ALTER TABLE session_images ADD COLUMN version_highwater INTEGER NOT NULL DEFAULT 1 CHECK(version_highwater >= version);
UPDATE session_images SET image_id=id;
CREATE INDEX session_images_logical ON session_images(image_id);
