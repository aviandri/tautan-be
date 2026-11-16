CREATE EXTENSION IF NOT EXISTS citext;
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email         citext NOT NULL UNIQUE,
    password_hash text   NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE refresh_tokens (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash text NOT NULL UNIQUE,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX refresh_tokens_user_idx ON refresh_tokens(user_id);

CREATE TABLE profiles (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    username     citext NOT NULL UNIQUE,
    display_name text NOT NULL DEFAULT '',
    bio          text NOT NULL DEFAULT '',
    avatar_url   text NOT NULL DEFAULT '',
    theme        jsonb NOT NULL DEFAULT '{}'::jsonb,
    seo          jsonb NOT NULL DEFAULT '{}'::jsonb,
    is_published boolean NOT NULL DEFAULT false,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX profiles_user_idx ON profiles(user_id);

CREATE TABLE tabs (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    profile_id uuid NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
    title      text NOT NULL,
    slug       text NOT NULL,
    position   int  NOT NULL DEFAULT 0,
    UNIQUE (profile_id, slug)
);

CREATE TABLE blocks (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    profile_id uuid NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
    tab_id     uuid NOT NULL REFERENCES tabs(id) ON DELETE CASCADE,
    parent_id  uuid REFERENCES blocks(id) ON DELETE CASCADE,
    type       text NOT NULL,
    position   int  NOT NULL DEFAULT 0,
    is_visible boolean NOT NULL DEFAULT true,
    data       jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX blocks_tab_idx ON blocks(tab_id, position);

CREATE TABLE events (
    id         bigserial PRIMARY KEY,
    profile_id uuid NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
    block_id   uuid REFERENCES blocks(id) ON DELETE SET NULL,
    kind       text NOT NULL CHECK (kind IN ('view','click')),
    referrer   text NOT NULL DEFAULT '',
    country    text NOT NULL DEFAULT '',
    device     text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX events_profile_created_idx ON events(profile_id, created_at);

CREATE TABLE contact_submissions (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    profile_id uuid NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
    block_id   uuid REFERENCES blocks(id) ON DELETE SET NULL,
    name       text NOT NULL,
    email      text NOT NULL,
    message    text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX contact_submissions_profile_idx ON contact_submissions(profile_id, created_at DESC, id DESC);
