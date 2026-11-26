-- Access level changes invalidate an approval snapshot and require a new review.
ALTER TABLE migration_review_groups
  ADD COLUMN IF NOT EXISTS access_level TEXT
  CHECK (access_level IS NULL OR access_level IN ('VIEW_ONLY','VIEW_EDIT'));
