# Environment variables (tautan-be + tautan-fe)

The production values for the chosen setup (single VPS) are wired in [`deploy/docker-compose.prod.yml`](../deploy/docker-compose.prod.yml). You only edit `deploy/.env`; see [DEPLOY.md](DEPLOY.md#where-the-domain-is-set) for where the domain goes.

Every variable the two apps read, audited on 2026-11-18. Columns:

- **Req (prod)**: ✅ must be set in production, ⚙️ set it for your topology, blank means optional.
- **Secret**: 🔒 keep it out of git and logs; store it in the platform's secret store or the VPS `.env` (mode 600).
- **Default**: the value used when the variable is unset or empty.
- **Since**: the tautan-be PR that added the variable: #29 (ops), #30 (Google sign-in), #31 (proxy/cookies), #32 (storage). Everything unmarked has been on `main` longer.

## API (tautan-be)

### Core

| Var | Req (prod) | Secret | Default | Notes |
|---|---|---|---|---|
| `DATABASE_URL` | ✅ | 🔒 | – (`make dev` uses embedded PG) | `postgres://user:pass@host:5432/db?sslmode=require`. Neon/Supabase: use the **direct** (non-pooled) URL if you run migrations from the app; pgx keeps its own pool. |
| `JWT_SECRET` | ✅ | 🔒 | `dev-secret-change-me` | HMAC key for access tokens. `openssl rand -base64 48`. With `APP_ENV=production` the API refuses the default or anything under 32 chars. Rotating it logs everyone out of their access token (refresh tokens survive). |
| `ADDR` | | | `:8080` | Listen address. |
| `APP_ENV` | ✅ | | `development` (the Docker image sets `production`) | `production` turns on `Config.Validate()` (fail fast on unsafe values) and JSON logs. *Since #29.* |
| `PUBLIC_BASE_URL` | ✅ | | `http://localhost:8080` | Public API origin, e.g. `https://api.tautan.id`. **Baked into stored upload URLs** (`<base>/uploads/<name>`), so choose it once; changing it later breaks existing image URLs unless the old host keeps serving. Must be https in production. |
| `APP_BASE_URL` | ✅ | | `http://localhost:3000` | Public FE origin, e.g. `https://tautan.id`. Used for vCard page URLs and the default revalidate URL. https in production. |
| `CORS_ORIGINS` | ✅ | | `http://localhost:3000` | Comma-separated browser origins allowed with credentials: `https://tautan.id,https://www.tautan.id`. Exact origins, no path or trailing slash. |
| `ACCESS_TTL` | | | `15m` | Go duration. |
| `REFRESH_TTL` | | | `720h` | Go duration (30 days). |

### Cookies and proxies

| Var | Req (prod) | Secret | Default | Notes |
|---|---|---|---|---|
| `COOKIE_SECURE` | ✅ | | `false` | `true` behind HTTPS (required by `APP_ENV=production`). |
| `COOKIE_SAMESITE` | | | `lax` | `lax`, `strict` or `none` (anything else is rejected in production). It also applies to the Google OAuth state cookie `tautan_oauth`. Lax is right when the FE and API share a registrable domain (`tautan.id` + `api.tautan.id`). `none` (forces Secure) only for different domains, and Safari/ITP may still block it. *Since #31.* (Before #31, `COOKIE_SECURE=true` implied `None`.) |
| `COOKIE_DOMAIN` | | | host-only | e.g. `.tautan.id`. Leave empty: the refresh cookie only needs to reach the API host. |
| `TRUSTED_PROXIES` | ⚙️ | | none | Comma-separated IPs/CIDRs plus `private`, `cloudflare`, `*`. Forwarding headers (client IP, country) are only believed from these peers. VPS + Caddy + Cloudflare: `private,cloudflare`. Fly behind Cloudflare: `private,cloudflare`. *Since #31.* |
| `CLIENT_IP_HEADER` | | | `X-Forwarded-For` | XFF is walked right to left, skipping trusted hops. Or a single-IP header from a trusted peer: `CF-Connecting-IP`, `Fly-Client-IP`, `X-Real-IP`. *Since #31.* |
| `COUNTRY_HEADER` | | | `CF-IPCountry` | Visitor country for analytics: `CF-IPCountry` (Cloudflare) or `X-Vercel-IP-Country`. Only read from a trusted proxy (*since #31*); without one, country is blank. |
| `TRUST_PROXY` | | | `false` | Legacy: `true` = `TRUSTED_PROXIES=*` (spoofable). Prefer `TRUSTED_PROXIES`. |

### Uploads

| Var | Req (prod) | Secret | Default | Notes |
|---|---|---|---|---|
| `STORAGE` | | | `local` | `local` (disk at `UPLOAD_DIR`) or `s3` (R2/S3/MinIO). The API serves `/uploads/*` either way. *Since #32.* |
| `UPLOAD_DIR` | | | `./uploads` (image: `/data/uploads`) | Local storage directory; mount a volume. |
| `S3_BUCKET` | ⚙️ (s3) | | – | |
| `S3_ENDPOINT` | ⚙️ (s3) | | AWS default | R2: `https://<account-id>.r2.cloudflarestorage.com`. |
| `S3_REGION` | | | `auto` | `auto` for R2, else the AWS region. |
| `S3_ACCESS_KEY_ID` | ⚙️ (s3) | 🔒 | – | R2: an API token with *Object Read & Write* on the bucket only. |
| `S3_SECRET_ACCESS_KEY` | ⚙️ (s3) | 🔒 | – | |
| `S3_PREFIX` | | | `uploads/` | Key prefix inside the bucket. |
| `S3_PATH_STYLE` | | | `false` | `true` for MinIO. |

### Revalidate webhook (BE → FE)

| Var | Req (prod) | Secret | Default | Notes |
|---|---|---|---|---|
| `REVALIDATE_SECRET` | ✅ | 🔒 | empty (off) | **Same value as the FE's `REVALIDATE_SECRET`.** While empty, published edits show up only after the 60 s ISR refresh. |
| `REVALIDATE_URL` | | | `$APP_BASE_URL/api/revalidate` | Override, e.g. `http://fe:3000/api/revalidate` on a private network. Alias: `FRONTEND_REVALIDATE_URL`. |

### Operations

| Var | Req (prod) | Secret | Default | Notes |
|---|---|---|---|---|
| `LOG_FORMAT` | | | `json` in production, else `text` | *Since #29.* |
| `LOG_LEVEL` | | | `info` | `debug` also logs probe requests. |
| `MIGRATE_ON_START` | | | `true` | Set `false` and run `api migrate` as a release step (Fly `release_command`, k8s Job). Concurrent runs are safe (advisory lock). |
| `SHUTDOWN_DELAY` | | | `0s` | After SIGTERM, `/readyz` fails for this long before the listener closes. Use `5s` behind a load balancer. |
| `SHUTDOWN_TIMEOUT` | | | `15s` | Max drain time for in-flight requests. |

### Integrations

| Var | Req (prod) | Secret | Default | Notes |
|---|---|---|---|---|
| `INSTAGRAM_OEMBED_TOKEN` | | 🔒 | – | Meta app token for Instagram oEmbed posters. Without it, Instagram embeds fall back to a plain facade. |
| `GOOGLE_CLIENT_ID` | | | – | Google sign-in is on only when the ID, secret and redirect URL are all set. *Since #30.* |
| `GOOGLE_CLIENT_SECRET` | | 🔒 | – | |
| `GOOGLE_REDIRECT_URL` | | | – | The FE callback page, e.g. `https://tautan.id/auth/google/callback`. Register it in Google Cloud Console. |
| `GOOGLE_ISSUER` | | | `https://accounts.google.com` | Only for tests (fake OIDC server). |

### Dev and test only

| Var | Default | Notes |
|---|---|---|
| `DEV_PG_PORT` | `54329` | Embedded Postgres port for `make dev` and `make seed-heavy`. |
| `TEST_DATABASE_URL` | – | Run tests against an existing Postgres instead of booting an embedded one. |

## Frontend (tautan-fe)

`NEXT_PUBLIC_*` values are **inlined at build time**. On Vercel, set them in project env (a redeploy rebuilds). For the Docker image, pass them as `--build-arg`s (CI reads repo variables), and rebuild to change them.

| Var | Req (prod) | Secret | When | Default | Notes |
|---|---|---|---|---|---|
| `NEXT_PUBLIC_API_URL` | ✅ | | build | `http://localhost:8080` | Public API origin the browser calls (no `/api/v1`). Also used by server-side reads unless `API_INTERNAL_URL` is set. |
| `NEXT_PUBLIC_SITE_URL` | ✅ | | build | `http://localhost:3000` (render), current origin (client) | Canonical/share/QR URLs. |
| `NEXT_PUBLIC_SITE_HOST` | ✅ | | build | `tautan.id` | Host shown in the UI ("tautan.id/alice"). |
| `NEXT_PUBLIC_API_MOCK` | | | build | unset | `1` = in-memory mock API. **Must be unset or `0` in production** (the Dockerfile forces `0`). |
| `REVALIDATE_SECRET` | ✅ | 🔒 | runtime | unset (route returns 503) | Same as the API's. |
| `API_INTERNAL_URL` | | | runtime | unset | Server-only base for page reads over a private network, e.g. `http://api:8080`. *Since tautan-fe #63.* |
| `NEXT_OUTPUT` | | | build | unset | `standalone` in the Dockerfile only. *Since tautan-fe #63.* |
| `PORT` / `HOSTNAME` | | | runtime | `3000` / `0.0.0.0` (image) | Standalone server listen address. |
| `NEXT_DIST_DIR` | | | build/dev | `.next` | Dev convenience for parallel builds. |

FE dev and test scripts only: `BASE`, `API`, `FE`, `HEADED`, `SLOWMO`, `PAUSE` (e2e), `CHROME_PATH`, `PLAYWRIGHT_CORE` (locked-stub e2e), `PORT` (perf/mock-api, default 8097) and `PUBLIC_JS_BUDGET_KB` (budget check, default 110).

## Shared values: set them together

| What | API | FE |
|---|---|---|
| Webhook secret | `REVALIDATE_SECRET` | `REVALIDATE_SECRET` (identical) |
| API origin | `PUBLIC_BASE_URL` | `NEXT_PUBLIC_API_URL` (identical) |
| FE origin | `APP_BASE_URL`, and listed in `CORS_ORIGINS` | `NEXT_PUBLIC_SITE_URL` |
