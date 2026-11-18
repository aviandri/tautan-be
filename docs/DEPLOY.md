# Deploying Tautan

Tautan's users and their visitors are mostly in Indonesia, and nearly all visitors arrive on a phone from a bio link. The performance budget (SPEC.md) sets public-page **TTFB ≤ 200 ms cached / ≤ 600 ms uncached** and **LCP ≤ 1.8 s on Slow 4G**. Every option below therefore keeps compute and data in **Singapore or Jakarta**, with Cloudflare's Jakarta PoP (CGK) in front.

Every variable is documented in [ENV.md](ENV.md). This guide assumes these PRs are merged: tautan-be #29 (ops), #31 (proxy/cookies), #32 (storage) and #33 (image), and tautan-fe #63 (Docker/Vercel).

## Latency, in brief

| Path | Typical RTT |
|---|---|
| Jakarta phone → Cloudflare/Vercel edge in Jakarta or Singapore | 5–20 ms (plus 4G radio, 50–150 ms) |
| Jakarta ↔ Singapore data centre | 12–20 ms |
| Jakarta ↔ Tokyo | ~70 ms |
| Jakarta ↔ US East (Vercel's default `iad1`) | 230+ ms |

The rule that follows: **FE functions, API and Postgres must sit in the same region.** An uncached `/[username]` render makes FE → API → DB round trips. Across an ocean that alone uses up the 600 ms budget. Each tautan-fe `vercel.json` therefore pins `sin1`.

## Options

Prices are list prices checked on 2026-11-18, in USD per month for launch-scale traffic (≈ 50k page views a month, < 10 GB of uploads). Verify them before committing; PaaS pricing changes often. Fly.io raised RAM to $6/GB-month in Oct 2026, and Neon now bills per CU-hour.

### A. Managed: Vercel (FE) + Fly.io (API) + Neon (Postgres) + Cloudflare R2

| Piece | Choice | Cost |
|---|---|---|
| FE | Vercel **Pro** (Hobby forbids commercial use), functions in `sin1` | $20 per member, plus usage beyond the included quota |
| API | Fly.io, region `sin`, 2 × shared-cpu-1x / 1 GB (AVIF encoding wants RAM) | ≈ $10–14 |
| DB | Neon Launch, `aws-ap-southeast-1` (Singapore), 0.25–0.5 CU **always on** | ≈ $20–40 (0.25 CU × 730 h × $0.106 ≈ $19, plus storage) |
| Uploads | Cloudflare R2 (`STORAGE=s3`), free egress | $0 under 10 GB, then $0.015/GB |
| DNS/CDN | Cloudflare Free, proxying `api.` for CF-IPCountry and edge caching of `/uploads` | $0 |
| **Total** | | **≈ $50–75** |

- **Pros:** no servers to patch. Neon runs backups and point-in-time recovery. Vercel edge-caches ISR pages, and `revalidateTag` purges them globally. Fly runs zero-downtime rolling deploys with `release_command`. Everything scales horizontally.
- **Cons:** four vendors and four bills. Neon's scale-to-zero adds a ~0.5 s cold start to the first query, so keep production always-on (that's what costs money). The API runs N instances, and its in-memory state is per instance: rate limits allow N× the configured rate, and each instance has its own image-encoding queue (harmless). Vercel's bandwidth and function overage can surprise you on a viral page.

### B. One VPS: docker-compose behind Caddy, Cloudflare in front (recommended)

| Piece | Choice | Cost |
|---|---|---|
| VPS | 2 vCPU / 4 GB / 80 GB SSD in **Singapore** (DigitalOcean SGP1, Vultr, Linode, AWS Lightsail), or **Jakarta** (Biznet Gio, IDCloudHost, Tencent Cloud Jakarta) | ≈ $24 (DO SGP1); Jakarta locals ≈ IDR 150–350k (≈ $10–22) |
| Everything else | Caddy, FE, API, Postgres 17 and the nightly backup, all in [`deploy/docker-compose.prod.yml`](../deploy/docker-compose.prod.yml) | $0 |
| Uploads + offsite backups | Cloudflare R2 | $0 under 10 GB |
| DNS/CDN/TLS edge | Cloudflare Free (proxied, Origin CA cert) | $0 |
| Optional | Provider snapshots/backups (DO: +20%) | ≈ $5 |
| **Total** | | **≈ $15–30** |

