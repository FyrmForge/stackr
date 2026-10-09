-- Migration I: an API key may carry a ceiling and one stack. level '' = no ceiling
-- (else viewer|member|owner|admin); stack_id NULL = every stack of the org.
ALTER TABLE api_keys ADD COLUMN level TEXT NOT NULL DEFAULT '';
ALTER TABLE api_keys ADD COLUMN stack_id TEXT REFERENCES stacks (id) ON DELETE CASCADE;
