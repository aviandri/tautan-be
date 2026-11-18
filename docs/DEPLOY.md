# Deploying Tautan

**Production setup (decided 2026-11-18):** one **Singapore VPS** running [`deploy/docker-compose.prod.yml`](../deploy/docker-compose.prod.yml), with **Caddy** for TLS and routing, **Cloudflare** (Free) in front, and **Cloudflare R2** for uploads and offsite backups. Images are pulled from **public GHCR packages**. Other hosting options are summarised in the [appendix](#appendix-a-alternatives-considered).

Tautan's visitors are mostly in Indonesia and arrive on phones from bio links. The SPEC.md budget is public-page **TTFB ≤ 200 ms cached / ≤ 600 ms uncached** and **LCP ≤ 1.8 s on Slow 4G**. This setup meets it with the following design:
- FE, API and Postgres share one box in Singapore, about 15 ms from Jakarta.
- The FE reads the API over the Docker network (`API_INTERNAL_URL`), so an uncached render never leaves the machine.
- Cloudflare's Jakarta PoP terminates TLS near visitors and caches `/uploads` images at the edge.

Every variable is documented in [ENV.md](ENV.md). It relies on tautan-be #29 (ops), #31 (proxy/cookies), #32 (storage) and #33 (image), and on tautan-fe #63 (Docker image).

```
phone ──TLS──▶ Cloudflare (CGK/SIN edge, caches /uploads)
                   │  proxied, Full (strict), Origin CA cert
                   ▼
            VPS (Singapore) ── Caddy :443 ──┬──▶ fe:3000  (Next standalone, ISR cache on disk)
                                            │        │ API_INTERNAL_URL=http://api:8080
                                            └──▶ api:8080 (Go) ──▶ db:5432 (Postgres 17)
                                                     │                 └─ backup (nightly pg_dump → ./backups → R2)
                                                     └──▶ R2 bucket (uploads + variants, STORAGE=s3)
```

## Monthly cost

List prices checked 2026-11-18, in USD, for launch-scale traffic (≈ 50k page views a month, < 10 GB of uploads):

| Piece | Choice | Cost |
|---|---|---|
| VPS | DigitalOcean SGP1, 2 vCPU / 4 GB / 80 GB (or Vultr, Linode or Lightsail in Singapore) | ≈ $24 |
| Snapshots | Provider weekly backups (DO: +20%) | ≈ $5 (optional) |
| R2 | Uploads + DB dumps; 10 GB free, then $0.015/GB, no egress fees | $0 at launch |
| Cloudflare | Free plan: DNS, proxy, TLS edge, image caching | $0 |
| GHCR | Public packages | $0 |
| Resend | Email over SMTP; the free tier covers launch volume (verify the current limits) | $0 at launch |
| **Total** | | **≈ $24–30** |

Trade-offs accepted with this choice:
- It's a single point of failure: a reboot means about 1 min down, and a deploy means a few seconds of 502s.
- We own OS patching and backup restores.
- It scales vertically only. The [appendix](#appendix-a-alternatives-considered) says when to move to managed services; nothing in the code is VPS-specific.

## Where the domain is set

**There's no production domain yet.** Everything ships with the placeholder `tautan.id` / `api.tautan.id`. Once a domain is chosen, change it in **all** of these places:

| # | Where | What to set | Takes effect |
|---|---|---|---|
| 1 | **tautan-fe repo variables** (*Settings → Secrets and variables → Actions → Variables*) | `NEXT_PUBLIC_API_URL=https://api.<domain>`, `NEXT_PUBLIC_SITE_URL=https://<domain>`, `NEXT_PUBLIC_SITE_HOST=<domain>` | The next FE image build. These are **baked into the JS bundle**, so re-run the `image` workflow, then pull. Unset variables fall back to the `tautan.id` placeholders (in `.github/workflows/image.yml`). |
| 2 | **`deploy/.env`** on the VPS | `SITE_DOMAIN=<domain>`, `API_DOMAIN=api.<domain>` | `docker compose up -d`. This one setting drives the API's `PUBLIC_BASE_URL`, `APP_BASE_URL` and `CORS_ORIGINS` (via `docker-compose.prod.yml`) **and** Caddy's site addresses. |
| 3 | **`deploy/Caddyfile`** | Nothing to edit: it reads `{$SITE_DOMAIN}` and `{$API_DOMAIN}` from #2. It also serves `www.<domain>` as a redirect. | With #2. |
| 4 | **Cloudflare** | The zone for `<domain>`; DNS `@`, `www` and `api`; an Origin CA certificate for `<domain>, *.<domain>` saved in `deploy/certs/` | Immediately. |
| 5 | **Email (Resend)** | The Resend domain `mail.<domain>`; SPF, DKIM and DMARC records in Cloudflare; `MAIL_FROM=Tautan <hello@mail.<domain>>` in `deploy/.env` | `docker compose up -d`. |
| 6 | **Google sign-in** (optional) | `GOOGLE_REDIRECT_URL=https://<domain>/auth/google/callback` in `deploy/.env`, and the same URL under *Authorised redirect URIs* in Google Cloud Console | `docker compose up -d`. |

⚠️ Choose the domain **before real users upload images**. Upload URLs are stored as absolute `https://<API_DOMAIN>/uploads/<name>`, so changing `API_DOMAIN` later breaks existing images unless the old hostname keeps pointing at the API.

## Step by step

### 0. Prerequisites

- A domain on a Cloudflare account (Free plan). You can rehearse with any domain you own and switch later; see the warning above.
- Admin access to both GitHub repos, and an SSH key.
- About an hour.

### 1. Images on GHCR (public)

1. Each repo's `image` workflow pushes on every merge to `main`: `ghcr.io/aviandri/tautan-be` and `ghcr.io/aviandri/tautan-fe` with `latest` and `sha-xxxxxxx` tags. The workflows only build and push; they never deploy.
2. Set the tautan-fe repo variables (row 1 of the domain table) **before** the FE build you plan to deploy.
3. **Manual step: make both packages public.** New GHCR packages are private, and a workflow's `GITHUB_TOKEN` can't change package visibility, so a repo admin does this once in the GitHub UI:
   1. Open the profile or org → **Packages** → `tautan-be` → **Package settings**.
   2. Under **Danger Zone → Change package visibility**, choose **Public** and type the package name to confirm.
   3. Repeat for `tautan-fe`.
   4. In each package's settings, check under **Manage Actions access** that the source repo has **Write** access, so later workflow runs can keep pushing. It's granted automatically when the workflow created the package.

   Check from any machine, with no login: `docker pull ghcr.io/aviandri/tautan-be:latest` should succeed. The VPS then pulls without a token. The images contain no secrets: all configuration is runtime env, apart from the public `NEXT_PUBLIC_*` URLs.

### 2. VPS

1. Create an Ubuntu 24.04 LTS droplet: 2 vCPU / 4 GB, region **SGP1**, your SSH key, weekly backups optional.
2. Harden it and install Docker:
   ```sh
   adduser deploy && usermod -aG sudo deploy && rsync -a ~/.ssh /home/deploy/ && chown -R deploy: /home/deploy/.ssh
   sed -i 's/^#\?PasswordAuthentication.*/PasswordAuthentication no/; s/^#\?PermitRootLogin.*/PermitRootLogin no/' /etc/ssh/sshd_config && systemctl reload ssh
   apt update && apt -y upgrade && apt -y install unattended-upgrades ufw
   ufw allow OpenSSH && ufw allow 80,443/tcp && ufw allow 443/udp && ufw enable
   curl -fsSL https://get.docker.com | sh && usermod -aG docker deploy
   ```
3. Recommended: in the provider's cloud firewall (DO: *Networking → Firewalls*), allow 80/443 **only from [Cloudflare's ranges](https://www.cloudflare.com/ips/)**, plus 22 from your IP. Docker-published ports bypass ufw, so the cloud firewall is the reliable place for this. Nobody can then reach the origin around Cloudflare.

### 3. Cloudflare

1. **DNS:** add `A @`, `A www` and `A api` records pointing at the VPS IP, all **Proxied** (orange cloud).
2. **SSL/TLS → Overview:** **Full (strict)**. **Edge Certificates:** turn on *Always Use HTTPS*, set minimum TLS 1.2, and enable HTTP/3.
3. **SSL/TLS → Origin Server → Create certificate:** RSA, hostnames `<domain>, *.<domain>`, 15 years. On the VPS (step 4), save the certificate as `deploy/certs/origin.pem` and the key as `deploy/certs/origin-key.pem`. We use the Origin CA rather than Let's Encrypt because ACME HTTP-01 fails behind Cloudflare once *Always Use HTTPS* is on.
4. **Caching:** keep the defaults. Cloudflare caches `/uploads/*.png|jpg|webp|avif` by extension, with the query string (`?w=`) in the key, and the API marks them `immutable`. **Never** add a "Cache Everything" rule on `api.`, because auth and editor responses must not be cached.
   - Cloudflare's Free plan ignores `Vary: Accept` on images, so the first format cached for a `?w=` URL is served to everyone. Every browser in tautan-fe's `browserslist` (Chrome/Edge/Firefox 111+, Safari 16.4+) decodes AVIF, so this is fine. For strict negotiation, add a Cache Rule: host `api.<domain>` AND path starts with `/uploads/` AND query contains `w=` → *Bypass cache*.
5. **Payment webhooks:** Xendit, Midtrans and Stripe POST to `https://api.<domain>/webhooks/{provider}/{profile_id}`. Caddy already proxies every path on `api.` to the API. Don't turn on *Bot Fight Mode* (it can't be bypassed per path on Free), and don't add a WAF challenge that covers `/webhooks/*`, because providers can't solve challenges. If you add WAF rules, add a *Skip* rule for URI path starts with `/webhooks/`.
6. Optional: under **Security → WAF → Rate limiting rules**, add 1 rule on `api.<domain>/api/v1/auth/*` (login brute force). The API rate-limits public endpoints itself.

### 4. Configure and start

```sh
sudo mkdir -p /opt/tautan && sudo chown deploy: /opt/tautan && cd /opt/tautan
git clone --depth 1 https://github.com/aviandri/tautan-be.git src && cp -r src/deploy/. .
mkdir -p certs backups && chmod 700 certs
$EDITOR certs/origin.pem certs/origin-key.pem       # paste from Cloudflare
cp .env.example .env && chmod 600 .env
for v in POSTGRES_PASSWORD JWT_SECRET REVALIDATE_SECRET PAYMENTS_ENCRYPTION_KEY; do sed -i "s|^$v=.*|$v=$(openssl rand -hex 32)|" .env; done
$EDITOR .env                                         # SITE_DOMAIN, API_DOMAIN, image tags, R2 (step 5)
docker compose -f docker-compose.prod.yml up -d      # pulls the public images; no docker login needed
docker compose -f docker-compose.prod.yml ps         # all healthy within ~30 s
docker compose -f docker-compose.prod.yml logs -f api   # JSON logs: "migrations applied", "tautan api listening"
```

The API applies migrations on start (`MIGRATE_ON_START=true`). With `APP_ENV=production` it refuses to start on unsafe config and logs which variable is wrong. `.env` is the only file holding secrets; back it up somewhere safe, such as a password manager. `PAYMENTS_ENCRYPTION_KEY` matters most: without it, every creator's stored payment-provider keys are unreadable.

### 5. R2 for uploads and offsite backups

1. **R2 → Create bucket:** `tautan-uploads`, location hint *Asia-Pacific*. Create a second bucket, `tautan-backups`. Both stay private.
2. **R2 → Manage API tokens → Create:** *Object Read & Write*, scoped to those two buckets. Note the Access Key ID, the Secret and the S3 endpoint (`https://<account-id>.r2.cloudflarestorage.com`).
3. In `.env`, set `STORAGE=s3`, `S3_BUCKET=tautan-uploads`, `S3_ENDPOINT`, `S3_ACCESS_KEY_ID` and `S3_SECRET_ACCESS_KEY`, then run `docker compose -f docker-compose.prod.yml up -d api`. The API still serves `/uploads/*` itself, so URLs don't change. Do this **before launch**, so local disk never holds production uploads.
4. If uploads already exist on the local volume, copy them once, then switch:
   ```sh
   docker run --rm -v tautan_uploads:/data:ro -e RCLONE_CONFIG_R2_TYPE=s3 -e RCLONE_CONFIG_R2_PROVIDER=Cloudflare \
     -e RCLONE_CONFIG_R2_ACCESS_KEY_ID=… -e RCLONE_CONFIG_R2_SECRET_ACCESS_KEY=… -e RCLONE_CONFIG_R2_ENDPOINT=https://<account-id>.r2.cloudflarestorage.com \
     rclone/rclone copy /data r2:tautan-uploads/uploads --exclude '.*'
   ```
5. **Backups.** The `backup` service writes a gzip `pg_dump` to `/opt/tautan/backups` every 24 h and keeps the newest 14. Ship them offsite with a host cron entry (`crontab -e`):
   ```
   30 3 * * * docker run --rm -v /opt/tautan/backups:/b:ro --env-file /opt/tautan/rclone.env rclone/rclone copy /b r2:tautan-backups/db
   ```
   `rclone.env` (mode 600) holds the same `RCLONE_CONFIG_R2_*` lines as above. Add an R2 lifecycle rule on `tautan-backups` that expires objects after 30 days.
6. **Restore drill** (do it once before launch):
   ```sh
   docker compose -f docker-compose.prod.yml exec db createdb -U tautan tautan_restore
   gunzip -c backups/tautan-<ts>.sql.gz | docker compose -f docker-compose.prod.yml exec -T db psql -U tautan -d tautan_restore
   ```

### 6. Email via Resend (password resets, notifications)

Decided: **Resend over SMTP**, sending from the `mail.<domain>` subdomain. Until this step `MAIL_DRIVER=log`: the API runs, but nothing is sent, and it logs a startup warning.

1. **Manual step: the user creates a Resend account** at resend.com. In **Domains → Add domain**, enter `mail.<domain>` and choose the region nearest Singapore/Asia if one is offered.
2. Resend then shows the DNS records to add. In **Cloudflare DNS**, add them exactly as shown, all **DNS only** (grey cloud):
   - the **SPF** TXT record (and the MX record for the bounce subdomain, if Resend lists one);
   - the **DKIM** TXT record (`resend._domainkey.mail.<domain>`);
   - **DMARC**, a TXT record at `_dmarc.mail.<domain>`: `v=DMARC1; p=quarantine; rua=mailto:<your inbox>`. Start with `p=none` for a week if you want to watch the reports first.

   Click **Verify** in Resend; it usually turns green within minutes.
3. In Resend, go to **API Keys → Create API key** with *Sending access* restricted to `mail.<domain>`. This key is the SMTP password, and Resend shows it only once.
4. In `deploy/.env`, set:
   ```
   MAIL_DRIVER=smtp
   MAIL_FROM=Tautan <hello@mail.<domain>>
   SMTP_HOST=smtp.resend.com
   SMTP_PORT=587
   SMTP_USERNAME=resend
   SMTP_PASSWORD=re_…            # the API key
   ```
   ⚠️ With `MAIL_DRIVER=smtp`, **`MAIL_FROM` must be a real address on the verified domain**. With the default `no-reply@localhost`, or a missing `SMTP_HOST`, the API refuses to start. Then run `docker compose -f docker-compose.prod.yml up -d api` and check the logs for the mail worker starting without errors.
5. DigitalOcean (like most VPS providers) blocks outbound port 25, which is why this uses 587 (STARTTLS). 465 (implicit TLS) also works.

Queued mail lives in Postgres (`email_outbox`), so a restart or a Resend outage delays mail without losing it.

### 7. Upgrades, rollback, operations

- **Upgrade:** set `TAUTAN_BE_TAG` and `TAUTAN_FE_TAG` in `.env` to the new `sha-xxxxxxx` tags (shown on each package's GHCR page), then run `docker compose -f docker-compose.prod.yml pull && docker compose -f docker-compose.prod.yml up -d`. On SIGTERM the API fails `/readyz`, waits `SHUTDOWN_DELAY`, drains requests and flushes pending page revalidations. Expect a few seconds of 502s while a container restarts.
- **Rollback:** put the previous sha tags back, then run `up -d`. Migrations only move forward, and ours are additive, so older code tolerates a newer schema. Check the release notes if a migration dropped anything.
- **Monitoring:**
  - An external uptime check (Better Stack, UptimeRobot or Cloudflare Health Checks) on `https://api.<domain>/readyz` and `https://<domain>/`.
  - `docker compose logs` for JSON logs; 5xx responses are logged at `ERROR`.
  - Disk: `df -h` and `docker system df`. Prune old images monthly with `docker image prune -a --filter until=720h`.
- **Postgres:** the defaults are fine on 4 GB. To tune, add `command: postgres -c shared_buffers=512MB -c effective_cache_size=2GB` to the `db` service.
- **Run one FE container only:** the ISR cache lives inside it.

## Smoke-test checklist (after every deploy)

Replace `<domain>` with the real one. Run from a phone on mobile data where noted. ☐ = pass/fail.

**Infrastructure**
- ☐ `docker pull ghcr.io/aviandri/tautan-fe:latest` works with no login (packages are public).
- ☐ `curl -s https://api.<domain>/livez` → `{"status":"ok"}`, and `/readyz` → 200.
- ☐ `curl -sI https://<domain>/` → 200 with `server: cloudflare` and `cf-ray` present. `curl -sI http://<domain>/` → 301 to https, and `https://www.<domain>/` → 301 to the apex.
- ☐ `docker compose ps`: every service is `healthy` or `running`, and the API logs have no `ERROR` in the first minute.
- ☐ The API log line for your request shows `"ip"` as your real public IP, not a Cloudflare or Docker address.

**Auth and cookies**
- ☐ Sign up at `https://<domain>/signup` and land on the dashboard.
- ☐ DevTools → Application → Cookies → `api.<domain>`: `tautan_refresh` is `HttpOnly`, `Secure`, `SameSite=Lax`, path `/api/v1/auth`.
- ☐ Reload the dashboard (the access token is in memory, so a reload forces `POST /auth/refresh`): you stay logged in. Test **Safari on iOS** too.
- ☐ Log out, then reload: you're logged out.
- ☐ (If configured) Google sign-in round trip works.
- ☐ "Forgot password" sends a reset email to a real inbox (not spam), and the link works. With `MAIL_DRIVER=log`, expect only the startup warning and no email.

**Editor → public page (ISR + revalidate)**
- ☐ Publish a page with a link block and an image block. `https://<domain>/<username>` renders.
- ☐ Edit the link title; within ~2 s a reload of the public page shows it, and the API logs show no `revalidate … status` errors. If it takes ~60 s, the webhook is broken: check that `REVALIDATE_SECRET` reaches both containers.
- ☐ Rename the username: the old URL 301s to the new one.
- ☐ `curl -sI https://api.<domain>/api/v1/public/<username>` shows `etag` and `cache-control: public, s-maxage=60, stale-while-revalidate=600`. Repeating with `-H 'If-None-Match: <etag>'` → 304.

**Uploads and images**
- ☐ Upload a JPEG avatar; it shows at once.
- ☐ After ~5 s, `curl -sI -H 'Accept: image/avif' 'https://api.<domain>/uploads/<name>.jpg?w=320'` → `content-type: image/avif` and `cache-control: …immutable`. A second request shows `cf-cache-status: HIT`.
- ☐ The object and its `.w160/.w320…` variants are in the R2 bucket under `uploads/`, and `docker compose exec api ls /data/uploads` is empty.
- ☐ Upload a PDF in a file block; the download works, and a 25 MB file uploads over mobile data.

**Analytics**
- ☐ From a phone on mobile data, open the public page and click a link. Analytics shows the view and the click with country **ID** (proves CF-IPCountry and the trusted proxies work).
- ☐ Spoofing check, bypassing Cloudflare: `curl -k --resolve api.<domain>:443:<vps-ip> -X POST -H 'CF-IPCountry: US' -A 'Mozilla/5.0' -d '{"kind":"view"}' https://api.<domain>/api/v1/public/<username>/events`. With the Cloudflare-only firewall the connection is refused (best). Otherwise expect a 204, with the view recorded **without** a country, not US.

**Performance (SPEC.md budget)**
- ☐ PageSpeed Insights (mobile) on `https://<domain>/<username>`: Performance ≥ 90, LCP ≤ 1.8 s, CLS ≤ 0.05.
- ☐ `curl -so /dev/null -w '%{time_starttransfer}\n' https://<domain>/<username>` from an Indonesian vantage point → ≤ 0.2 s when warm.

**Payments** (once tautan-be #36 and its webhook PR are deployed)
- ☐ The API starts with `APP_ENV=production` and logs no complaint about `PAYMENTS_ENCRYPTION_KEY`.
- ☐ `curl -s -o /dev/null -w '%{http_code}\n' -X POST https://api.<domain>/webhooks/xendit/00000000-0000-0000-0000-000000000000` returns the API's own 4xx JSON, not a Cloudflare challenge page. Before the webhook handler PR is deployed, chi's plain-text `404 page not found` is expected; it still proves the request reached the API.

**Operations**
- ☐ `ls backups/` shows a dump less than 24 h old, and R2 `tautan-backups/db` has it after the cron run.
- ☐ `docker compose restart api` during a `curl` loop on `/readyz`: at most a few seconds of 502/503, then 200.

---

## Appendix A: alternatives considered

Both alternatives cost 2–3× more at launch. Revisit them when one of these becomes true: you need more than one API instance, a single box can't keep up with AVIF encoding, or reboot downtime becomes unacceptable. The code already supports them: S3 storage, a stateless API, and an FE that builds for Vercel.

| Option | Pieces | ≈ USD/month | Why not now |
|---|---|---|---|
| **Managed** | Vercel Pro (`sin1`, which `vercel.json` already pins) + Fly.io `sin` (2 × shared-cpu-1x/1 GB) + Neon Launch `aws-ap-southeast-1` (always on) + R2 | $50–75 | Four vendors and bills. Neon needs scale-to-zero off (≈ $19+/mo for 0.25 CU). The API's in-memory rate limits multiply per instance. Vercel Hobby forbids commercial use. |
| **Vercel + Railway** | Vercel Pro + Railway Pro (API + Postgres container) in Singapore + R2 | $40–55 | Postgres is a container with a volume (no managed HA or PITR), so we'd still run backups. Usage billing is harder to predict. |

Notes for moving to the managed option later:
- **Fly:** `primary_region = "sin"`, `[deploy] release_command = "/api migrate"` with `MIGRATE_ON_START=false`, an http check on `/readyz`, `kill_timeout = "30s"` and `SHUTDOWN_DELAY=5s`. Keep Cloudflare proxying `api.` with `TRUSTED_PROXIES=private,cloudflare`. Without Cloudflare, use `CLIENT_IP_HEADER=Fly-Client-IP` and `TRUSTED_PROXIES=private`, and country stays blank. Deploy with `fly deploy --image ghcr.io/aviandri/tautan-be:sha-…`.
- **Neon:** pick the region at creation (it can't be changed later). Use the **direct** connection string, because golang-migrate's advisory lock doesn't work through a transaction pooler.
- **Vercel:** set `NEXT_PUBLIC_*` and `REVALIDATE_SECRET` in project env. On the API, set `REVALIDATE_URL=https://<domain>/api/revalidate`.
- **Cookies:** keep the FE and API on the same registrable domain (`<domain>` + `api.<domain>`). A `*.vercel.app` FE against a `*.fly.dev` API is cross-site, and Safari drops the refresh cookie.

## Appendix B: latency and data residency

| Path | Typical RTT |
|---|---|
| Jakarta phone → Cloudflare edge (CGK/SIN) | 5–20 ms (plus 4G radio, 50–150 ms) |
| Jakarta ↔ Singapore data centre | 12–20 ms |
| Jakarta ↔ US East (Vercel's default `iad1`) | 230+ ms |

FE, API and DB must share a region: an uncached render makes FE → API → DB round trips.

Indonesia's PDP Law (UU 27/2022) and PP 71/2019 generally allow private-sector platforms to store personal data abroad, Singapore included, provided the transfer safeguards are met. Tautan stores creator accounts, form submissions and subscriber emails. Confirm with counsel before launch. If in-country hosting is required, the same compose stack runs unchanged on a Jakarta VPS (Biznet Gio, IDCloudHost).
