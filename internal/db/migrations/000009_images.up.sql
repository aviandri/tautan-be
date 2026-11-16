-- Perf 2b: dimensions and variant state for uploaded images.
CREATE TABLE IF NOT EXISTS images (
    name       text PRIMARY KEY,            -- file name under UPLOAD_DIR
    width      int  NOT NULL,
    height     int  NOT NULL,
    planned    int[] NOT NULL DEFAULT '{}', -- variant widths to encode
    ready      boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now()
);