- **Pros:** cheapest by 2–3×. FE → API → DB hops are on localhost; with `API_INTERNAL_URL` the uncached render never leaves the box, which gives the best uncached TTFB of all three options. Cloudflare's Jakarta PoP terminates TLS close to visitors. One bill, one place to look, and no vendor-specific config. It moves to option A piece by piece later, because nothing here is VPS-specific: S3 storage, a stateless API and the standalone FE image all carry over.
- **Cons:** a single point of failure. A VPS reboot means ~1 min of downtime, and a deploy means a few seconds while containers restart. You own OS patching, Postgres tuning and backup restores (practise once). Vertical scaling only until you split it out. ISR pages are cached on the box, not at Cloudflare's edge, but the FE serves them from disk in a few ms.

### C. Vercel (FE) + Railway (API + Postgres), both in Singapore

| Piece | Choice | Cost |
|---|---|---|
| FE | Vercel Pro, `sin1` | $20 |
| API + DB | Railway (Pro for production: $20 including $20 usage), region Singapore, API service plus a Postgres service with a volume | ≈ $20–35 |
| Uploads | R2 | $0 |
| **Total** | | **≈ $40–55** |

- **Pros:** the simplest managed setup. Railway deploys straight from the GHCR image or the repo, and API and DB share a private network. One fewer vendor than A.
- **Cons:** Railway Postgres is a container with a volume, not a managed HA database, so backups are your job (use `deploy/backup.sh` as a cron service). Usage-based billing is harder to predict. Fewer regions.

### Recommendation

**Start on B (one Singapore VPS, ideally DigitalOcean SGP1 2 vCPU/4 GB, behind Cloudflare, with R2 for uploads and offsite backups).** At launch scale it's the cheapest, it's the fastest for uncached pages, and it has the fewest moving parts. Move to **A** when one of these becomes true: you need more than one API instance, a single box can't take the AVIF encoding load, or downtime during a reboot becomes unacceptable. Take a Jakarta provider instead of Singapore only if it offers snapshots and an API you trust. It saves ~10 ms per round trip, but Cloudflare already terminates TLS in Jakarta.

## Step by step: option B

### 0. Prerequisites

- A domain (e.g. `tautan.id`) on a Cloudflare account (Free plan).
- A GitHub account with access to both repos, and an SSH key.
- About an hour.

### 1. Images

1. Merge the PRs listed above. Each repo's `image` workflow then pushes `ghcr.io/aviandri/tautan-be` and `ghcr.io/aviandri/tautan-fe` with `latest` and `sha-xxxxxxx` tags.
2. **Before the FE builds on `main`**, set repository variables in tautan-fe (*Settings → Secrets and variables → Actions → Variables*): `NEXT_PUBLIC_API_URL=https://api.tautan.id`, `NEXT_PUBLIC_SITE_URL=https://tautan.id` and `NEXT_PUBLIC_SITE_HOST=tautan.id`. They're baked into the bundle; re-run the workflow if you change them.
3. GHCR packages start private. Either make both packages public (*Package settings → Change visibility*), or create a classic PAT with only `read:packages` and run `docker login ghcr.io -u <user>` on the VPS.

### 2. VPS

