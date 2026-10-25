-- adopted: the record was already in the zone when stackr took it over. It is
-- repointed when its host's ingress moves, but reconcile and stop managing
-- never delete it.
ALTER TABLE dns_records ADD COLUMN adopted INTEGER NOT NULL DEFAULT 0;
