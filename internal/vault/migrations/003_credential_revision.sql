-- Every credential write bumps revision, so a process can tell that a refresh was committed since
-- it read the credential, whatever the new token text or expiry turned out to be.
ALTER TABLE credentials ADD COLUMN revision INTEGER NOT NULL DEFAULT 1;
