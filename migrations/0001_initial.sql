CREATE TABLE scans (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    seed_url TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('queued', 'running', 'completed', 'cancelled', 'failed', 'interrupted')),
    max_pages INTEGER NOT NULL,
    config_snapshot TEXT NOT NULL DEFAULT '{}',
    started_at TEXT,
    finished_at TEXT,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    error TEXT,
    pages_discovered INTEGER NOT NULL DEFAULT 0,
    pages_checked INTEGER NOT NULL DEFAULT 0,
    links_checked INTEGER NOT NULL DEFAULT 0,
    truncated INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX scans_created_at_idx ON scans(created_at DESC);
CREATE INDEX scans_state_idx ON scans(state);
CREATE UNIQUE INDEX scans_single_active_idx ON scans((1)) WHERE state IN ('queued', 'running');

CREATE TABLE pages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    scan_id INTEGER NOT NULL REFERENCES scans(id) ON DELETE CASCADE,
    url TEXT NOT NULL,
    final_url TEXT,
    status_code INTEGER,
    fetch_state TEXT NOT NULL,
    redirect_chain TEXT NOT NULL DEFAULT '[]',
    title TEXT,
    description TEXT,
    canonical TEXT,
    og_title TEXT,
    og_description TEXT,
    og_url TEXT,
    og_image TEXT,
    robots TEXT,
    checked_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE(scan_id, url)
);

CREATE INDEX pages_scan_id_idx ON pages(scan_id);

CREATE TABLE findings (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    page_id INTEGER NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
    rule_id TEXT NOT NULL,
    category TEXT NOT NULL,
    severity TEXT NOT NULL,
    status TEXT NOT NULL,
    evidence TEXT NOT NULL DEFAULT '',
    explanation TEXT NOT NULL,
    suggestion TEXT NOT NULL
);

CREATE INDEX findings_page_id_idx ON findings(page_id);

CREATE TABLE destinations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    scan_id INTEGER NOT NULL REFERENCES scans(id) ON DELETE CASCADE,
    url TEXT NOT NULL,
    classification TEXT NOT NULL,
    health TEXT NOT NULL,
    status_code INTEGER,
    redirect_chain TEXT NOT NULL DEFAULT '[]',
    checked_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE(scan_id, url)
);

CREATE TABLE link_occurrences (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    page_id INTEGER NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
    destination_id INTEGER REFERENCES destinations(id) ON DELETE SET NULL,
    raw_href TEXT NOT NULL,
    classification TEXT NOT NULL,
    fragment TEXT
);

CREATE INDEX link_occurrences_page_id_idx ON link_occurrences(page_id);
