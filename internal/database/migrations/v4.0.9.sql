-- v4.0.7 already reconstructed provider submissions from historical redrives.
ALTER TABLE durable_jobs DROP COLUMN redrive_count;
ALTER TABLE sessions DROP COLUMN fact_count;
ALTER TABLE sessions DROP COLUMN profile_bytes;
