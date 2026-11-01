CREATE TABLE sessions (
    id TEXT PRIMARY KEY NOT NULL,
    data TEXT NOT NULL,
    expires_at INTEGER NOT NULL
);

CREATE INDEX sessions_expiry_idx ON sessions(expires_at);
