-- Payments follow-up: a download counts once per signed link (exp + file
-- index), however many requests (Range, resumes) that link serves.
CREATE TABLE order_download_links (
    order_id   uuid        NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    link_key   text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (order_id, link_key)
);
