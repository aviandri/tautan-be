# Security audit, November 2026 (pre-launch)

**Scope:**
- tautan-be `main` at `b153f68` and tautan-fe `main` at `b2e21fa` (2026-11-19).
- The payment code (be#41/#42/#44/#48, fe#80–#82/#86) was reviewed separately in its own PRs and is only re-checked here where it overlaps (uploads, IDOR).

**Method:** code reading of every area below. Key claims were checked against the code, and the SSRF blocklist against a scratch copy of `publicIP`. File and line references are to the commits above.

**Severity:** **High** means exploitable by an anonymous attacker with real impact; fix before launch. **Medium** means exploitable with preconditions, or a DoS. **Low** means defence in depth or needs an unlikely precondition. **Info** is a note.

## Summary

| # | Sev | Area | Finding | Status |
|---|-----|------|---------|--------|
| H1 | High | Auth | No rate limit on sign-in (or sign-up) | Fixed in be#52 |
| M1 | Medium | Rate limits | Limiter map can grow without bound; past 10k keys, every hit scans it under one lock | Fixed in be#55 |
| M2 | Medium | Auth / CSRF | JSON endpoints accept `text/plain`, so a cross-site form can log a victim into the attacker's account (login CSRF) | Fixed in be#56 |
| M3 | Medium | Auth | Refresh rotation has no reuse detection | Fixed in be#57 |
| L1 | Low | Uploads | Lead-magnet download serves any stored file, including another seller's private `p_` deliverable | Fixed in be#54 |
| L2 | Low | Headers | No `frame-ancestors` / `X-Frame-Options` on the dashboard | Fixed in fe#90 |
| L3 | Low | Headers | No CSP anywhere; no Permissions-Policy; Caddy-only headers are missing on Vercel | fe#90 (headers); CSP in Report-Only in fe#94, enforce after the Mac run |
| L4 | Low | SSRF | `publicIP` passes `::/96` (IPv4-compatible), `::ffff:0:0:0/96`, `fec0::/10` and `192.88.99.0/24` | Fixed in be#58 |
| L5 | Low | Uploads | Served uploads have no `CSP: sandbox` or CORP | Fixed in be#58 (CSP sandbox; CORP skipped on purpose) |
| L6 | Low | Uploads | No cap on declared image dimensions | Fixed in be#58 |
| L7 | Low | IDOR (defence in depth) | Some child/secondary UPDATEs carry no `profile_id` scope | Accepted for launch |
| L8 | Low | Integrity | Stored ids (announcement `form_block_id`, link `dest_kind=tab` `tab_id`) aren't checked for ownership | Accepted for launch |
| L9 | Low | Auth | `/auth/refresh`, `/auth/username-available` and the Google endpoints have no rate limit | Fixed in be#58 |
| I1–I8 | Info | various | See the end | I6, I8 fixed in fe#95; the rest accepted |

**No Critical findings.** No exploitable XSS, SSRF, path traversal or cross-tenant read or write was found.

---

## High

### H1: No rate limit on sign-in or sign-up (fixed in be#52)
- **Where:** `internal/server/auth.go:107` (`handleLogin`) and `:41` (`handleSignup`). Every other public POST has a limiter; these two had none.
- **Impact:**
  - Unlimited online password guessing and credential stuffing against any account. bcrypt cost is the only brake.
  - Unlimited signups, and each one emails a confirmation link to an address the caller chooses.
- **Fix (be#52):**
  - Login: 60 attempts per 15 min per IP; 10 **failed** attempts per 15 min per account (lowercased email, any IP) through the new `rateLimiter.Exceeded`.
  - Signup: 20 per hour per IP.
  - Trade-off: 10 bad guesses lock password sign-in on that account for 15 minutes. Reset and Google sign-in still work.

## Medium

### M1: Rate limiter memory and CPU under key rotation
- **Where:** `internal/server/ratelimit.go:27-48`. Keys come from the caller, e.g. `engage.go:253` (`ip|visitor_id|block`) and `:308`.
- **Problem:** `Allow` inserts a key on every new value and only prunes *expired* windows, and only once the map passes 10,000 keys. One IP rotating `visitor_id` adds a key per request.
  - At 253/308 the per-key limiter runs before the per-IP one (`||` short-circuit), so the key is inserted even when the IP is over its limit.
  - Past 10k live keys, every `Allow` on that limiter iterates the whole map under its mutex. Memory grows, and every vote/ask request turns O(n).
- **Fix:**
  - Check the coarse per-IP limiter first.
  - Hard-cap the map: when it's full of live windows, refuse new keys (fail closed for that limiter) instead of scanning on every call.
  - Or prune on a ticker rather than inline.

### M2: Login CSRF via `text/plain` JSON
- **Where:** `internal/server/respond.go:31` (`decodeJSON` ignores `Content-Type`).
- **Exploit:** an attacker page auto-submits `<form method=post enctype=text/plain action=https://api…/api/v1/auth/login>` with a field named `{"email":"attacker@x","password":"pw","x":"` and value `"}`, which produces a valid JSON body.
  - A cross-site top-level form POST is a "simple" request (no preflight).
  - The response sets the attacker's refresh cookie. On the victim's next visit, the FE refreshes into the **attacker's** account, and anything the victim then enters (a page, payment keys) lands there.
  - Signup is affected the same way.
- **Not affected:** every other cookie use. The API authorises with a Bearer header, not cookies, so ordinary CSRF doesn't apply.
- **Fix:** require `Content-Type: application/json` in `decodeJSON`. Then a cross-site form can't produce it, and `fetch` needs a CORS preflight that the allow-list rejects.
  - Exempt the beacon endpoint `POST /public/{u}/events`, which deliberately takes `text/plain` (`public.go:151`).
  - The form-post handlers (`order_forms.go`) parse forms and are unaffected.
  - The FE `api.ts` already sends JSON.

### M3: Refresh-token reuse isn't detected
- **Where:** `internal/server/auth.go:177` (`handleRefresh`).
- **Problem:** a rotated (revoked) token presented again just gets a 401. If a refresh token is stolen and used once, the thief keeps a valid rotation chain and the real user is silently logged out. Nothing ends the thief's chain.
- **Fix:**
  - Record a family id per login.
  - When a revoked-but-unexpired token is presented, revoke the whole family and stamp `sessions_revoked_at`.
  - Allow a few seconds of grace for two tabs refreshing at once, so concurrent refreshes don't trip it.

## Low

### L1: Lead-magnet download can serve private `p_` files (fixed in be#54)
- **Where:**
  - `internal/server/engage.go:449-462`: `handleLeadDownload` opens `path.Base(file_url)` from the upload store with no name check.
  - `checkLeadMagnet` (`engage.go:123`) accepts any `/uploads/…` URL.
  - `lead_magnet` isn't covered by `privatizeDeliverables` (`private_uploads.go:91`).
- **Exploit:** seller A saves `file_url: "/uploads/p_<B's file>"` on A's own lead magnet, then downloads B's paid deliverable for free.
- **Why only Low:** A must already know B's 128-bit random name. No leak path was found: public reads strip it, buyers see only the file name, and download links are signed.
- **Fix:** serve only names matching `storedNameRE` from `handleLeadDownload`.

### L2: Clickjacking of the dashboard (fixed in fe#90)
- **Where:**
  - `tautan-fe/next.config.ts:42` (`headers()`) and `deploy/Caddyfile:23-30`.
  - There's no `frame-ancestors` or `X-Frame-Options` anywhere.
- **Mitigation already in place:** with the default `COOKIE_SAMESITE=lax`, a cross-site iframe doesn't send the refresh cookie, so a framed dashboard isn't signed in. It becomes exploitable with `COOKIE_SAMESITE=none`.
- **Fix:** `X-Frame-Options: SAMEORIGIN` and `CSP: frame-ancestors 'self'` on every route except the public pages `/[username]`.

### L3: No CSP; no Permissions-Policy; headers depend on Caddy
- **Where:** same as L2.
- **Notes:**
  - The sanitisers hold today (see "Verified" below), but nothing backs them up.
  - The HSTS, nosniff and Referrer-Policy headers exist only in the Caddyfile, so a Vercel deploy of the FE gets none of them (browsers default Referrer-Policy to strict-origin-when-cross-origin anyway; HSTS on Vercel comes from the platform).
- **Fix:**
  - nosniff and Permissions-Policy are now also sent from `next.config.ts` (fe#90).
  - A CSP is still open. Start in Report-Only:
    - `default-src 'self'; script-src 'self' 'unsafe-inline' https://www.googletagmanager.com https://connect.facebook.net; frame-src` with the embed allow-list from `src/lib/embeds.ts`; `object-src 'none'; base-uri 'none'; form-action 'self' <API origin>`.
    - `'unsafe-inline'` stays for Next's bootstrap and the Announcement inline script.
    - Nonces would break ISR on `/[username]`, so use them on the dashboard only.

### L4: SSRF blocklist IPv6 gaps
- **Where:** `internal/server/fetch.go:40` (`publicIP`).
- **What passes:** `::127.0.0.1` (`::/96`), `::ffff:0:7f00:1` (SIIT), `fec0::/10` and `192.88.99.0/24`.
- **Exploitability:** only if the host routes those ranges (stock Linux without sit/NAT64 doesn't).
- **Fix:** for IPv6, allow only `2000::/3`, then apply the existing blocklist; add `192.88.99.0/24`.

### L5: Served uploads lack sandboxing headers
- **Where:** `internal/server/uploads.go:148-179`. Only nosniff is set.
- **Fix:** add `Content-Security-Policy: default-src 'none'; sandbox` and `Cross-Origin-Resource-Policy: same-site` to `/uploads/*` and both download handlers.

### L6: No cap on declared image dimensions
- **Where:** `uploads.go:103`. `DecodeConfig` only has to succeed.
- **Problem:** a 5 MB PNG declaring 60000×60000 is accepted. Variants are skipped above 40 MP, but the original is served and can hang visitors' tabs.
- **Fix:** reject uploads above 40 MP or 16384 px per side.

### L7: Secondary UPDATEs without a profile scope
- **Where:**
  - `blocks.go:235, 266, 320, 338` (children on move, delete and restore)
  - `engage.go:630` (`UPDATE questions … WHERE id`)
  - `cart.go:245, 255` (ship)
- **Status:** each follows a pid-scoped check of the same id inside the transaction, so none is exploitable today.
- **Fix:** add `AND profile_id = $pid` to each (defence in depth).

### L8: Stored ids not checked for ownership
- **Where:** the announcement `form_block_id` (`extras.go:63-66`) and a link's `dest_kind=tab` `tab_id` (`blockdata.go:254`) are only checked to be UUID-shaped.
- **Impact:** none observed. The FE resolves both within the page, and form posts are scoped to `{username}`.
- **Fix:** check them against pid, as `checkBlockRefs` does.

### L9: Unlimited auth side endpoints
- **Where:**
  - `auth.go:177` `/auth/refresh` (needs a valid cookie, so cheap)
  - `auth.go:222` `/auth/username-available` (username enumeration is by design, but unthrottled)
  - `/auth/google/start` and `/auth/google/callback` (state-bound)
- **Fix:** a coarse per-IP limiter, e.g. 120 per minute.

## Info
- **I1:** `handleLeadDownload` and paid downloads 302 to the seller's external URL (`engage.go:464`, `orders.go:948`). This is an open redirect limited to http(s) URLs that passed `checkWebURL`; acceptable by design.
- **I2:** images uploaded with `?kind=file` (up to 25 MB) never get variants, because `processImage` reads at most 5 MB (`images.go:254`). Keep images at 5 MB.
- **I3:** making a once-free upload paid copies it to a `p_` name but leaves the public original reachable and cached as immutable (`private_uploads.go:67-81`). Document this, or delete the original when no block references it.
- **I4:** embed iframes have no `sandbox` (`tautan-fe/src/components/public/runtime-core.ts:228-243`). Hosts are allow-listed (`src/lib/embeds.ts`).
- **I5:** `SellBlocksB.tsx:47, 194-195` use `image_url` and `qris_image_url` without `safeImageSrc`. The BE already restricts both to http(s) or `/uploads/`, and `img src` can't run script.
- **I6:** `FormResponses.tsx:107` builds `mailto:${text}` unencoded, which allows parameter injection (pre-filled cc/body) but no script. Use `encodeURIComponent`, as `Inbox.tsx:113` does.
- **I7:** `profiles` has no `UNIQUE (user_id)`. `withProfile` and `profileForUser` pick the oldest profile (`profile.go:22`, `auth.go:321`). Safe while signup is the only creator.
- **I8:** `/api/revalidate` (FE) reads an unbounded body before verifying the HMAC (`src/app/api/revalidate/route.ts:22`). Cap it at 4 KB.

## Accepted for launch

These stay open on purpose. Each was re-checked on 2026-11-19 and is either not exploitable today or costs more to change than it protects. Revisit them when the code around them changes.

- **L7:** secondary UPDATEs without a `profile_id` scope (`blocks.go:235, 266, 320, 338`, `engage.go:630`, `cart.go:245, 255`). Each runs after a pid-scoped check of the same id inside the same transaction. Add the scope when those queries are next touched.
- **L8:** the stored announcement `form_block_id` and the link `tab_id` aren't owner-checked. Both are only resolved within the owner's own page, and form posts are scoped to `{username}`, so a foreign id does nothing.
- **I1:** downloads 302 to the seller's own external URL. This is by design, and limited to http(s) URLs that passed `checkWebURL`.
- **I2:** images uploaded with `kind=file` over 5 MB get no variants. This is a quality issue (served at original size), not a security one.
- **I3:** a once-free upload that later becomes paid stays reachable at its old public URL. Documented: sellers who need it private should upload the file again as a paid file.
- **I4:** embed iframes have no `sandbox`. Their sources are allow-listed hosts, and `frame-src` (fe#94) restricts them again.
- **I5:** two image `src` values skip `safeImageSrc` on the FE. The BE restricts both fields to http(s) or `/uploads/`, and `img src` can't run script.
- **I7:** there's no `UNIQUE (profiles.user_id)`. Signup is the only place profiles are created; add the constraint before any multi-profile feature.

## Fix log

| PR | Fixes |
|----|-------|
| be#52 | H1: sign-in and sign-up rate limits |
| be#54 | L1: lead-magnet download serves only public files |
| fe#90 | L2 and part of L3: anti-framing on app and checkout routes; nosniff and Permissions-Policy |
| be#55 | M1: bounded limiter memory; per-IP ceilings checked first |
| be#56 | M2: `requireJSON` on sign-in and token endpoints |
| be#57 | M3: refresh-token families with reuse detection (migration 000024) |
| fe#94 | L3: CSP in Report-Only plus `/api/csp-report`. Journey (CSP_STRICT) and a GA4/Pixel/embeds check showed 0 violations. Enforce after the Mac run. |
| be#58 | L4, L5, L6, L9 |
| fe#95 | I6, I8 |

---

## Verified (coverage)

**Auth and session**
- **Access tokens:** HS256 only, with issuer, iat and expiry checked and the `sessions_revoked_at` cut-off applied (`tokens.go:36-48, 113-140`).
- **Secrets:** `JWT_SECRET` must be at least 32 random characters in production (`config.go:154`). Purpose-separated HMACs (`sign("lead"|…)`) and the OAuth state (issuer `tautan-oauth`) can't be replayed as access tokens.
- **Refresh tokens:** a 32-byte random value stored as sha256. The cookie is `HttpOnly`, `Secure` from config, `SameSite=Lax` by default and path `/api/v1/auth` (`tokens.go:55-110`). It is rotated on every refresh and revoked on logout, password reset and email change.
- **CORS:** an exact-origin allow-list with credentials. `*` and non-https origins are rejected in production (`server.go:143`, `config.go:157-164`).
- **CSRF:** every owner endpoint authorises with a Bearer header, so cookie CSRF is limited to the auth endpoints (see M2).

**Client IP and rate limits**
- `clientIP` resolves through `TRUSTED_PROXIES` (`clientip.go`), so per-IP limits see the visitor, not the proxy.
- Public POSTs are all limited: contact, form, subscribe, lead, enter, ask, vote, unlock, orders, lookup, events, reset, verify and email change. Webhooks are limited per page.

**Revalidate webhook (FE)**
- HMAC-SHA256 over `ts.body`, a ±5 min window, a constant-time compare, and a validated username list; 503 when the secret is unset (`src/lib/revalidate.ts`).

**Uploads**
- **Type and serving:** the type is sniffed from the bytes, and HTML, XML and SVG are refused (`uploads.go:75-89`). Only raster images are served inline, with nosniff; everything else is served as an attachment.
- **Names and paths:** names are random 128-bit plus our extension. The serve pattern `^[0-9a-f]{32}…` blocks traversal, and the stores reject separators again.
- **Size:** `MaxBytesReader` caps uploads.
- **Private files:** `p_` names are never served by `/uploads`.
- **Image bombs:** decompression bombs are skipped above 40 MP.

**SSRF**
- The IP check runs in `Dialer.Control` on the resolved address, which covers DNS rebinding and every redirect. Metadata, loopback, RFC1918, CGNAT, NAT64, 6to4 and Teredo addresses are blocked.
- Request rules: http(s) only, default ports, no userinfo, at most 3 redirects, `Proxy: nil`, a 3 s timeout, and a size limit applied after gzip decoding.
- unfurl, import, image rehosting, oEmbed, post_grid, YouTube and RSS all use the safe fetcher (`fetch.go`, `unfurl.go:313`, `importer.go:245,301`, `remote.go`).

**XSS**
- No `dangerouslySetInnerHTML` on user data. The Announcement inline script holds only server ids, JSON-escaped with `<` escaped. QR SVG comes from our own URL.
- Mini-markdown builds React nodes.
- `safeHref` uses the WHATWG URL parser plus a scheme allow-list (`src/lib/safe-url.ts`). The BE `checkURL` / `checkWebURL` / `checkImageURL` reject `javascript:`.
- Theme values are regex-limited on both sides and written as inline custom properties.
- Metadata goes through the Next Metadata API; there's no JSON-LD.
- Visitor content in the dashboard is rendered as text. Emails use `html/template`.

**Tenant isolation**
- Every owner route resolves `pid` through `withProfile` (`profile.go:19-33`), and every id-taking query is scoped to it. The full route table is in the audit notes on the PR.
- Public block endpoints check the published profile, visibility, soft-delete, schedule and gate.
- A seller can't sell another seller's private upload through file, product or shop cards (`private_uploads.go:55-66`). Lead magnets were the exception; see L1.
