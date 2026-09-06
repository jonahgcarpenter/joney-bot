-- Only newly written exchanges can opt into public group transcript search.
ALTER TABLE session_turns ADD COLUMN group_gateway TEXT NOT NULL DEFAULT '';
ALTER TABLE session_turns ADD COLUMN group_chat_id TEXT NOT NULL DEFAULT '';
ALTER TABLE session_turns ADD COLUMN public_user_text TEXT NOT NULL DEFAULT ''
    CHECK (
        (group_gateway = '' AND group_chat_id = '' AND public_user_text = '')
        OR (group_gateway IN ('discord', 'imessage') AND length(trim(group_chat_id)) > 0
            AND group_chat_id = trim(group_chat_id))
    );

CREATE TRIGGER session_turns_group_public_update
BEFORE UPDATE OF group_gateway, group_chat_id, public_user_text ON session_turns
WHEN NEW.group_gateway IS NOT OLD.group_gateway
    OR NEW.group_chat_id IS NOT OLD.group_chat_id
    OR NEW.public_user_text IS NOT OLD.public_user_text
BEGIN
    SELECT RAISE(ABORT, 'immutable session turn group provenance and public text');
END;

CREATE TRIGGER session_turns_group_answer_update
BEFORE UPDATE OF assistant_text ON session_turns
WHEN OLD.group_gateway != '' AND NEW.assistant_text IS NOT OLD.assistant_text
BEGIN
    SELECT RAISE(ABORT, 'immutable public group answer');
END;

CREATE INDEX idx_session_turns_group
ON session_turns(group_gateway, group_chat_id, id)
WHERE group_gateway != '';
