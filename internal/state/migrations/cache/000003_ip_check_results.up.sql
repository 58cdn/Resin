CREATE TABLE IF NOT EXISTS ip_check_results (
    subject_kind TEXT NOT NULL CHECK (subject_kind IN ('egress_ip', 'node_hash')),
    subject TEXT NOT NULL,
    source TEXT NOT NULL,
    result_json TEXT NOT NULL,
    checked_at_ns INTEGER NOT NULL,
    expires_at_ns INTEGER NOT NULL,
    PRIMARY KEY (subject_kind, subject, source)
);

CREATE INDEX IF NOT EXISTS idx_ip_check_results_expiry
    ON ip_check_results (expires_at_ns);

-- The polymorphic subject cannot have a foreign key. Keep node-owned results
-- tied to physical deletion, including bootstrap repair and bulk deletes.
CREATE TRIGGER IF NOT EXISTS delete_node_ip_check_results
AFTER DELETE ON nodes_static
BEGIN
    DELETE FROM ip_check_results
    WHERE subject_kind = 'node_hash' AND subject = OLD.hash;
END;