1. Create an Ubuntu 24.04 LTS droplet: 2 vCPU / 4 GB, region SGP1, your SSH key, backups optional.
2. Harden it and install Docker:
   ```sh
   adduser deploy && usermod -aG sudo deploy && rsync -a ~/.ssh /home/deploy/ && chown -R deploy: /home/deploy/.ssh
   sed -i 's/^#\?PasswordAuthentication.*/PasswordAuthentication no/; s/^#\?PermitRootLogin.*/PermitRootLogin no/' /etc/ssh/sshd_config && systemctl reload ssh
   apt update && apt -y upgrade && apt -y install unattended-upgrades ufw
   ufw allow OpenSSH && ufw allow 80,443/tcp && ufw allow 443/udp && ufw enable
   curl -fsSL https://get.docker.com | sh && usermod -aG docker deploy
   ```
   Optional, and stricter: allow 80/443 only from [Cloudflare's ranges](https://www.cloudflare.com/ips/), so nobody can bypass the proxy. Docker publishes ports past ufw, so do it in the provider's cloud firewall (DO: *Networking → Firewalls*).

### 3. Cloudflare

1. **DNS:** add `A @`, `A www` and `A api` records pointing at the VPS IP, all **Proxied** (orange cloud).
2. **SSL/TLS → Overview:** **Full (strict)**. **Edge Certificates:** turn on *Always Use HTTPS*, set minimum TLS 1.2, and enable HTTP/3.
3. **SSL/TLS → Origin Server → Create certificate:** RSA, hostnames `tautan.id, *.tautan.id`, 15 years. Save the certificate as `deploy/certs/origin.pem` and the key as `deploy/certs/origin-key.pem` on the VPS (step 4).
4. **Caching:** keep the defaults. Cloudflare caches `/uploads/*.png|jpg|webp|avif` by extension, with the query string (`?w=`) in the key, and API responses are already `immutable`. **Never** add a "Cache Everything" rule on `api.`, because auth and editor responses must not be cached.
   - Cloudflare ignores `Vary: Accept` on images on the Free plan, so the first format it caches for a `?w=` URL is served to everyone. Every browser in tautan-fe's `browserslist` (Chrome/Edge/Firefox 111+, Safari 16.4+) decodes AVIF, so this is safe for supported browsers. If you need strict negotiation, add a Cache Rule: hostname `api.tautan.id` AND URI path starts with `/uploads/` AND query contains `w=` → *Bypass cache*. Origin serving is cheap.
5. Optional: under **Security → WAF → Rate limiting rules**, add 1 rule on `api.tautan.id/api/v1/auth/*` (login brute force). The API also rate-limits public endpoints itself.

### 4. Configure and start

```sh
sudo mkdir -p /opt/tautan && sudo chown deploy: /opt/tautan && cd /opt/tautan
git clone --depth 1 https://github.com/aviandri/tautan-be.git src && cp -r src/deploy/. .
mkdir -p certs backups && chmod 700 certs
$EDITOR certs/origin.pem certs/origin-key.pem       # paste from Cloudflare
cp .env.example .env && chmod 600 .env
for v in POSTGRES_PASSWORD JWT_SECRET REVALIDATE_SECRET; do sed -i "s|^$v=.*|$v=$(openssl rand -hex 32)|" .env; done
$EDITOR .env                                         # SITE_DOMAIN, API_DOMAIN, tags, R2 (step 5)
docker compose -f docker-compose.prod.yml up -d
docker compose -f docker-compose.prod.yml ps        # all healthy within ~30 s
docker compose -f docker-compose.prod.yml logs -f api   # JSON logs; "migrations applied", "tautan api listening"
```

The API applies migrations on start (`MIGRATE_ON_START=true`). With `APP_ENV=production` it refuses to start on unsafe config and logs exactly which variable is wrong.

### 5. Cloudflare R2 for uploads and offsite backups (recommended)

1. **R2 → Create bucket:** `tautan-uploads`, location hint *Asia-Pacific*. Create a second bucket, `tautan-backups`.
2. **R2 → Manage API tokens → Create:** *Object Read & Write*, scoped to those two buckets. Note the Access Key ID, the Secret and the S3 endpoint (`https://<account-id>.r2.cloudflarestorage.com`).
3. In `.env`, set `STORAGE=s3`, `S3_BUCKET=tautan-uploads`, `S3_ENDPOINT`, `S3_ACCESS_KEY_ID` and `S3_SECRET_ACCESS_KEY`, then run `docker compose -f docker-compose.prod.yml up -d api`. The API still serves `/uploads/*` itself, so URLs don't change. Leave the bucket private.
4. If you already have local uploads, copy them over once, before switching:
   ```sh
   docker run --rm -v tautan_uploads:/data:ro -e RCLONE_CONFIG_R2_TYPE=s3 -e RCLONE_CONFIG_R2_PROVIDER=Cloudflare \
     -e RCLONE_CONFIG_R2_ACCESS_KEY_ID=… -e RCLONE_CONFIG_R2_SECRET_ACCESS_KEY=… -e RCLONE_CONFIG_R2_ENDPOINT=https://<account-id>.r2.cloudflarestorage.com \
     rclone/rclone copy /data r2:tautan-uploads/uploads --exclude '.*'
   ```
5. **Backups.** The `backup` service writes a gzip `pg_dump` to `/opt/tautan/backups` every 24 h and keeps 14. Ship them offsite with a host cron entry (`crontab -e`):
   ```
   30 3 * * * docker run --rm -v /opt/tautan/backups:/b:ro --env-file /opt/tautan/rclone.env rclone/rclone copy /b r2:tautan-backups/db
   ```
   `rclone.env` holds the same `RCLONE_CONFIG_R2_*` lines as above (mode 600). Add an R2 lifecycle rule on `tautan-backups` to expire objects after 30 days.
6. **Restore drill** (do it once):
   ```sh
   gunzip -c backups/tautan-<ts>.sql.gz | docker compose -f docker-compose.prod.yml exec -T db psql -U tautan -d tautan_restore
   ```
   Create `tautan_restore` first with `createdb`.

### 6. Upgrades, rollback, operations

- **Upgrade:** pin `TAUTAN_BE_TAG` and `TAUTAN_FE_TAG` to the new `sha-xxxxxxx` tags in `.env`, then run `docker compose -f docker-compose.prod.yml pull && docker compose -f docker-compose.prod.yml up -d`. On SIGTERM the API fails `/readyz`, waits `SHUTDOWN_DELAY`, drains requests and flushes pending revalidations. Caddy health-checks `/readyz`. Expect a few seconds of 502s while a container restarts; zero-downtime needs two API instances (option A).
- **Rollback:** put the previous sha tags back and run `up -d`. Migrations only move forward. If a release added a migration, check that the old code tolerates the new schema (our migrations are additive) before rolling back.
- **Monitoring:**
  - An external uptime check (Better Stack, UptimeRobot or Cloudflare Health Checks) on `https://api.tautan.id/readyz` and `https://tautan.id/`.
  - `docker compose logs` for JSON logs; 5xx are logged at `ERROR`.
  - Disk: `df -h` and `docker system df`, and prune old images monthly with `docker image prune -a --filter until=720h`.
- **Postgres:** the defaults are fine for 4 GB. To tune, add `command: postgres -c shared_buffers=512MB -c effective_cache_size=2GB` to the `db` service.

## Notes for option A (when you outgrow the VPS)

- **Fly (API):**
  - In `fly.toml`, set `primary_region = "sin"` and `[deploy] release_command = "/api migrate"`, and set `MIGRATE_ON_START=false`.
  - Add `[[http_service.checks]] path = "/readyz"` and `kill_timeout = "30s"`, and set `SHUTDOWN_DELAY=5s`.
  - Env: `TRUSTED_PROXIES=private,cloudflare`, with Cloudflare proxying `api.` for CF-IPCountry. Without Cloudflare, use `CLIENT_IP_HEADER=Fly-Client-IP` and `TRUSTED_PROXIES=private`, and country stays blank.
  - Set `STORAGE=s3` (Fly volumes are per machine).
  - Deploy from the GHCR image: `fly deploy --image ghcr.io/aviandri/tautan-be:sha-…`.
- **Neon:**
  - Create the project in `aws-ap-southeast-1`; the region can't be changed later.
  - Use the **direct** connection string for `DATABASE_URL`. golang-migrate's advisory lock doesn't work through a transaction pooler, and pgx pools on its own anyway.
  - Turn off scale-to-zero for the production branch.
- **Vercel (FE):**
  - `vercel.json` already pins `sin1`.
  - Set `NEXT_PUBLIC_API_URL`, `NEXT_PUBLIC_SITE_URL`, `NEXT_PUBLIC_SITE_HOST` and `REVALIDATE_SECRET` in project env.
  - On the API side, set `REVALIDATE_URL=https://tautan.id/api/revalidate` (the default from `APP_BASE_URL`).
  - Vercel's own `X-Vercel-IP-Country` only applies to requests that hit Vercel. Visitor analytics events go browser → API, so country still comes from Cloudflare on `api.`.
- **Cookies:** with FE `tautan.id` and API `api.tautan.id` (same site), `COOKIE_SAMESITE=lax` works everywhere. Don't use a `*.vercel.app` FE against a `*.fly.dev` API in production: that's cross-site, and Safari blocks the refresh cookie.

## Data residency

Indonesia's PDP Law (UU 27/2022) and PP 71/2019 generally allow private-sector platforms to store personal data abroad, Singapore included, provided the transfer safeguards are met. Tautan stores creator accounts, form submissions and subscriber emails. Check with counsel before launch; if you need in-country hosting, run option B on a Jakarta VPS.

## Smoke-test checklist (after every deploy)

Run from a phone on mobile data where noted. ☐ = pass/fail.

**Infrastructure**
- ☐ `curl -s https://api.tautan.id/livez` → `{"status":"ok"}`, and `/readyz` → 200.
- ☐ `curl -sI https://tautan.id/` → 200 with `server: cloudflare` and `cf-ray` present. `curl -sI http://tautan.id/` → 301 to https.
- ☐ `docker compose ps`: every service is `healthy` or `running`, and the API logs contain no `ERROR` in the first minute.
- ☐ The API log line for a request shows `"ip"` as your real public IP, not a Cloudflare or Docker IP.

**Auth and cookies**
- ☐ Sign up on `https://tautan.id/signup` and land on the dashboard.
- ☐ DevTools → Application → Cookies → `api.tautan.id`: `tautan_refresh` is `HttpOnly`, `Secure`, `SameSite=Lax`, path `/api/v1/auth`.
- ☐ Reload the dashboard (the access token lives in memory, so a reload forces `POST /auth/refresh`): you stay logged in. Test in **Safari on iOS** too.
- ☐ Log out, then reload: you're logged out.
- ☐ (If configured) Google sign-in round trip works.

**Editor → public page (ISR + revalidate)**
- ☐ Publish a page with a link block and an image block. Open `https://tautan.id/<username>`; it renders.
- ☐ Edit the link title in the dashboard; within ~2 s a reload of the public page shows the change. The API logs show no `revalidate … status` errors. A 60 s delay means the revalidate webhook is broken: check `REVALIDATE_SECRET` on both sides.
- ☐ Rename the username: the old URL 301s to the new one.
- ☐ `curl -sI https://api.tautan.id/api/v1/public/<username>` shows `etag` and `cache-control: public, s-maxage=60, stale-while-revalidate=600`. Repeating with `-H 'If-None-Match: <etag>'` → 304.

**Uploads and images**
- ☐ Upload a JPEG avatar; it shows at once.
- ☐ After ~5 s, `curl -sI -H 'Accept: image/avif' 'https://api.tautan.id/uploads/<name>.jpg?w=320'` → `content-type: image/avif`, `cache-control: …immutable`. A second request shows `cf-cache-status: HIT`.
- ☐ With `STORAGE=s3`: the object and its `.w160/.w320…` variants appear in the R2 bucket under `uploads/`.
- ☐ Upload a PDF through a file block; the download works, and a 25 MB file uploads over mobile data.

**Analytics**
- ☐ Open the public page from a phone on mobile data and click a link. Dashboard → Analytics shows the view and the click, with country **ID** (proves CF-IPCountry plus trusted proxies).
- ☐ Spoofing check, bypassing Cloudflare: `curl -k --resolve api.tautan.id:443:<vps-ip> -X POST -H 'CF-IPCountry: US' -A 'Mozilla/5.0' -d '{"kind":"view"}' https://api.tautan.id/api/v1/public/<username>/events` → 204, but analytics shows the view with **no** country, not US. (Skip this if the cloud firewall only admits Cloudflare: the connection is refused, which is even better.)

**Performance (budget in SPEC.md)**
- ☐ PageSpeed Insights (mobile) on `https://tautan.id/<username>`: Performance ≥ 90, LCP ≤ 1.8 s, CLS ≤ 0.05.
- ☐ `curl -so /dev/null -w '%{time_starttransfer}\n' https://tautan.id/<username>` from an Indonesian vantage point (or a Jakarta VPS) → ≤ 0.2 s when warm.

**Operations**
- ☐ `ls backups/` shows a dump from the last 24 h, and the R2 `tautan-backups` bucket has it after the cron run.
- ☐ `docker compose restart api` while running a `curl` loop against `/readyz`: at most a few seconds of 502/503, then 200.
