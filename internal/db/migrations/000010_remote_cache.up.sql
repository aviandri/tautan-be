-- P3-B: server-side cache for oEmbed posts, YouTube channel feeds, podcast
-- feeds and re-hosted remote images.
CREATE TABLE IF NOT EXISTS remote_cache (
    key        text PRIMARY KEY,           -- post:<url> | yt:<channel> | rss:<feed> | img:<src>
    data       jsonb NOT NULL DEFAULT 'null',
    ok         boolean NOT NULL DEFAULT false,
    error      text NOT NULL DEFAULT '',
    fetched_at timestamptz NOT NULL DEFAULT now(),
    next_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS remote_cache_next_idx ON remote_cache (next_at);
