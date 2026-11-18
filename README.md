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

Health checks: `/livez` (process up, no DB), `/readyz` (DB reachable and not shutting down),
and the older `/healthz` and `/api/v1/health` (DB ping).

`api migrate` applies migrations and exits; `api healthcheck` probes `/livez` (for Docker
`HEALTHCHECK` in the distroless image).

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
| `APP_ENV` | `development` | `production` refuses to start with unsafe settings (dev JWT secret, http URLs, `COOKIE_SECURE=false`) |
| `LOG_FORMAT` / `LOG_LEVEL` | `text` (`json` in production) / `info` | structured logs via `log/slog` |
| `MIGRATE_ON_START` | `true` | set `false` and run `api migrate` as a release step instead |
| `SHUTDOWN_DELAY` / `SHUTDOWN_TIMEOUT` | `0s` / `15s` | on SIGTERM `/readyz` fails for the delay, then requests drain |
| `TRUST_PROXY` | `false` | use `X-Forwarded-For` for client IP (rate limits); enable only behind your own proxy |
| `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET` | (empty) | Google sign-in; the feature is off unless these and `GOOGLE_REDIRECT_URL` are set. The secret is a secret. |
| `GOOGLE_REDIRECT_URL` | (empty) | the FE callback page, e.g. `http://localhost:3000/auth/google/callback` |
| `GOOGLE_ISSUER` | `https://accounts.google.com` | OIDC issuer for discovery (only tests change it) |

## Google sign-in setup

The API runs the OAuth 2.0 / OpenID Connect authorization code flow with PKCE, state and
nonce. Google redirects the browser to the **FE** page `/auth/google/callback`, which POSTs
the code to the API. That means the redirect URI registered at Google is the FE URL, not the API URL.

1. Open [Google Cloud console](https://console.cloud.google.com/) and create (or pick) a project.
2. **APIs & Services → OAuth consent screen** (Google Auth Platform → Branding/Audience):
   - User type: **External**. Fill in the app name (Tautan), support email, app domain and
     authorised domain (e.g. `tautan.id`).
   - Scopes: `openid`, `.../auth/userinfo.email`, `.../auth/userinfo.profile`. These are
     non-sensitive, so Google won't review the app.
   - While the app is in **Testing**, only the listed test users can sign in. Click
     **Publish app** before launch.
3. **APIs & Services → Credentials → Create credentials → OAuth client ID**:
   - Application type: **Web application**.
   - Authorised JavaScript origins: leave empty (the browser never calls Google's JS).
   - Authorised redirect URIs, one per environment, matching `GOOGLE_REDIRECT_URL` exactly
     (scheme, host, port, path, no trailing slash):
     - `http://localhost:3000/auth/google/callback` (local; the FE's dev port)
     - `https://tautan.id/auth/google/callback` (production)
4. Copy the client ID and secret into `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET`, set
   `GOOGLE_REDIRECT_URL`, and restart. `GET /api/v1/auth/providers` should now return
   `{"google": true}` and the FE shows "Continue with Google".

Notes:
- The flow keeps state, nonce and the PKCE verifier in a 10-minute httpOnly cookie
  (`tautan_oauth`, path `/api/v1/auth/google`). It follows the refresh cookie's
  SameSite/Secure rules, so the FE and API must be same-site, or the API must run with
  `COOKIE_SECURE=true` (HTTPS).
- A Google account whose **verified** email matches an existing account is linked to it.
  Otherwise a new account is created and goes through onboarding with a temporary
  username, like a signup without a username. Unverified emails are rejected (403 `email_not_verified`).
- Tests use a fake OIDC server (`internal/server/google_test.go`), so CI needs no real credentials.

## Layout

```
cmd/api        production entrypoint (needs DATABASE_URL)
cmd/dev        embedded Postgres + API, for local development
internal/db    pgx pool + embedded golang-migrate migrations (run on startup)
internal/server  chi router, handlers, middleware
```

## Deploy

`docker compose up --build` runs Postgres 17 plus the API.
