-- API keys may expire, and say when they were last used. NULL expires_at = never.
ALTER TABLE api_keys ADD COLUMN expires_at DATETIME;
ALTER TABLE api_keys ADD COLUMN last_used_at DATETIME;
