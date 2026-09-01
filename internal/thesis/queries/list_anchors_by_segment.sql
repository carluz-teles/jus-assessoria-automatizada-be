-- name: ListAnchorsBySegment :many
SELECT id, draft_segment_id, thesis_anchor_id, status
FROM segment_anchor
WHERE draft_segment_id = $1
ORDER BY id ASC;
