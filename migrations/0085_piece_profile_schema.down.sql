-- Seed rows cascade away with their parent tables below (profile_rule/profile_section ON DELETE
-- CASCADE from piece_profile; compliance_rule has no dependents left once profile_rule is gone).
DROP TABLE IF EXISTS thesis_coverage;
DROP TABLE IF EXISTS segment_anchor;
DROP TABLE IF EXISTS draft_segment;
DROP TABLE IF EXISTS thesis_anchor;
DROP TABLE IF EXISTS thesis;
DROP TABLE IF EXISTS piece_profile_version;
DROP TABLE IF EXISTS section_rule;
DROP TABLE IF EXISTS profile_rule;
DROP TABLE IF EXISTS profile_requirement;
DROP TABLE IF EXISTS profile_section;
DROP TABLE IF EXISTS piece_profile;
DROP TABLE IF EXISTS compliance_rule;
DROP TABLE IF EXISTS format_profile;
DROP TABLE IF EXISTS matter;
DROP TABLE IF EXISTS base_skeleton;
