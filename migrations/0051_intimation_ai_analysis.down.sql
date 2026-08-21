-- Down: drop the intimation AI-analysis columns added by 0051.
ALTER TABLE intimation
    DROP COLUMN ai_summary,
    DROP COLUMN ai_providencias,
    DROP COLUMN ai_analyzed_at;
