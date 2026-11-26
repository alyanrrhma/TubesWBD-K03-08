-- Snapshot the required approver set when a revision enters review. Later
-- access grants or leader changes must not silently change an active review.
CREATE TABLE IF NOT EXISTS migration_review_groups (
    revision_id BIGINT NOT NULL REFERENCES migration_revisions(id) ON DELETE RESTRICT,
    group_id BIGINT NOT NULL REFERENCES app_groups(id) ON DELETE RESTRICT,
    leader_user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    required_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (revision_id, group_id)
);

CREATE INDEX IF NOT EXISTS migration_review_groups_approver_idx
    ON migration_review_groups (revision_id, leader_user_id);
