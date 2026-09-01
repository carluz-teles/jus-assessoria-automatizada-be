-- name: InsertSegmentAnchor :one
INSERT INTO segment_anchor (id, draft_segment_id, thesis_anchor_id, status)
VALUES ($1, $2, $3, $4)
RETURNING id, draft_segment_id, thesis_anchor_id, status;
