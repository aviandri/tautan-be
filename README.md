# tautan-be

Go REST API for **Tautan**, a link-in-bio product. The API contract lives in the
workspace `SPEC.md` (base path `/api/v1`).

## Quick start (no Docker, no Postgres needed)

```sh
make dev     # API on http://localhost:8080, embedded Postgres on :54329 (`DEV_PG_PORT`), data in ./.devdata
make test    # tests start their own throwaway embedded Postgres
```

Go is expected at `~/.local/go` (the Makefile adds it to `PATH`). The first run downloads
Postgres binaries (~10 MB) into `.devdata/cache`.

Health check: `curl localhost:8080/api/v1/health` → `{"status":"ok"}`

## Configuration (env)

| Var | Default | |
|---|---|---|
| `ADDR` | `:8080` | listen address |
| `DATABASE_URL` | (embedded in `make dev`) | required for `make run` / Docker |
| `JWT_SECRET` | `dev-secret-change-me` | HMAC secret for access tokens |
| `ACCESS_TTL` / `REFRESH_TTL` | `15m` / `720h` | token lifetimes |
| `CORS_ORIGINS` | `http://localhost:3000` | comma separated, credentials allowed |
| `UPLOAD_DIR` | `./uploads` | served at `/uploads/*` |
| `PUBLIC_BASE_URL` | `http://localhost:8080` | used to build upload URLs |
| `COOKIE_SECURE` | `false` | set `true` behind HTTPS |
| `COUNTRY_HEADER` | `CF-IPCountry` | header holding the visitor's country code |
| `APP_BASE_URL` | `http://localhost:3000` | public FE origin (page URL in vCards) |
| `TRUST_PROXY` | `false` | use `X-Forwarded-For` for client IP (rate limits); enable only behind your own proxy |

## Layout

```
cmd/api        production entrypoint (needs DATABASE_URL)
cmd/dev        embedded Postgres + API, for local development
internal/db    pgx pool + embedded golang-migrate migrations (run on startup)
internal/server  chi router, handlers, middleware
```

## Deploy

`docker compose up --build` runs Postgres 17 plus the API.
