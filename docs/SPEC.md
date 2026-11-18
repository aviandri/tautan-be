# Tautan — link-in-bio product (spec v0.1)

Tautan ("link" in Indonesian) is an original link-in-bio product with the same feature
set as liinks.co. **Use our own name, copy, colours and visual design; don't copy
their branding, text or assets.** We match features, not looks.

Repos (cloned under /home/claude/tautan):
- `tautan-be`: Go API, https://github.com/aviandri/tautan-be
- `tautan-fe`: Next.js app, https://github.com/aviandri/tautan-fe

Status: researched from liinks.co's public marketing page only. A detailed UI
walk-through, done in a browser on the Mac session, will come in as `RESEARCH.md`
and may change things. Build for the MVP scope below; keep the code easy to extend.

## MVP scope (phase 1)

1. **Auth**: email + password sign-up and login, JWT access token plus a refresh token in
   an httpOnly cookie. You pick a username at sign-up; it becomes your page at `/{username}`.
2. **Profile page editor** (dashboard):
   - Profile: display name, bio, avatar image upload
   - Blocks, ordered by drag and drop, each one can be hidden: `link`, `text`, `image`,
     `video` (YouTube/Vimeo embed), `music` (Spotify/SoundCloud embed), `social_icons`,
     `folder` (holds child links), `faq`, `testimonial`, `contact_form`
   - Tabs: a page has 1..n tabs and every block belongs to a tab
   - Live phone preview beside the editor
   - Publish toggle (`is_published`): unpublished pages return 404 to the public
3. **Themes**: about 8 preset themes plus advanced overrides (background colour,
   gradient or image, button style fill/outline/shadow, button radius, font family from a
   curated Google Fonts list, text colour)
4. **Public page** `/{username}`: server-rendered, fast, a good SEO/OG preview
   (custom OG title, description and image), tab navigation (`?tab=slug` opens that
   tab), a working contact form
5. **Analytics**: records page views and link clicks with referrer, country (from a
   header) and device. The dashboard shows totals, a time series (7/30/90 days), top
   links and top referrers.
6. **QR code** for the page URL (download as PNG/SVG), generated on the FE.
7. **Contact form submissions**: an inbox list in the dashboard.

Phase 2 (later, don't build yet): import from Instagram or Linktree, custom domains,
multiple profiles per account, billing (Starter/Pro, 14-day trial), AI chat, public API.

## Data model (Postgres)

```
users(id uuid pk, email unique, password_hash, created_at)
profiles(id uuid pk, user_id fk, username unique citext, display_name, bio,
         avatar_url, theme jsonb, seo jsonb, is_published bool, created_at, updated_at)
tabs(id uuid pk, profile_id fk, title, slug, position int)
blocks(id uuid pk, profile_id fk, tab_id fk, parent_id fk null (folder children),
       type text, position int, is_visible bool, data jsonb, created_at, updated_at)
events(id bigserial, profile_id, block_id null, kind 'view'|'click', referrer,
       country, device, created_at)          -- index (profile_id, created_at)
contact_submissions(id uuid, profile_id, block_id, name, email, message, created_at)
```

`blocks.data` holds one JSON shape per type (this file is the source of truth; FE and BE keep
the same TS/Go types):
- link `{title, url, thumbnail_url?}`
- text `{content}` (markdown subset)
- image `{url, alt?, link_url?}`
- video `{url}`, music `{url}` (the FE turns the URL into an embed)
- social_icons `{items:[{platform, url}]}`
- folder `{title}` (children are blocks of type link with parent_id = folder)
- faq `{items:[{q, a}]}`
- testimonial `{items:[{quote, author, avatar_url?}]}`
- contact_form `{title, button_label}`

`theme` jsonb: `{preset, bg:{type:'color'|'gradient'|'image', value}, button:{style, radius, color, text_color}, font, text_color}`

## REST API (base `/api/v1`, JSON, errors: `{error:{code,message}}`)

Auth
- `POST /auth/signup {email,password,username}` → `{access_token, user, profile}` and sets the refresh cookie
- `POST /auth/login {email,password}` → same shape
- `POST /auth/refresh` (cookie) → `{access_token}`
- `POST /auth/logout`
- `GET  /auth/username-available?u=` → `{available}`

Owner (Bearer token)
- `GET /me` → `{user, profile}`
- `PATCH /profile {display_name,bio,avatar_url,theme,seo,is_published,username}`
- `GET /profile/editor` → `{profile, tabs, blocks}` (all blocks, hidden ones included)
- `POST /tabs`, `PATCH /tabs/{id}`, `DELETE /tabs/{id}`, `PUT /tabs/order {ids:[]}`
- `POST /blocks {tab_id,type,data,parent_id?}`, `PATCH /blocks/{id}`, `DELETE /blocks/{id}`
- `PUT /blocks/order {tab_id, ids:[]}`
- `POST /uploads` (multipart) → `{url}` (local disk under `./uploads`, served at `/uploads/*`; S3 later)
- `GET /analytics/summary?range=7d|30d|90d` → `{views, clicks, ctr, series:[{date,views,clicks}], top_links:[{block_id,title,clicks}], top_referrers:[{referrer,count}]}`
- `GET /contact-submissions?cursor=`

Public (no auth)
- `GET /public/{username}` → `{profile, tabs, blocks}` (published pages, visible blocks only; 404 otherwise)
- `POST /public/{username}/events {kind, block_id?}` (the server derives referrer, device and country from headers)
- `POST /public/{username}/contact {block_id,name,email,message}` (rate limited)

### Contract details (added by FE, v0.1.1)

Shapes the list above left open; the FE types in `tautan-fe/src/lib/types.ts` follow these:
- `seo` jsonb: `{title?, description?, image_url?}`
- `GET /contact-submissions?cursor=` → `{items: ContactSubmission[], next_cursor: string|null}` (opaque cursor, 20 per page, newest first)
- `POST /tabs {title, slug?}` (the server slugifies the title if no slug is given) → `Tab`; `PATCH /tabs/{id} {title?, slug?}` → `Tab`. Deleting the last tab → 400 `last_tab`. Deleting a tab deletes its blocks.
- `PATCH /blocks/{id} {data?, is_visible?, tab_id?, parent_id?}` → `Block`. Moving a folder to another tab moves its children too. Deleting a folder deletes its children.
- `POST /blocks` → `Block` (position = end of its siblings). `PATCH /profile` → `Profile`.
- `PUT /blocks/order` also reorders folder children: send the child ids with the folder's `tab_id`.
- Username rule: `^[a-z0-9_.]{3,30}$`, stored lowercase. Reserved names (`admin, dashboard, login, signup, api, settings, uploads`) are unavailable.
- `POST /uploads`: the multipart field name is `file`. Relative `url`s (`/uploads/x.png`) resolve against `NEXT_PUBLIC_API_URL`.
- CORS: allow origin `http://localhost:3000` with credentials (the refresh cookie is sent cross-origin from :3000 to :8080).

## Tech choices

- **BE**: Go 1.23+, chi router, pgx plus sqlc (or plain pgx), golang-migrate migrations,
  bcrypt, golang-jwt. Config from env. Tests use `go test`. Go is **not installed** on this
  machine; install it to `~/.local/go` from go.dev/dl (no sudo). Docker and Postgres are not
  installed either; use `fergusstrange/embedded-postgres` for local dev and tests, or
  install Postgres binaries without sudo. Include a Dockerfile and docker-compose for deploys.
  Runs on `:8080`.
- **FE**: Next.js 15 App Router, TypeScript, Tailwind, the dnd-kit drag-and-drop library,
  TanStack Query, zod. Runs on `:3000`.
  `NEXT_PUBLIC_API_URL=http://localhost:8080`. The public page `/[username]` is server
  rendered. Load the `design-taste-frontend` skill for the landing page and visual
  polish.

## Working agreement (multi-session)

- Each session owns the area it was assigned. Don't edit another session's area without
  messaging its owner first.
- Work on feature branches, push them, and open PRs to `main` with `gh pr create`. Small,
  frequent PRs. Merge your own PR once it builds and its tests pass.
- If you need to change the API contract, edit this SPEC.md and message both other sessions.
- When you finish a milestone, message the orchestrator `tautan-d9` with what you shipped, the PR
  links, and anything blocked.

## Phase 2: parity with liinks.co (from the research in tautan-fe PR #9, `docs/RESEARCH.md`)

Every item below adds to MVP. Keep existing block types and fields working (only additive changes to `data`/`theme`).
The full detail is in RESEARCH.md; section numbers are given in brackets. Priority order:

**P2-1 Richer blocks** [§3.3, Gaps A4–A7, C]
- link: `description`, `layout` (button|thumbnail|thumbnail_highlight|grid2|grid3|image_background|carousel), `icon` (emoji or image url), `label` badge, `text_align`, `dest_kind` (url|email|file|tab), `schedule {start_at?, end_at?}` (enforced by the server on public reads)
- image: `full_bleed`, `lightbox`. folder: `style` collapsible|inline. faq/testimonial: optional `heading`/`description`; testimonial items `role`, `url`
- New types: `heading`, `divider`, `file` (upload, download), `discount_code` {code, description} (counts copies via the click event), `countdown` {ends_at, label}, `booking` {url} (Calendly/Cal.com embed), `quote`, `email_signup` (one field, goes into Audience)
- Soft delete: `blocks.deleted_at`, `GET /blocks/deleted`, `POST /blocks/{id}/restore`
**P2-2 Theme v2** [§4, Gaps A8] (stays in `profile.theme` jsonb): `header {layout: classic|banner|headshot, banner_image_url?}`, `fonts {title, body}`, separate `block`/`button`/`label` colours, `border {width, color}`, `shadow {amount, style: soft|solid}`, `spacing`, `bg_desktop?`. Plus `profile.socials [{platform,url}]` and `socials_position` top|bottom (a header socials row separate from blocks).
**P2-3 Forms + Audience** [§3.4, §7] form block (`form`): typed fields (text, email, phone, textarea, select, checkbox, date, number), required flags, success message. Submissions are stored with JSON answers. Audience = contacts deduplicated by email across forms and email_signup, with source and timestamps. CSV export for both. `contact_form` remains as a preset of `form`.
**P2-4 Analytics v2** [§6]: unique visitors (daily hashed IP+UA salt), period-over-period %, custom from/to, hour/day/week buckets, breakdowns by country/device/browser/OS/utm_source/channel, a per-block series, CSV export.
**P2-5 Smart paste** [Gaps B]: `POST /unfurl {url}` → `{title, description, image_url, suggested_type}` (OG scrape with SSRF protection: block private IPs, 3 s timeout, size cap). Media URLs → video/music, emails → mailto link.
**P2-6 Onboarding** [§0.1]: after signup: start (import URL | from scratch with an intent: local | sell | audience | other) → profile → starter sections → theme → claim URL (username claimed last; sign-up gets a temporary username). `POST /import {url}` imports links, avatar and bio from Linktree/Beacons/any public link page (scrape anchors plus OG).
**P2-7 Page extras** [§3.7, §9]: announcement (banner|popup, CTA, schedule), GA4 and Meta Pixel IDs in settings, a share button plus vCard save-contact, page language, a username change that keeps a 301 redirect from the old name.
**P2-8 Google sign-in** [Gaps A12]: needs OAuth client credentials from the user; build behind a config flag.

Later (not now): gates, custom domains, multiple profiles/teams, billing, public API + MCP, QR studio, short links, AI features, print studio.

### Phase 2 API: implemented shapes (BE)

All changes are additive: new optional fields, new types, new endpoints. MVP shapes are unchanged.

#### P2-1 Richer blocks
Any block's `data` may carry `schedule {start_at?, end_at?}` (RFC 3339). Public reads drop blocks outside their window (`start_at <= now < end_at`), plus the children of a dropped folder. The editor shows everything.
- link `+ {description? ≤300, layout? button|thumbnail|thumbnail_highlight|grid2|grid3|image_background|carousel, icon? (emoji ≤8 chars or image URL), label? ≤24, text_align? left|center|right, dest_kind? url|email|file|tab (default url), tab_id? (required when dest_kind=tab; url may then be ""), schedule?}`. With `dest_kind: email`, `url` may be a bare address; it is stored as `mailto:…`.
- image `+ {full_bleed?, lightbox?}` · folder `+ {style? collapsible|inline}` · faq `+ {heading?, description?}` · testimonial `+ {heading?, description?}`, items `+ {role?, url?}`
- New types:
  - `heading {title, text_align?}`
  - `divider {style? solid|dashed|dotted|double|none}`
  - `file {title, url, file_name?, size?}`
  - `discount_code {code ≤40 ("" = no code needed), description?, brand?, discount?, url?, button_label?}`: a copy is reported as a `click` event on the block
  - `countdown {ends_at (RFC 3339), label?, description?, ended_message?, hide_after_end?}`
  - `booking {url}`: https on calendly.com, cal.com or app.cal.com only
  - `quote {quote, name?, role?, heading?}`
  - `email_signup {title?, description?, placeholder?, button_label?, success_message?}`: submissions go to Audience (P2-3)
- `POST /uploads?kind=file`: also accepts PDF, ZIP, MP3, MP4 and plain text, ≤25 MB, served as attachments. Images stay ≤5 MB. The response now also has `file_name`, `size` and `content_type`, alongside `url`.
- Soft delete: `DELETE /blocks/{id}` sets `deleted_at` (a folder's live children are deleted with it). Deleted blocks disappear from the editor, public reads, ordering and events.
  - `GET /blocks/deleted` → `{blocks: Block[]}`, newest first, each with `deleted_at`. Children deleted together with their folder aren't listed separately. Blocks are purged 30 days after deletion.
  - `POST /blocks/{id}/restore` → `Block`: back at the end of its group. A folder brings back the children deleted with it. A child whose folder is gone or still deleted returns to the top level.

#### P2-2 Theme v2 + header socials
`PATCH /profile {theme}` validates the keys it knows. Unknown keys are stored untouched, so new FE fields never break a save. Every key is optional.
- `preset` (`[a-z0-9_-]{1,40}`), `text_color`, `font`, `bg_desktop` (colour), `spacing` (0–64)
- `bg {type: color|gradient|image|split|none, value}`: a colour for `color`; `linear|radial|conic-gradient(...)` (no `url(`, `;` or braces) for `gradient` and `split`; a URL for `image`
- `button {style: fill|outline|shadow, radius 0–100, color, text_color}` · `block {color, text_color}` · `label {color, text_color}`
- `border {width 0–12, color}` · `shadow {amount 0–100, style: soft|solid}` · `fonts {title, body}` (font family names)
- `header {layout: classic|banner|headshot|business, banner_image_url?}`
- Colours: `#rgb[a]`, `#rrggbb[aa]`, `rgb()/rgba()/hsl()/hsla()` or a CSS colour name. Error code: 400 `invalid_theme`.
- **Multi-colour themes (tautan-be #28):** `palette? {surfaces, accents, ink, ink_on_accent}` and `palette_mode?: cycle|by_type|mono`.
  - `surfaces`: 4–6 colours (card backgrounds, used in turn by `cycle`, per card type by `by_type`, or just the first by `mono`). `accents`: 1–2 colours; `accents[0]` is primary buttons, `accents[1]` is reserved for highlights. `ink` and `ink_on_accent` are both required.
  - Palette colours are stricter than the rest of the theme: hex (`#rgb[a]`, `#rrggbb[aa]`) or `rgb()/rgba()` only, with no hsl and no names.
  - `palette: null` clears it, falling back to the single-colour theme. `palette_mode` without a palette is accepted and ignored.
  - As with the rest of the theme, unknown keys inside `palette` are stored untouched.

`Profile` gains `socials: [{platform, url}]` (default `[]`) and `socials_position: "top"|"bottom"` (default `top`). Both are patchable and returned by `/me`, the editor and `/public/{username}`.
- `platform`: lowercase `[a-z0-9_]{1,30}` (e.g. instagram, tiktok, x, youtube, email). `"divider"` is a spacer, and its url is ignored.
- `url` must be http(s), mailto or tel, and the FE builds it from a handle. At most 30 items.
- Errors: `invalid_socials`, `invalid_socials_position`.

#### P2-3 Forms + Audience
- Error envelope addition: validation errors may carry `error.details: {field_id: message}`.
- New block type `form`: `{title?, description?, button_label?, success_message?, hide_labels?, create_contacts? (default true), subscribe?: everyone|ask|none (default none), fields: FormField[] (1–30), schedule?}`.
  - `FormField {id: [a-z0-9_-]{1,40} unique, type: text|email|phone|textarea|select|checkbox|date|number, label, placeholder?, required?, options?: string[] (select only, 1–50)}`.
  - `contact_form` keeps working unchanged. The FE can offer it as a form preset.
- `POST /public/{username}/form {block_id, answers: {field_id: value}, subscribe?: bool}` → 201 `{id, success_message}`.
  - Values by type: text ≤500, textarea ≤5000, email, phone, number (number or numeric string), date `YYYY-MM-DD`, select (one of the options), checkbox (bool; a required checkbox must be true). Unknown keys are dropped.
  - 400 `invalid_answers` with `details`; 400 `invalid_block` (not a visible, live, in-schedule form on that page); 429 `rate_limited` (10 per 10 min per IP).
  - Answers are stored as `[{field_id, label, type, value}]`, with label and type snapshotted.
- `POST /public/{username}/subscribe {block_id, email, name?}` (an `email_signup` block) → 201 `{success_message}`. The contact is always subscribed.
- Audience contacts are deduplicated per page by email (case-insensitive) and created from:
  - forms (when `create_contacts` is on and an email answer exists; subscribed when `subscribe=everyone`, or `ask` plus `subscribe:true`; name from the field with id `name` or a text field labelled "name")
  - `email_signup` (subscribed)
  - `contact_form` (not subscribed)
  
  Subscription is sticky.
- Owner endpoints:
  - `GET /forms` → `{items: [{block_id, type: form|email_signup, title, tab_id, is_visible, responses, last_response_at}]}`
  - `GET /forms/{block_id}/submissions?cursor=` → `{items: [{id, block_id, contact_id, answers, created_at}], next_cursor}` (20 per page, newest first)
  - `GET /forms/{block_id}/submissions.csv`: "Submitted at", then one column per current field label, then fields that only appear in older answers
  - `GET /audience?status=all|subscribed|unsubscribed&q=&cursor=` → `{items: Contact[], next_cursor, counts: {all, subscribed, unsubscribed}}`, ordered by last activity
    - `Contact {id, email, name, subscribed, subscribed_at, sources: [{block_id, type, title}], responses, first_seen_at, last_activity_at}`
  - `GET /audience/{id}` → `{contact, activity: [{kind: form|contact, block_id, answers?, message?, created_at}]}` (latest 50)
  - `GET /audience.csv?status=&q=` and `GET /contact-submissions.csv`
  - CSV cells starting with `= + - @` (or tab/CR) are prefixed with `'` against formula injection.

#### P2-4 Analytics v2
Event capture: `POST /public/{username}/events` also accepts `page_url?` (send `location.href`; `utm_source/medium/campaign` are read from it) and explicit `utm_source?`, `utm_medium?`, `utm_campaign?` (these win). The server derives:
- browser (Chrome, Safari, Firefox, Edge, Opera, Samsung Internet, Instagram, Facebook, TikTok, Other) and OS (iOS, Android, Windows, macOS, Linux, ChromeOS, Other)
- the referrer host, with `l.`/`m.`/`www.` folded (l.instagram.com → instagram.com)
- channel: social, search, email, qr, paid, referral or direct
- a visitor hash (sha256 over a random daily salt + IP + UA, never linkable across days)

Every v2 endpoint takes these common query params:
- `range`: today|yesterday|7d|30d (default)|90d|month (to date)|year (to date)|custom with `from`/`to` (`YYYY-MM-DD`, inclusive, ≤2 years)
- `tz`: IANA zone, default UTC. Day boundaries and buckets use it.
- `interval`: hour (≤7 days)|day (≤1 year)|week (weeks start Monday). Default: hour ≤2 days, day ≤93 days, else week.
- Filters (cross-filtering): any of `country, device, browser, os, utm_source, utm_medium, utm_campaign, channel, referrer` = exact value. `referrer=` (empty) means direct.

Endpoints:
- `GET /analytics/overview` → `{range: {from, to, tz, interval}, totals, previous, change, series, top_link}`
  - `totals` and `previous` are `{visitors, views, clicks, ctr, engagement}`. `previous` is the period of equal length just before. `ctr` = clicks/views and `engagement` = share of visitors who clicked, both 0..1.
  - `change`: % change per totals key (1 decimal), `null` when the previous value is 0
  - `series: [{t, views, clicks, visitors}]`, zero-filled. `t` is `YYYY-MM-DDTHH:00` for hour and `YYYY-MM-DD` (bucket start) for day/week.
  - `top_link: {block_id, title, clicks} | null`
- `GET /analytics/breakdown?dimension=<one of the filter names>&limit=20 (≤100)` → `{range, dimension, items: [{value, views, clicks, visitors}]}`, sorted by views
- `GET /analytics/blocks` → `{range, items: [{block_id, type, title, clicks, is_visible, deleted, created_at}]}`: every block, including hidden and deleted ones (toggle them in the UI), sorted by clicks
- `GET /analytics/blocks/{id}/series` → `{range, block_id, clicks, series: [{t, clicks}]}`
- `GET /analytics/export.csv?type=series (default)|breakdown (&dimension=)|blocks`

`GET /analytics/summary` (MVP) is unchanged. Visitors are counted only for events recorded after this release.

#### P2-5 Smart paste
`POST /unfurl {url}` (owner) → `{url, suggested_type, dest_kind?, fetched, title, description, image_url, site_name}`
- An email (or `mailto:`) → `{url: "mailto:…", suggested_type: "link", dest_kind: "email", title: <email>}`, with no fetch.
- Scheme-less input gets `https://`. `suggested_type` from the URL:
  - `video`: youtube, youtu.be, vimeo, tiktok, twitch, dailymotion
  - `music`: spotify, soundcloud, apple music, bandcamp, deezer, tidal, audiomack
  - `booking`: calendly, cal.com
  - `image`: .png/.jpg/.gif/.webp, or an `image/*` response
  - otherwise `link`
- Metadata comes from OpenGraph, then Twitter tags, then `<title>`/description. `image_url` is resolved to absolute http(s).
- Unreachable or slow sites still return 200 with `fetched: false`.
- Errors: 400 `invalid_url`; 400 `blocked_url` (private, loopback or link-local address, non-default port, credentials in the URL, or a redirect to one of those); 429 `rate_limited` (30/min per user).
- Fetch rules (shared with `/import`): http/https only on ports 80/443; the resolved IP is checked at dial time (so DNS rebinding and redirects are covered); ≤3 redirects; 3 s total; 1 MB body cap; no proxies; UA `TautanBot/1.0`.

#### P2-6 Onboarding + import
- `Profile.onboarding`: `{completed, completed_at?, step?, intent?: local|sell|audience|other, source?: import|scratch|instagram, username_claimed, suggested_username?}`.
  - Accounts that existed before this release have `completed: true`.
  - New sign-ups start with `completed: false`.
- `POST /auth/signup`: `username` is now **optional**.
  - Without it the account gets a temporary username `tmp.<10 chars>`, an empty display_name, and `onboarding: {username_claimed: false, suggested_username}`.
  - `suggested_username` is an available name derived from the email local part, e.g. `jane.doe+x@…` → `jane.doe.x`.
  - Names starting with `tmp.` can never be claimed.
- `PATCH /profile`:
  - `{onboarding: {step?, intent?, source?, completed?}}` merges into the stored object. `completed: true` stamps `completed_at`.
  - `username_claimed` and `suggested_username` are server-managed: claiming a username sets `username_claimed: true` and drops the suggestion.
  - Publishing a page while its username is unclaimed → 400 `username_not_claimed`. Claiming and publishing in the same PATCH is allowed.
  - Error code: `invalid_onboarding`.
- `POST /import {url, apply?: false, tab_id?}` scrapes a public link page (Linktree's embedded data, any page's OG tags and anchors) through the same SSRF-safe fetcher as `/unfurl` (2 MB cap).
  - → `{preview: {source_url, profile: {display_name, bio, avatar_url}, links: [{title, url}] (≤50), socials: [{platform, url}] (≤30)}}`
  - The page's own navigation, legal links and non-http links are skipped. Social profile URLs become socials; posts and videos stay links.
  - With `apply: true` it also returns `{applied: {links, socials, display_name, bio, avatar}, profile}`:
    - display_name is filled when empty or equal to the username; bio and avatar only when empty. The avatar is downloaded and re-hosted under `/uploads`.
    - socials are set only if the profile has none.
    - links become link blocks appended to `tab_id` (default: the first tab).
    - `onboarding.source` becomes `import`.
  - Errors: `invalid_url`, `blocked_url`, 502 `fetch_failed`, `invalid_tab`, 429 `rate_limited`.

#### P2-7 Page extras
`Profile` gains `announcement` (object or `null`) and `settings` (object, default `{}`). Both are patchable and returned everywhere a Profile is.
- `announcement: {id, layout: banner|popup|card (default banner), title ≤100, description? ≤204, image_url?, cta?: {label ≤40, url | form_block_id}, bg_color?, text_color?, schedule?}`.
  - `id` is server-assigned and stays the same across edits, so dismissals stick. It changes only after `{announcement: null}` and a re-create. Key per-device dismissals on it.
  - Public reads return `null` outside the schedule window.
  - Error code: `invalid_announcement`.
- `settings: {ga4_id? (G-XXXX), meta_pixel_id? (digits), language? (id, en, pt-BR…), show_share?, show_save_contact?, contact_email?, contact_phone?}`.
  `show_share` and `show_save_contact` are stored exactly as sent: an explicit `false` is kept and returned in editor and public reads, and a missing key stays missing (tautan-be #27). Share is ON by default, so the FE saves `show_share` explicitly and treats a missing key as on.
  - The whole object is replaced on PATCH. Unknown keys are dropped, since these values end up in scripts.
  - Error code: `invalid_settings`.
- Username change: the old name keeps working as a redirect to the profile.
  - `GET /public/{old}` → **301** with `Location: /api/v1/public/{new}` and body `{error: {code: "moved"}, redirect_to: "new"}`. The SSR page should issue its own 301 to `/{new}`.
  - Retired names can't be taken by other accounts (username-available reports false; signup or PATCH → 409 `username_taken`), but the owner can reclaim them.
  - At most 5 redirects are kept per profile, and temporary `tmp.` names leave none. An unpublished target → 404.
- `GET /public/{username}/vcard` → `text/vcard` (vCard 3.0, `{username}.vcf`). Contents: FN/N from display_name, NOTE from bio, URL `APP_BASE_URL/{username}`, EMAIL/TEL from settings, PHOTO (a local avatar is embedded), and X-SOCIALPROFILE per social. Published pages only.
- Share button and page language are FE concerns driven by `settings`.

#### P2-8 Google sign-in
OIDC authorization code flow with PKCE (S256), state and nonce. It is off unless the BE has `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET` and `GOOGLE_REDIRECT_URL`; setup steps are in the tautan-be README.
- `GET /auth/providers` → `{google: bool}`. The FE shows "Continue with Google" only when `google` is true.
- `GET /auth/google/start` is a **browser navigation** (`window.location`), not a fetch.
  - → 302 to Google, and sets a 10-minute httpOnly cookie `tautan_oauth` (path `/api/v1/auth/google`) holding state, nonce and the PKCE verifier.
  - Disabled → 404 `provider_disabled`. Google unreachable → 302 to `GOOGLE_REDIRECT_URL?error=provider_error`.
- Google redirects to `GOOGLE_REDIRECT_URL`, the **FE** page `/auth/google/callback?code&state` (or `?error=access_denied` etc. when the user cancels; the FE handles that without calling the BE).
- `POST /auth/google/callback {code, state}` with `credentials: "include"` → the `/auth/login` shape plus `is_new_user`: `{access_token, user, profile, is_new_user}`, and sets the refresh cookie.
  - 200 for an existing account, 201 for a new one. The state cookie is single-use and is cleared on every call.
  - Account resolution: an existing Google link (by OIDC `sub`) → that user. Otherwise an account whose email equals Google's **verified** email gets linked. Otherwise a new account is created with no password: temporary username, `onboarding: {completed: false, username_claimed: false, suggested_username}`, exactly like signup without a username. Route it through onboarding as usual.
  - **Pre-hijack protection:** the first Google link to an existing account with no identity and an unconfirmed email (see "Email verification") clears its password, revokes every refresh token, and rejects access tokens issued before the link (`users.sessions_revoked_at`). The response then has `password_removed: true`. Later Google sign-ins, and links from a second Google account, leave everything alone.
  - A Google-only account trying password login → 401 `password_not_set`.
- `GET /auth/google/start?u=<username>` (optional): the name typed on the landing page (`/signup?u=`). If a *new* account is created and the name is valid and free, it becomes `onboarding.suggested_username`.
- `GET /me` → `user` also carries `has_password: bool` and `identities: string[]` (e.g. `["google"]`). Only /me returns these, not the auth responses.
- `PATCH /me/password {new_password}` (Bearer) → 204. Only allowed while the account has no password: otherwise 409 `password_already_set`. 400 `invalid_password` (8–72 chars).
  - Errors:
    - 400 `invalid_state`: cookie missing, expired or mismatched; restart the flow.
    - 400 `invalid_code`: missing, used or expired code.
    - 403 `email_not_verified`
    - 502 `provider_error`: token exchange or ID token verification failed.
    - 404 `provider_disabled`

#### Email and password reset (BE: Mailer + outbox)
- Email is queued in `email_outbox` and sent by a worker through `MAIL_DRIVER=log|smtp` (see the tautan-be README). In dev, links show up in the API log.
- `POST /auth/password/forgot {email}` → **202** `{accepted: true}`, whether or not the account exists.
  - Sends a reset link `APP_BASE_URL/reset-password?token=…` (single-use, valid 30 minutes).
  - At most 3 emails per account per hour; extra requests still get 202. 429 `rate_limited` per IP.
- `POST /auth/password/reset {token, new_password}` → the `/auth/login` shape plus the refresh cookie, so the user is signed in.
  - Errors: 400 `invalid_token` (unknown, used or expired), 400 `invalid_password` (8–72 chars), 429 `rate_limited`.
  - On success, every other session ends (refresh tokens revoked, older access tokens rejected) and other outstanding reset links die.
  - Works for Google-only accounts too, giving them a password.

#### Email verification (blocks publishing; user decision 2026-11-18)
- `users.email_verified_at`. These count as confirming the address:
  - the emailed link (sent at signup, valid 72 h, single-use; using one kills the others)
  - Google sign-in, both new and linked accounts (backfilled for existing links)
  - a completed password reset
- `GET /me` → `user.email_verified: bool`.
- `PATCH /profile {is_published: true}` on an **unpublished** page by an unverified account → **403 `email_unverified`**. Everything else (editing, preview, unpublishing) works.
  - **Grandfathering:** pages already published when this shipped stay published. Re-sending `is_published: true` for a live page is fine. Unpublishing and then publishing again needs a confirmed email. `email_verified_at` is not backfilled, so nobody gets marked verified who wasn't.
  - Check order with claim + publish in one PATCH: `username_not_claimed` (400) first, then `email_unverified` (403).
- `POST /auth/verify-email {token}` (no auth, so the link works on any device) → `{verified: true}`; 400 `invalid_token`; 429 `rate_limited`.
- `POST /me/verification-email` (Bearer) → 202 `{sent: true}`, or 200 `{already_verified: true}`. 429 `rate_limited` after 3 per account per hour; the signup email counts.
- The Google pre-hijack rule (P2-8) now only wipes the password when the account has no identity **and** `email_verified_at` is null.

#### Owner notifications (user decision 2026-11-18: on by default, instant)
- A new contact-form message or form response emails the page owner through the outbox. The email shows the fields, has a link to `/dashboard/inbox` or `/dashboard/forms` and a link to notification settings, and sets **Reply-To** to the visitor's email when there is one.
- Only sent to a **confirmed** owner address, because the email carries visitors' personal data.
- At most **20 per owner per hour**. Beyond that, submissions are still stored but not emailed.
- `email_signup` subscriptions don't notify.
- `GET /me` → `user.notify_submissions: bool` (default true).
- `PATCH /me/notifications {submissions: bool}` → `{submissions}`; 400 `invalid_notifications`.

#### Changing the account email (P1 email item 1)
- `POST /me/email {new_email, password}` (Bearer) → 202 `{pending_email}`. Nothing changes until the link sent to the **new** address (`/confirm-email-change?token=`, 24 h, single use) is opened. Accounts with a password must give it (403 `invalid_password`), so a stolen session alone can't move the account. Google-only accounts have no password to give; for them the safeguard is the old-address notice and its undo link. Errors: 400 `invalid_email` / `same_email`, 409 `email_taken`, 429 `rate_limited` after 3 requests per account per hour. A new request replaces the previous link.
- `GET /me` → `user.pending_email: string | null`. `DELETE /me/email/pending` → 204 cancels it.
- `POST /auth/email-change/confirm {token}` (no auth, so it works on any device) → the `/auth/login` shape. Switches the email, marks it verified, signs out every session and refresh token, then emails the **old** address. That email shows the new address masked (`n***@example.com`) and an undo link (`/revert-email-change?token=`, 7 days). 400 `invalid_token` if the link is used, expired, cancelled or replaced, or if the account no longer has the email the link was made for. 409 `email_taken` if someone signed up with the address in the meantime.
- `POST /auth/email-change/revert {token}` → the `/auth/login` shape. Puts the old email back only if the account still has the changed-to address, signs out every session and cancels any pending change. The password stays as it is; the FE then points the user to reset it.
- Google sign-in links by the Google `sub`, so changing the email doesn't unlink Google.

## Phase 3: card catalog beyond link-in-bio (approved 2026-11-16; build order P3-A Sell, P3-B, P3-C/D)

Goal: Tautan is not "another Linktree". A page can sell, take orders, pitch brands and run an audience.
In the UI, blocks are called **cards**. Today there are 19 types: link, text, image, video, music,
social_icons, folder, faq, testimonial, contact_form, heading, divider, file, discount_code, countdown,
booking, quote, email_signup, form. This section adds 29 types in four groups, plus card-level gates.
Every new type is a new `blocks.type` with its own `data` shape. Every change stays additive. Each card
fires the existing click event (with a `meta.action` such as `buy`, `copy`, `vote`, `wa_order`) so Insights covers it.
Brand marks (Shopee, Tokopedia, WhatsApp …) come from simple-icons, which is already a dependency. Never draw look-alikes.

### P3-A Sell: small sellers (no payment processing needed)
| type | what it does | data |
|---|---|---|
| `product` | One product: photos (≤8, swipe), name, price, compare-at price, badge (New, Best seller, Pre-order, Sold out), variants (size/colour chips), and a buy action: `url` \| `whatsapp` \| `marketplace` | `{images[], name, price, currency, compare_at?, badge?, variants?:[{name, options[]}], stock_note?, action:{kind, url?, wa_number?, wa_template?, marketplace?}}` |
| `shop` | A grid of products (2 cols on phone, 3 on desktop) with category chips; opens the product sheet | `{title?, categories?[], items:[product data]}` |
| `marketplace` | A "Buy on" row with the real marks for Shopee, Tokopedia, TikTok Shop, Lazada, Blibli, Bukalapak, Etsy, Amazon, Gumroad | `{title?, stores:[{platform, url}]}` |
| `whatsapp` | A chat or order button; optional mini order form (items, qty, name, address) composed into a prefilled `wa.me` message | `{label, number, message_template, order_form?:{fields[]}}` |
| `price_list` | A services or menu price list in sections, with optional photos and an "ask" button | `{sections:[{title, items:[{name, desc?, price, image_url?}]}], currency, note?}` |
| `store_info` | Hours with an "Open now" badge (visitor tz vs store tz), street and area, map search link, delivery couriers and areas | `{hours:[{day, open, close}], tz, address?, area?, couriers?[], delivery_note?}` |
| `payment` | QRIS image or bank/e-wallet accounts with copy buttons and a "confirm payment" WhatsApp/form link | `{qris_image_url?, accounts:[{bank, name, number}], note?, confirm?:{kind, target}}` |
| `tip_jar` | "Support me": suggested amounts and platform links (Saweria, Trakteer, Ko-fi, PayPal, Buy Me a Coffee) | `{title, amounts?[], platforms:[{platform, url}]}` |
| `reviews` | A rating summary (average, count, star bars) plus review quotes with stars and an optional source link (Google, Shopee) | `{average?, count?, distribution?[5], items:[{stars, quote, author, source_url?}]}` |

#### P3-A data contract (proposed by FE, 2026-11-16; zod in tautan-fe `src/lib/types.ts`, branch `feat/p3-types`)
Common rules:
- **Money** is an integer in minor units of `currency` (ISO 4217; `IDR` | `USD` | `SGD` | `MYR`, default `IDR`). Rp 85.000 → `8500000`.
- **WhatsApp numbers** are E.164 with the plus: `^\+[1-9][0-9]{7,14}$`.
- **URLs** follow the existing `checkURL` rules.
- Every type takes the shared `schedule?`.

| type | shape and limits |
|---|---|
| `product` | `images[]` 0–8 URLs (first = cover), `name` 1–100, `description?` ≤1000, `price?`, `currency?`, `compare_at?` (> price), `badge?` new\|best_seller\|pre_order\|sold_out, `variants?` 0–3 × `{name ≤30, options[] 1–20 × ≤30}`, `stock_note?` ≤60, `action {kind: url\|whatsapp\|marketplace, url? (url/marketplace), wa_number? (E.164), wa_template? ≤500 with {name} {variant} {price}, marketplace? platform, label? ≤40}` |
| `shop` | `title?` ≤100, `categories?` 0–12 × ≤30, `items` 1–60 × (product fields without schedule, plus `id` `[a-z0-9_-]{1,40}` unique, `category?` ∈ categories) |
| `marketplace` | `title?` ≤60, `stores` 1–9 × `{platform, url}`, platform unique ∈ shopee, tokopedia, tiktok_shop, lazada, blibli, bukalapak, etsy, amazon, gumroad |
| `whatsapp` | `label` 1–40, `number` E.164, `message_template` 1–1000 with {items} {name} {address} {note} {total}, `order_form? {fields[] ⊂ items\|name\|address\|note (unique, ≥1), items? 1–30 × {name ≤80, price?} (required when fields has "items"), currency?}` |
| `price_list` | `sections` 1–10 × `{title ≤60, items 1–40 × {name 1–80, desc? ≤200, price?, price_from? bool, image_url?}}`, `currency?`, `note?` ≤300, `ask? {label ≤40, kind: whatsapp\|url, target}` |
| `store_info` | `hours` 0–21 × `{day: mon..sun, open "HH:MM", close "HH:MM"}` (close < open = past midnight; "24:00" allowed as close; unlisted days are closed), `tz` IANA, `address?` ≤200, `area?` ≤100, `couriers?` ⊂ gosend, grabexpress, jne, jnt, sicepat, anteraja, pos, shopee_express, lalamove, paxel, pickup, `delivery_note?` ≤300 |
| `payment` | `title?` ≤60, `qris_image_url?`, `accounts` 0–6 × `{bank ≤40, name ≤60, number ≤40}` (needs QRIS or ≥1 account), `note?` ≤300, `confirm? {kind: whatsapp\|url\|form, target (E.164 \| URL \| form block id), label? ≤40}` |
| `tip_jar` | `title` 1–100, `description?` ≤300, `amounts?` 0–4, `currency?`, `platforms` 1–5 × `{platform: saweria\|trakteer\|kofi\|paypal\|buymeacoffee, url}` |
| `reviews` | `title?` ≤60, `average?` 0–5, `count?` ≥0, `distribution?` exactly 5 counts (5★ → 1★), `source?` ≤40, `items` 0–20 × `{stars 1–5, quote 1–500, author 1–60, source_url?}` (needs average or ≥1 item) |

#### P3-A: implemented (BE, tautan-be PR #13)
The backend validates the FE contract above exactly (same limits, enums and required fields), with these settled details:
- **URLs are http(s) only.** Buy/store/platform/source/confirm/ask links reject `mailto:`, `tel:` and `javascript:`. Image fields (`images[]`, `image_url`, `qris_image_url`) also accept `/uploads/...` paths.
- **Normalisation on save** (the response returns the stored data): an empty `currency` becomes `"IDR"`; WhatsApp numbers lose spaces, dashes, dots and brackets (`+62 812-3456-7890` → `+6281234567890`), then must match `^\+[1-9][0-9]{7,14}$`; `action` fields that don't belong to `action.kind` are dropped; shop items lose `schedule`; a standalone `product` loses `id` and `category`.
- `price`, `compare_at` and `price_list` item prices are optional integers in minor units, 0 to 1e14. `compare_at` needs `price` and must be greater. `tip_jar.amounts[]` must be > 0.
- `price_list.ask` and `payment.confirm` share one shape: `{kind, target, label?}`. `target` is E.164 for whatsapp, http(s) for url, and a block uuid for form.
- `whatsapp.order_form.items` is required (1–30) when `fields` includes `items`.
- `store_info.tz` must be a real IANA zone. `open` can't be `24:00`. `close` must differ from `open`. Couriers are unique ids from the list.
- **Validation errors:** `400 {error:{code:"invalid_data", message, details:{"items.1.price": message}}}`. `details` has a single key, the dotted path of the first failing field. This applies to every block type, including P1 and P2 ones.

**Click actions.** `POST /public/{u}/events` accepts `meta: {action}` on click events only. The pattern is `^[a-z][a-z_]{0,23}(:[a-z0-9_]{1,32})?$`, e.g. `buy`, `wa_order`, `copy_account`, `copy`, `marketplace:shopee`, `tip:saweria`. Anything else, or meta on a view, returns `400 invalid_action`. It is stored in `events.action`.
- `GET /analytics/blocks` items gain `actions: {"buy": 2, "marketplace:shopee": 1}`, which is `{}` when a block has none. `title` falls back to `data.name` for products.
- `GET /analytics/blocks/{id}/series?action=buy` narrows the series to one action.

**Smart paste.** `POST /unfurl` adds `kind: "marketplace"` and `platform` (shopee | tokopedia | tiktok_shop | lazada | blibli | bukalapak | etsy | amazon | gumroad), detected from the host, including short links such as shp.ee, tk.tokopedia.com, s.lazada.*, amzn.to and gum.co.
- `suggested_type` is `"marketplace"`, or `"product"` when the page is `og:type=product` or exposes a price.
- When the page has `product:price:amount`/`og:price:amount` with a supported currency, the response adds `price` (minor units) and `currency`. `title` and `image_url` come from OG as before.
- This is best effort: many marketplace pages render with JS or block bots, so the platform is always set, but price and title may be missing.

### P3-B Creators and influencers
| type | what it does | data |
|---|---|---|
| `media_kit` | Per-platform stats (followers, avg views, engagement %), audience split (age, gender, top cities), "updated" date and a PDF download | `{platforms:[{platform, handle, followers, avg_views?, engagement?}], audience?:{age[], gender[], cities[]}, updated_at, pdf_url?}` |
| `rate_card` | Collab packages with price "from", deliverables and turnaround; the button opens a collab request form (an existing form under the hood) | `{packages:[{name, price_from?, deliverables[], turnaround?}], currency, form_block_id?}` |
| `brands` | A "Worked with" logo wall (uploaded logos, optional links) | `{title?, logos:[{image_url, name, url?}]}` |
| `post_grid` | Pinned Instagram/TikTok/YouTube posts as a 3-col thumbnail grid (oEmbed by URL, so no account connection is needed) | `{posts:[{url, platform, thumb_url, caption?}]}` |
| `latest_video` | The latest YouTube upload from a channel (RSS, refreshed hourly) | `{channel_id, show:{count:1..3}}` |
| `shop_the_look` | A photo with tappable hotspots, each opening a product or affiliate link with a price | `{image_url, spots:[{x, y, title, price?, url, code?}]}` |
| `gallery` | An image carousel or grid with a lightbox | `{images:[{url, alt, link_url?}], layout: carousel\|grid}` |
| `events` | Tour, workshop and event dates (date, city, venue, ticket link, Sold out / Few left), or a **live schedule** with weekly recurrence ("Live on TikTok every Fri 20:00") and "Live now" during the slot | `{mode: dates\|weekly, items:[...] , tz}` |
| `podcast` | Latest episodes from an RSS feed with an inline player | `{feed_url, count}` |
| `community` | A join card for a WhatsApp group/channel, Telegram, Discord or Line: member count and a Join button | `{platform, url, title, members_note?}` |

#### P3-B: implemented (BE)
The common rules match P3-A: money is in minor units with `currency` (IDR | USD | SGD | MYR, empty → IDR); links are http(s) only; image fields also accept `/uploads/`; every type takes `schedule?`; errors are `invalid_data` with `details` keyed by path.

**Static cards (tautan-be PR #20)**

| type | shape and limits |
|---|---|
| `media_kit` | `title?` ≤60, `platforms` 1–8 × `{platform: instagram\|tiktok\|youtube\|x\|facebook\|threads\|twitch\|linkedin\|spotify\|podcast\|blog, handle 1–60, followers ≥0, avg_views? ≥0, engagement? 0–100 (%), url?}`, `audience? {age[] ≤8, gender[] ≤4, cities[] ≤10}`, each slice `{label 1–40, pct 0–100}` with sums ≤100, `updated_at` YYYY-MM-DD, `pdf_url?` (http(s) or /uploads/) |
| `rate_card` | `title?` ≤60, `packages` 1–12 × `{name 1–80, price_from?, deliverables 1–10 × 1–100, turnaround? ≤40}`, `currency`, `note?` ≤300, `button_label?` ≤40, `form_block_id?`, which must be a live `form` or `contact_form` block **on the same page** (checked on create and patch) |
| `brands` | `title?` ≤60, `logos` 1–30 × `{image_url, name 1–60, url?}` |
| `shop_the_look` | `image_url`, `alt?` ≤200, `currency`, `spots` 1–12 × `{x 0–100, y 0–100 (percent of the image box, from the top left), title 1–80, price?, url, code? ≤40}` |
| `gallery` | `title?` ≤60, `images` 1–30 × `{url, alt? ≤200, link_url?}`, `layout` carousel \| grid (default grid) |
| `community` | `platform` whatsapp_group \| whatsapp_channel \| telegram \| discord \| line, `url`, which must be https on that platform's host (chat.whatsapp.com; whatsapp.com/channel/…; t.me / telegram.me; discord.gg / discord.com; line.me), `title` 1–80, `members_note?` ≤60, `button_label?` ≤40 |
| `events` | `title?` ≤60, `tz` IANA (required), `mode` dates \| weekly. **dates:** `dates` 1–50 × `{date YYYY-MM-DD, time? HH:MM, title? ≤80, city? ≤60, venue? ≤100, url?, status? sold_out\|few_left\|cancelled}`. **weekly:** `weekly` 1–20 × `{days 1–7 unique mon..sun, start HH:MM, duration_min 5–720, title? ≤80, platform? tiktok\|instagram\|youtube\|twitch\|facebook\|shopee\|other, url?}`. The array for the other mode is dropped on save. |

The payment card's `confirm` with kind `form` now gets the same same-page form check as `rate_card.form_block_id`.

**Events in public reads.** `GET /public/{u}` adds `data.computed` to events cards. The editor read never includes it.
```
computed: {
  computed_at: "2026-11-20T13:30:00Z",
  live: {index, starts_at, ends_at} | null,   // weekly: the slot on air now (index into weekly[])
  next: {index, starts_at, ends_at} | null,   // weekly: the next slot to start (looks 7 days ahead)
  past_hidden: 1,                              // dates: how many past dates were dropped
  today: "2026-11-20"                          // dates: today in the card's tz
}
```
- **Dates mode:** dates before today in the card's `tz` are removed from `dates[]` in the public read.
- **Weekly mode:** slot times are computed in `tz`, so they stay correct across DST, and overnight slots that started the day before count as live. The times are UTC instants.
- **Client correction:** the public page is cached (s-maxage 60, swr 600), so "Live now" should be checked on the client against `live.ends_at` and `next.starts_at`, with no schedule maths needed. Render the server value first so the layout doesn't shift.

**Fetched cards (tautan-be PR #21).** Visitors' browsers never call Instagram, TikTok, YouTube or podcast hosts for these cards.
- The server fetches through the SSRF-safe fetcher and re-hosts every thumbnail and cover as an upload, so each gets AVIF/WebP variants and appears in `media`.
- Results are cached in `remote_cache` and merged into `data.resolved` in both the public and editor reads.
- A refresher runs every 10 minutes and refetches whatever is due: feeds hourly, oEmbed posts every 7 days. Failed fetches retry after 15 min for feeds and 6 h for posts, and a failure never replaces the last good result.
- A save fetches new keys straight away, in the background.
- When cached content changes, the revalidate webhook fires for the published pages that use it.

| type | stored shape | `data.resolved` (read only) |
|---|---|---|
| `post_grid` | `title?` ≤60, `posts` 1–12 × `{url, caption? ≤200, platform}`. `platform` is set by the server from `url`: instagram (`/p/`, `/reel/`, `/tv/`), tiktok (`/@user/video/…`, vm./vt.tiktok.com), youtube (`watch?v=`, `/shorts/`, youtu.be). Other URLs → 400. | `{posts: [{status: ok\|link_only\|pending, url, platform, title?, author?, thumb_url?}]}`, one per post, in order. `link_only` means no thumbnail was obtainable (typically Instagram, whose oEmbed needs an app token; set `INSTAGRAM_OEMBED_TOKEN` on the BE to enable it), so render a link card. |
| `latest_video` | `title?` ≤60, `channel_id` (`UC` + 22 chars), `show: {count 1–3}` (default 1) | `{status: ok, fetched_at, channel_title, videos: [{video_id, title, url, published_at, thumb_url}]}` (newest first, trimmed to `show.count`) or `{status: "pending"}`. Render a click-to-load facade with `thumb_url` as the poster. |
| `podcast` | `title?` ≤60, `feed_url` http(s), `count` 1–10 (default 3) | `{status: ok, fetched_at, title, image_url, episodes: [{title, published_at?, audio_url, audio_type?, duration_sec?, link?}]}` (feed order, trimmed to `count`) or `{status: "pending"}`. `image_url` is re-hosted. `audio_url` stays on the podcast host, so use `<audio preload="none">` and the browser only fetches it on play. |

**Embed posters (tautan-be PR #23).** `video` and `music` blocks gain a read-only `data.resolved` in page reads. It is fetched on save and refreshed weekly through the same remote cache. A failed fetch keeps the last good result and retries after 6 h.
```
resolved: {status: "ok", fetched_at, provider: youtube|vimeo|tiktok|spotify|soundcloud|other,
           title?, author?, poster?: "/uploads/<name>.jpg", video_id?: "<youtube id>"}
or {status: "pending"} before the first fetch
```
- **Where the poster comes from:** YouTube uses the 1280x720 `maxresdefault` frame when it exists, otherwise the oEmbed thumbnail (`hqdefault`). Vimeo, TikTok, Spotify and SoundCloud use their oEmbed `thumbnail_url`.
- **Media map:** the poster is re-hosted, so it has an entry in `media` with width, height and AVIF/WebP widths. Use it as the facade `<img>`, with `priority` when it is the LCP. The iframe loads only on click.
- **Other providers:** `provider: "other"` with no poster.

**/unfurl for YouTube channels.** Pasting `youtube.com/@handle`, `/c/…`, `/user/…` or `/channel/UC…` returns `youtube_channel_id`, `channel_title` and `suggested_type: "latest_video"`.
- The id is read from the channel page: the canonical link, then `itemprop=identifier`, then `externalId`.
- A `/channel/UC…` URL yields the id even when the fetch fails.

### P3-C Engage
| type | what it does | data |
|---|---|---|
| `poll` | A single question with 2–5 options; results are shown after voting (one vote per visitor per day, using the analytics salt) | `{question, options[], show_results: after_vote\|always\|never, ends_at?}` |
| `ask_me` | Anonymous questions into a dashboard inbox; answered ones can be pinned as public Q&A | `{prompt, public_answers: bool}` |
| `lead_magnet` | A free download gated by email: it adds the visitor to Audience, then reveals the file | `{title, file_url, cover_url?, button_label}` |
| `giveaway` | Entry with email plus optional follow checklist links, an end date, and the winner announced on the card | `{title, prize, ends_at, steps:[{label, url}], rules_url?}` |

#### P3-C/D: implemented (BE)
**P3-C Engage (tautan-be PR #22).** Common rules are as before. `subscribe` uses the form semantics: `everyone` | `ask` (the default here; the visitor's `subscribe: true` opts in) | `none`.

| type | stored shape |
|---|---|
| `poll` | `question` 1–200, `options` 2–5 × `{id, label 1–80}`. `id` matches `[a-z0-9_-]{1,40}` and is unique; it is assigned by the server when empty, so send it back on edits so votes stay attached. `show_results` after_vote \| always \| never (default after_vote), `ends_at?` |
| `ask_me` | `prompt` 1–200, `public_answers` bool, `placeholder?` ≤100, `button_label?` ≤40 |
| `lead_magnet` | `title` 1–100, `description?` ≤300, `file_url` (http(s) or /uploads/; **never sent in public reads**), `cover_url?`, `button_label?` ≤40, `success_message?` ≤200, `subscribe` |
| `giveaway` | `title` 1–100, `prize` 1–200, `ends_at` (required), `steps` 0–5 × `{label 1–80, url}`, `rules_url?`, `subscribe` |

Read-only additions in page reads:
- **poll:** `closed` (bool). `results: {counts: {<option id>: n}, total}` appears publicly only when `show_results=always`, and always in the editor read.
- **ask_me:** `answers: [{question, answer, answered_at}]` holds the pinned, answered questions, newest first, at most 20. It appears publicly only when `public_answers`, and always in the editor read.
- **lead_magnet:** `file_url` is removed in public reads.
- **giveaway:** `ended` (bool), `entry_count`, and once ended and drawn, `winner: {display, drawn_at}`. `display` is "Budi S." from the name, or a masked email such as "bu***@gmail.com". The email itself is never public.

Public endpoints. Each returns 404 when the card isn't live on the page, and 403 `locked` if the card is gated and not unlocked.
- `POST /public/{u}/blocks/{id}/vote` `{option_id, visitor_id?}`:
  - `200 {voted, results?}`. `results` is omitted when `show_results=never`.
  - `409 {error:{code:"already_voted"}, voted, results?}` once per visitor per UTC day. Here `voted` is the option stored earlier today, not the one just sent.
  - **Who counts as a visitor** (tautan-be #39, E2E 2026-11-18 #5): the analytics daily-salt hash of `visitor_id` when one is sent, otherwise of IP + UA.
    - `visitor_id` is a random first-party id the public page keeps in `localStorage['tautan:vid']`, matching `^[A-Za-z0-9_-]{16,64}$`. A malformed one is ignored and IP + UA is used.
    - It exists because many Indonesian mobile users share carrier-NAT IPs and identical iOS Safari UAs, so IP + UA alone refused real voters.
  - `409 poll_ended`, `400 invalid_option`.
  - `429`: 30 per 10 min per IP + `visitor_id` per card, plus a ceiling of 300 per 10 min per IP per card, so rotating ids can't stuff the ballot.
- `GET /public/{u}/blocks/{id}/vote?visitor_id=…` → `200 {voted: option_id|null, results?}`, `Cache-Control: private, no-store` (tautan-be #39).
  - Results follow `show_results`: always included for `always`, included once `voted` is set unless `never`.
  - The page is ISR-cached, so the FE (tautan-fe #75) records "voted today" (UTC) per poll in `localStorage['tautan:poll:<block id>']`. It calls this endpoint on load only for those polls, then shows results / "You voted today" instead of the Vote button. A `null` answer clears the record.
- `POST /public/{u}/blocks/{id}/ask` `{question 1–500, visitor_id?}` → `204`. `429` after 5 per 10 min per IP + `visitor_id` per page, plus 60 per 10 min per IP per page.
- `POST /public/{u}/blocks/{id}/lead` `{email, name?, subscribe?}` → `200 {download_url, expires_at, emailed}`.
  - The contact is upserted into Audience.
  - `download_url` is `/api/v1/public/{u}/download/<signed token>` and is valid for 15 min. It serves `/uploads/` files as an attachment (`Cache-Control: private, no-store`) and 302-redirects to external files. Responses: `403 invalid_token`, `410 expired`.
  - **Emailed copy (P1 email item 2):** the same kind of link, valid for **7 days**, is also emailed to the visitor through the outbox, subject "Your download: <title>". This only happens when `MAIL_DRIVER=smtp` and the page owner's email is confirmed (an unconfirmed throwaway account can't use the form to send mail). `emailed` reports whether it was queued; the on-page link is always returned, so it stays the fallback.
  - Anti-abuse: at most 3 lead emails per recipient address per 24 h across all pages, and 100 per card per hour, on top of the 10 per 10 min per-IP form limit. Past a cap the request still succeeds with `emailed: false`. The email uses only text that's already public (card title, page name and URL) and has no Reply-To.
- `POST /public/{u}/blocks/{id}/enter` `{email, name?, subscribe?}` → `200 {entered: true, already_entered}`. Entries are deduped by email per giveaway, and the contact goes into Audience. `409 giveaway_ended`.

Owner endpoints:
- **Polls:** `GET /polls/{id}/results` → `{counts, total}`.
- **Questions:**
  - `GET /questions?status=unanswered|answered|all&block_id=` → `{items:[{id, block_id, question, answer, answered_at, pinned, created_at}]}`, newest first, at most 500.
  - `PATCH /questions/{id}` `{answer?, pinned?}`. An empty answer clears it and unpins. Pinning an unanswered question → `400 not_answered`.
  - `DELETE /questions/{id}` → `204`.
- **Giveaways:**
  - `GET /giveaways/{id}` → `{entries, winner: {email, name, display, drawn_at} | null}`.
  - `POST /giveaways/{id}/draw` `{notify?: bool}` (body optional) → uniformly random (crypto/rand), only after `ends_at` (otherwise `409 not_ended`, or `409 no_entries`). Drawing again replaces the winner.
  - **Winner email (P1 email item 3):** unless `notify: false`, the drawn entrant gets a "You won: <title> (<page>)" email through the outbox. It contains the prize, the page link and "they'll get in touch at this address". It has no Reply-To, so the owner's account email stays private. The response gains `notified: bool`. The email goes out only when the owner's email is confirmed. Caps: 5 winner emails per card per day (redraws included) and 2 per recipient per day across all pages. Past a cap, the draw still succeeds with `notified: false`. A redraw emails the new winner; the previous winner is not told, so the FE should say so before redrawing.
  - Answer, pin, delete and draw fire the revalidate webhook.

The same-page form check (`rate_card.form_block_id`, `payment.confirm` kind form) accepts `form`, `contact_form` and `email_signup` blocks.

**P3-D Text and layout (tautan-be PR #24).**

| type | stored shape |
|---|---|
| `spotlight` | `image_url?`, `eyebrow?` ≤40, `headline` 1–100, `text?` ≤300, `button? {label 1–40, url}`, `align` left \| center (default left) |
| `marquee` | `text` 1–200, `separator` ≤8 (default "•"), `speed` slow \| normal \| fast (default normal), `direction` left \| right (default left), `style` ribbon \| plain (default ribbon). Not clickable. |
| `timeline` | `title?` ≤60, `style` numbered \| dotted (default numbered), `items` 1–20 × `{eyebrow? ≤40, title 1–100, description? ≤300, badge? ≤24, highlight?}`, with at most one highlighted |
| `resume` | `title?` ≤60, `items` 1–30 × `{role 1–80, company 1–80, company_url?, start YYYY-MM, end? YYYY-MM (≥ start), current?, description? ≤500}`. `end` is dropped when `current`, and the FE shows "Present". |
| `before_after` | `before_url`, `after_url` (images), `before_label?` ≤20, `after_label?` ≤20, `alt?` ≤200, `start_pct?` 0–100 (FE default 50) |
| `map` | `label?` ≤80, `address?` ≤200, `area?` ≤100, `lat` −90..90, `lng` −180..180 (both required, rounded to 5 decimals ≈ 1 m), `zoom` 3–19 (default 15), and `links: {google, apple, osm}` |

`map.links` holds directions links built by the server; input values are ignored.

**Map rendering: data-only, drawn by the FE.**
- **No tiles:** the backend serves no map image, and visitors' browsers load no third-party tiles. That avoids tile licensing and API keys, and the card costs zero network requests.
- **FE:** render a stylised SVG location card (pin, label, address/area) from `lat`/`lng`/`zoom`, with Directions buttons from `links`. The owner editor may use any picker to set `lat`/`lng`.
- **Later:** a real static street image can be added with a keyed tile provider (rendered server-side once into `/uploads/`) without changing this shape.

**Additions to existing types:**
- `text.style?`: card \| sticky_note \| speech_bubble \| torn_paper \| chalkboard.
- `discount_code.codes?`: 0–10 × `{code 1–40, description? ≤300, discount? ≤24, url?, ends_at?}`, shown in sequence (swipe). The existing single-code fields stay valid.
- `social_icons.variant?`: icons (default; stored as absent) \| donate. With donate, every item's `platform` must be saweria \| trakteer \| kofi \| paypal \| buymeacoffee.

### P3-D Text and layout (parity with liinks.co, plus showpieces)
| type | what it does |
|---|---|
| `spotlight` | A big hero card: image, headline, short text and a button (a drop, a launch) |
| `marquee` | Scrolling text ribbon (phrase, separator, speed, direction, ribbon or plain); not clickable |
| `timeline` | Numbered or dotted steps (eyebrow, title, description, badge), one of them highlighted |
| `resume` | Work history (role, company plus link, dates, "Present", description) |
| `before_after` | A drag slider between two photos (salons, renovation, editing) |
| `map` | Location card: a static map of the street and area (no iframe) with a Directions link |
- **text** gains `style`: card \| sticky_note \| speech_bubble \| torn_paper \| chalkboard.
- **discount_code** gains several codes in sequence (swipe).
- **social_icons** gains a `donate` variant.

### Card-level gates (any card)
`gate: {kind: password | email | age_18 | follow, ...}`: the card shows locked until the visitor
unlocks it. Email gates feed Audience. The server never sends a gated card's `data` until it is unlocked.

#### Card-level gates: implemented (BE, tautan-be PR #25)
**Setting a gate.** `POST /blocks` and `PATCH /blocks/{id}` take a top-level `gate`, next to `data` and not inside it. `null` removes the gate.
```
gate: {kind: "password", password: "4–72 chars", message?: "≤200"}   // password is write-only; omit it on PATCH to keep the current one
gate: {kind: "email", subscribe?: everyone|ask|none (default ask), message?}
gate: {kind: "age_18", message?}
gate: {kind: "follow", follow: {platform: "instagram", url: "https://…"}, message?}   // honour system
```
The password is stored as a bcrypt hash in `blocks.gate`, a column that is never part of `data`.

**Reads:**
- **Editor:** `block.gate = {kind, message?, follow?, subscribe?, has_password}`. The hash is never sent.
- **Public:** a gated card comes back as `{…block, data: null, locked: true, gate: {kind, message?, follow?}}`.
  - The children of a gated folder are stubbed the same way, with `locked: true`.
  - Nothing from a locked card reaches `media` or the payload, so the cached public page is the locked version.

**Unlocking:** `POST /public/{u}/blocks/{id}/unlock`. The body depends on the kind:
- password: `{password}`
- email: `{email, name?, subscribe?}`, which also upserts the contact into Audience
- age_18: `{confirm_age: true}`
- follow: `{followed: true}`
- any kind: `{token}` to re-open the card with a token saved earlier

Responses:
- `200 {block, children: [...], media, unlock_token, expires_at}`, with `Cache-Control: private, no-store`.
  - `block` and `children` are processed exactly as in public reads: resolved, computed, results and so on.
  - The token lasts 30 days and is HMAC-signed over the block id and a fingerprint of the gate, so editing or removing the gate invalidates old tokens.
- Errors: `403 wrong_password` (or an invalid token), `400 invalid_email` / `confirm_age` / `followed` / `not_gated`, `404`, `429` (10 attempts per 10 min per IP per card).

**Using a token:**
- Store `unlock_token` per card (localStorage). On load, call unlock with `{token}` to swap the stub for the data. That keeps the HTML cacheable.
- The card's own public endpoints (`vote`, `ask`, `lead`, `enter`, `/form`, `/subscribe`, `/contact`) answer `403 locked` for gated cards unless the request carries `?unlock=<token>`. The query parameter avoids a CORS preflight. For a child of a gated folder, use the folder's token.

### Payments: Xendit, Midtrans and Stripe (owner: tautan-41; approved by the user 2026-11-18)
Paid digital downloads, a cart and checkout for `shop`, then paid bookings, memberships and courses.

**Model.** The creator connects their **own** provider account(s) in settings: Xendit (Invoices), Midtrans (Snap) and/or
Stripe (Checkout Sessions), each in `sandbox` or `live` mode. Money goes straight to the creator's account. Tautan never
holds funds and takes no cut (for now). Buyers pay on the provider's **hosted page**: the buy button is server HTML, and
checkout is a redirect. No provider JS ever loads on `/[username]`.

Build order, one feature per PR: **(a)** provider settings, encryption and test connection → **(b)** paid digital
download (price on `file`/`product`, checkout, signed expiring download link on the success page plus an order lookup,
since there is no email sending yet) → **(c)** `shop` cart checkout → **(d)** paid bookings and memberships (later).

#### Payments (a): provider settings: implemented (BE)
- **Env (BE).** `PAYMENTS_ENCRYPTION_KEY`: 32 bytes, base64 or hex (`openssl rand -base64 32`). It is required
  when `APP_ENV=production`. Elsewhere payments are off while it is empty. A malformed key stops the API from starting. `PAYMENTS_ENCRYPTION_KEYS_OLD`: comma-separated retired
  keys, still readable during a rotation. `make dev` falls back to a built-in dev key.
- **Storage.** `payment_providers(profile_id, provider, mode, enabled, credentials bytea, hints, verified_at, last_error)`,
  with one row per provider per profile. `credentials` is AES-256-GCM over `{secret_key, webhook_secret}`, with the
  row's `profile_id|provider|mode` as associated data. Keys are **never** returned. The API shows only the last 4
  characters (`"…1234"`).
- **Credentials per provider:**

  | provider | `secret_key` | `webhook_secret` | currencies |
  |---|---|---|---|
  | `xendit` | secret API key: `xnd_development_…` (sandbox) / `xnd_production_…` (live), with Money-in write permission | callback verification token (Dashboard → Settings → Webhooks), sent back as `x-callback-token` | IDR, USD |
  | `midtrans` | server key: `SB-Mid-server-…` (sandbox) / `Mid-server-…` (live) | none: notifications are signed with the server key (`signature_key` = SHA512) | IDR only |
  | `stripe` | `sk_test_`/`rk_test_` (sandbox) or `sk_live_`/`rk_live_` (live). A restricted key needs Checkout Sessions write. | signing secret `whsec_…` from the webhook endpoint the creator adds in Stripe | IDR, USD |

- `GET /payments/providers` → `{available, providers: ProviderSettings[]}`. `available` is false when the server has no
  encryption key. In that case the FE shows "Payments aren't available yet" and the write endpoints answer
  `503 payments_unavailable`. The list always has all three providers, in the order xendit, midtrans, stripe:
  ```
  ProviderSettings {provider, configured, enabled, mode: "sandbox"|"live"|null,
    secret_key_hint: "…1234"|null, webhook_secret_hint: "…abcd"|null, needs_webhook_secret: bool,
    webhook_url: "https://<api>/webhooks/<provider>/<profile_id>", currencies: ["IDR","USD"],
    verified_at: ts|null, last_error: {code, message}|null, updated_at: ts|null}
  ```
- `PUT /payments/providers/{provider}` with `{mode?, secret_key?, webhook_secret?, enabled?}` → `{provider: ProviderSettings, test: {ok, code?, message?} | null}`.
  - Creating a connection needs `mode` and `secret_key`.
  - Secrets are write-only. Omit one to keep the stored value. `webhook_secret: ""` clears it.
  - Switching `mode` needs the new mode's `secret_key`. Stored secrets don't carry over between modes.
  - When the key or mode changes (or the keys were never verified), the server **tests the connection** in the same
    request and returns the result in `test`. A failed test still saves the keys, but forces `enabled: false` and
    records `last_error`. A toggle-only PUT doesn't call the provider (`test: null`).
  - `enabled` defaults to the stored value, and to false after a key change. Turning on Xendit or Stripe needs a
    `webhook_secret`: `400 webhook_secret_required` (`details.webhook_secret`).
  - Errors: `400 invalid_data` with `details` per field (`mode`, `secret_key`, `webhook_secret`). Messages are written
    for the creator, e.g. "this is a publishable key; use a secret key (sk_…) or a restricted key (rk_…)" or "live mode
    needs a live key starting with xnd_production_". Unknown provider → 404.
- `POST /payments/providers/{provider}/test` → `{provider, test}`. It re-tests the stored keys. A pass sets
  `verified_at`. A failure sets `last_error` but leaves `enabled` alone (an outage shouldn't stop sales).
  Errors: `404 not_configured`, `409 credentials_unreadable` (the encryption key changed without listing the old
  one, so the creator re-enters the keys), `429 rate_limited` (20 per 10 min per profile).
- `DELETE /payments/providers/{provider}` → 204.
- `test.code` / `last_error.code`: `invalid_credentials` (the provider answered 401/403, with its message),
  `provider_unreachable` (network or timeout) or `provider_error`.
- **Webhook URL.** The creator pastes `webhook_url` into the provider dashboard:
  - Xendit: Settings → Webhooks → "Invoices paid".
  - Stripe: Developers → Webhooks → add endpoint, events `checkout.session.completed`,
    `checkout.session.async_payment_succeeded`, `checkout.session.async_payment_failed`, `checkout.session.expired`
    and `charge.refunded`. Copy the signing secret back into Tautan.
  - Midtrans: nothing to paste. Each transaction carries `X-Override-Notification` with this URL. Show it anyway,
    for reference.
- **Provider interface (Go, `internal/payments`).** `CreateCheckout(order) → {ref, url, expires_at}`,
  `VerifyWebhook(req, body) → Event`, `GetStatus(ref) → Event` and `TestConnection()`. Adapters are unit-tested
  against fake HTTP servers; CI never calls a real provider.

#### Payments (b): paid digital download: implemented (BE, tautan-be PR "Payments (b)")
**Card data (additive).**
- `file` gains `price?` and `currency?`:
  - `price` is a positive integer in minor units.
  - `currency` is IDR|USD and defaults to IDR when a price is set.
  - IDR prices must be whole rupiah (a multiple of 100), because Xendit and Midtrans take no sen.
- `product.action.kind` gains `checkout`, with `download: {url, file_name?}`:
  - `url` is http(s) or `/uploads/…`.
  - `price` is required, with the same rules as above.
  - Shop items can use it too (bought by `item_id`).
  - `download` is dropped from every other action kind.
- **Deliverables are private.**
  - On save, a priced card's local upload is copied to a private name, `/uploads/p_<hex>.<ext>`, owned by the
    profile, and the card's URL is rewritten to it.
  - `GET /uploads/p_…` is always 404, so only the signed order download serves these files.
  - `POST /uploads?kind=private` uploads a deliverable straight to a private name (same file types as
    `kind=file`); the FE should use it for paid files.
  - A free file can't point at a private upload: `400 invalid_data` on `url` ("upload it again to give it away for free").
  - A card can only reference private files its own profile owns. Anything else is `400 invalid_data`
    ("file not found; upload it again").
  - External deliverable URLs are left as they are.
- Validation errors use the usual `400 invalid_data` with `details` (`price`, `currency`, `action.download.url`).

**Public reads** (`GET /public/{u}`, unlock responses):
- A priced file comes back with `url: ""`.
- Checkout products (and shop items) come back without `action.download`.
- The editor keeps both.
- Page reads gain top-level `payments: {providers: [{provider, currencies}], currencies: ["IDR","USD"]} | null`.
  This covers the enabled providers only. Changing provider settings triggers the revalidate webhook.
- The FE shows a buy button only when `payments.currencies` includes the card's currency.

**Checkout (buyer, no auth):**
- `POST /public/{u}/orders` with `{block_id, item_id? (shop), options?: {"<variant name>": "<option>"}, email, name?, provider?}` → `201 {order_id, code, token, checkout_url, expires_at}`.
  - The FE then redirects (303) to `checkout_url`.
  - Prices come from the stored card. Every variant group needs a valid choice.
- **Plain HTML form mode.** The same path accepts `application/x-www-form-urlencoded` with fields `block_id`,
  `item_id?`, `email`, `name?`, `provider?` and `option.<variant name>=<choice>`. It answers `303` to `checkout_url`,
  or, on any error, `303` to `{APP_BASE_URL}/{u}/checkout?block=…&item=…&error=<code>`.
  - The FE checkout page posts straight to the API, with no JS and no FE proxy, so rate limits see the buyer's IP.
  - Redirects carry `Referrer-Policy: no-referrer` and `Cache-Control: private, no-store`.
  - Order lookup has the same form mode: `email` and `code` → `303` to the order page, or to
    `{APP_BASE_URL}/{u}/orders?error=not_found`.
- `provider` can be left out when exactly one enabled provider supports the currency. Otherwise the API answers
  `400 {error: {code: "provider_required"}, providers: ["midtrans","stripe"]}`.
- Errors:
  - `400 invalid_email` / `invalid_name` / `invalid_options` (`details`) / `not_for_sale` / `invalid_provider`
  - `404` (card not on this page, hidden or out of schedule)
  - `403 locked` (a gated card: unlock first, then pass `?unlock=<token>`)
  - `409 payments_unavailable` (no enabled provider takes this currency)
  - `429` (10 per 10 min per IP per page)
  - `502 checkout_failed`: the provider refused. The order is marked failed, and the reason goes to the creator's
    `last_error` in Settings.
- The checkout stays open for 1 hour (Stripe's minimum is 31 min).
- Return URLs (both point at the FE):
  - success: `{APP_BASE_URL}/{u}/orders/{order_id}?t={token}`
  - cancel: the same URL plus `&cancelled=1`
- Midtrans gets `X-Override-Notification: <webhook url>` on every transaction.

**Order page (buyer):** `GET /public/{u}/orders/{id}?t={token}` → `{order, downloads}`, sent with `Cache-Control: private, no-store`.
```
order: {id, code, status, amount, currency, items: [{block_id, item_id?, kind: file|product, name, variant?, unit_amount, quantity, file_name?}],
        buyer_email, buyer_name, provider, mode, created_at, paid_at, expires_at, checkout_url (pending and not expired, else null)}
downloads: [{index, name, file_name, url, expires_at}]   // only while status = paid; links are signed and last 24 h
```
- A wrong token or another page's order → 404.
- **Buyer access survives the seller's changes.** The order page, downloads and lookup ignore `is_published` and
  follow username renames. Only creating a new order needs a published page.
- The order URL is a **capability URL**: the token never expires. The FE order page must send
  `Referrer-Policy: no-referrer`, so the token doesn't leak to external file hosts, and logs should drop the `t=`
  query parameter.
- While an order is pending (or expired/failed for up to 7 days), each view asks the provider for its status, at
  most once per 10 s per order. That covers late or missing webhooks, so the page can simply refresh itself, e.g.
  `<meta http-equiv="refresh" content="5">` while pending.
- Download URLs are `GET /api/v1/public/{u}/orders/download/{token}`:
  - uploads come back as an attachment named after `file_name`; external URLs come back as a 302
  - 30 downloads per order in total. A download counts once per signed link: every request that link serves
    (Range, resumes, seeking) is free, but a new link at the limit gets `429 download_limit`. Each view of the order
    page mints fresh links.
  - External deliverable URLs are just a 302 to the creator's URL, so they aren't protected. The editor should say
    that uploading the file to Tautan is what keeps it private.
  - Errors: `410 expired` (open the order page for fresh links), `403 invalid_token` / `not_paid` (including
    refunded), `429 download_limit`
- **Lookup:** `POST /public/{u}/orders/lookup {email, code}` → `{order_id, token}`, or `404 not_found`.
  - The code is case-insensitive, the dash and spaces are optional, and O/I/L read as 0/1.
  - Limited to 10 per 10 min per IP.
  - Codes look like `K7Q2-M9XD` and appear in the provider's description, so they show on the receipt.

**Webhooks:** `POST /webhooks/{provider}/{profile_id}` on the API origin, outside `/api/v1`.
- Responses:
  - 401 bad signature
  - 404 provider not connected
  - 200 `{result}` for everything handled: `paid` | `pending` | … | `duplicate` | `unknown_order` | `ignored` (`mode_mismatch`)
  - 500 makes the provider retry, and the event isn't recorded
- **What authenticates what:**
  - Stripe events are fully signed, so they're used as sent.
  - Midtrans signs only `order_id + status_code + gross_amount`, and Xendit authenticates with a static token and no
    body signature. For these two the webhook is only a **trigger**: the API calls `GetStatus` with the creator's key
    and acts on that answer. If the provider can't be reached, the API answers 502 and the provider retries.
  - Midtrans settlement/capture also requires `status_code` 200.
- Limited to 300 requests per minute per page (429), so junk posts don't each cost a credential decrypt.
- Idempotent on `payment_events (profile_id, provider, event_key)`:
  - Stripe: the event id.
  - Midtrans: `order_id:status_code:gross_amount` (signed fields only) plus the confirmed state.
  - Xendit: `inv:<invoice id>` plus the confirmed state (type, status, amount, currency).
- **Transitions only move forward:**
  - pending → paid/expired/failed
  - expired/failed → paid (a late payment wins)
  - paid → refunded
- Anything else is ignored, so replays and out-of-order events are harmless.
- **"Paid" needs the provider to report an amount and currency that both match the order.** A missing amount or
  currency counts as a mismatch. In that case the order stays pending (reconcile settles it later), the event is
  marked `amount_mismatch`, and it's logged as an error.
- On the move to paid, two things happen, both inside the order's transaction:
  - The buyer is upserted into Audience (not subscribed), with the bought card as the source.
  - A receipt email is queued through the outbox (`order_receipt`): seller name, order code, line items, total, and
    a "Get your download" button linking to the order page. It never contains the file URL. The email is sent once,
    even for replayed webhooks.
  - Receipts are capped at 60 per page per hour and 5 per recipient per hour. Over the cap, the order is still paid;
    only the email is skipped.

**Dashboard (owner):**
- `GET /orders?status=all|pending|paid|expired|failed|refunded&q=&cursor=` → `{items: Order[], next_cursor, counts: {all, pending, paid, expired, failed, refunded}, revenue: {"IDR": minor, …}}`
  - 20 per page, newest first.
  - `q` matches email, name or code. Counts ignore the status filter.
  - `revenue` sums paid orders.
- `Order {id, code, status, buyer_email, buyer_name, items (as above), amount, currency, provider, mode, provider_ref, downloads, created_at, expires_at, paid_at, refunded_at}`.
- `GET /orders/{id}` → `{order, events: [{type, status, received_at}], order_page_url}`.
  `order_page_url` is the buyer link, to resend by hand.
- `POST /orders/{id}/refresh`: re-checks with the provider, then returns the same shape as `GET /orders/{id}`.
  `502 provider_error` when the provider can't be reached.
- `GET /orders.csv?status=&q=`, with columns Created at, Order, Status, Buyer email, Buyer name, Items, Amount
  (major units), Currency, Provider, Mode, Paid at and Provider reference. Cells are formula-safe.

#### Payments (c): shop cart checkout: implemented (BE, tautan-be PR "Payments (c)")
One cart per `shop` card, paid in one checkout. Physical items ship. Prices, variants and the shipping fee always come
from the stored card; the browser only says which items, how many and which options.

**Card data (additive).**
- `product.action` (checkout) gains `delivery: "download" | "ship"`. It defaults to `download` and is stored as absent.
  - `ship` is for shop items only. A `product` card with `ship` → `400 invalid_data` on `action.delivery`.
  - A `ship` item has no deliverable: `action.download` is dropped on save.
- `shop` gains `shipping_fee?`, a flat fee per order in minor units. It's added once when the cart has a `ship` item.
  `0` is stored as absent (free shipping). It must be whole rupiah for IDR.
- All checkout items in one shop must use one currency (`400 invalid_data` on `items[i].currency`). The shipping fee is
  in that currency.
- Public reads include `delivery` and `shipping_fee`, since the FE needs both.

**Checkout.** `POST /public/{u}/orders` also accepts a cart:
```
{block_id, items: [{item_id, quantity?, options?: {"<variant name>": "<option>"}}], email, name?, provider?,
 shipping?: {name, phone, address, city, postal_code, country?, notes?}}
```
- `items` is for shop cards only, and replaces `item_id`. Sending both, or sending `items` for another card type →
  `400 invalid_items`.
- 1–20 lines. A `ship` line takes `quantity` 1–20 (default 1). A `download` line is always 1; anything else →
  `400 invalid_quantity`.
- Equal lines (same item and variant) merge: `ship` quantities add up (the merged total is capped at 20); a repeated
  download counts once.
- **Shipping address**, required when anything ships, on the cart or the single `item_id` path → otherwise
  `400 invalid_shipping` with `details` per field (`shipping.name`, `shipping.phone`, …):
  - `name` (≤100), `address` (≤300), `city` (≤80), `notes` (≤300, optional).
  - `phone`: loose. Spaces, dashes, dots and brackets are dropped, then `^\+?[0-9]{7,15}$` (08…, 62…, +62…, or
    international numbers).
  - `country`: ISO 3166-1 alpha-2, default `ID`. `postal_code` is required with 5 digits for `ID`, and optional
    elsewhere (letters, digits, spaces and dashes, max 12 characters).
  - The address is buyer personal data. Only the page owner sees it (order detail, CSV). It's never logged, never in
    redirects, and never sent to the payment provider.
  - Retention: an hourly job clears the address of orders that were never paid (pending, expired or failed) 30 days
    after their checkout closed. Paid and refunded orders keep it for the seller's records.
  - In `orders.csv`, a phone number like `+6281234567890` keeps its `+`; other cells starting with `= + - @` are
    still quoted (formula-safe).
- The total is the lines plus the shipping fee. The provider gets each line plus a "Shipping" line, and the description
  "N items from @u · order CODE".
- **Amount limits, checked before redirecting.** Providers that can't take the total are left out of the automatic
  choice and of `provider_required`. When none can → `400 amount_too_small` / `amount_too_large`, with a message such
  as "the total must be at least Rp 10.000 to pay online". Choosing such a provider explicitly gives the same error.

  | provider | IDR | USD |
  |---|---|---|
  | Xendit | Rp 1.000 – Rp 1.000.000.000 | US$1 – US$999,999.99 |
  | Midtrans | Rp 1.000 – Rp 1.000.000.000 | (IDR only) |
  | Stripe | Rp 10.000 – Rp 9.999.999.999 | US$0.50 – US$999,999.99 |

  These are conservative per-order bounds. A payment method may narrow them further (e.g. retail outlets from
  Rp 10.000), and the provider's page shows that.
- Other errors are as in (b): `invalid_options` (detail `items[i].options`), `not_for_sale` (detail `items[i]`),
  `payments_unavailable`, `429`, `502 checkout_failed`.

**Form mode (cart).** The same form post accepts `line.N.item`, `line.N.qty` and `line.N.option.<name>` for N = 0…19.
Gaps are skipped, and a non-numeric quantity is `invalid_quantity`. Shipping uses `shipping.name`, `shipping.phone`,
`shipping.address`, `shipping.city`, `shipping.postal_code`, `shipping.country` and `shipping.notes`.
- On error the 303 goes back to `{APP_BASE_URL}/{u}/checkout?block=…&error=<code>`, carrying the cart in the same
  field names (`line.N.item`, `line.N.qty`, `line.N.option.<name>`, bounded) and `field=<detail key>` for each failing
  field (e.g. `field=shipping.phone`). Email, name and address are never echoed.
- So the FE cart link has the same shape: `/{u}/checkout?block=<id>&line.0.item=tee&line.0.qty=2&line.0.option.Size=M&line.1.item=zine`.

**Orders.** `items[]` gains `delivery: "ship"` on physical lines. The order gains:
- Buyer view (`GET /public/{u}/orders/{id}`): `shipping_fee`, `fulfilment: null | "unfulfilled" | "shipped"`,
  `tracking` and `shipped_at`. Never the address. `downloads` lists only the download lines, with `index` = the line's
  position in `items`.
- Owner `Order`: the same, plus `shipping: {name, phone, address, city, postal_code, country, notes} | null`.
- `fulfilment` is null when nothing ships and `unfulfilled` until shipped.
- The receipt adds a Shipping line and "<seller> will ship your order". The button reads "View your order" when
  nothing downloads.

**Fulfilment (owner).**
- `POST /orders/{id}/ship {tracking?}` (tracking ≤100 characters) → the `GET /orders/{id}` shape.
  - Only for paid orders that have items to ship; otherwise `409 not_shippable`.
  - The first call sets `fulfilment: shipped` and `shipped_at`, then queues an `order_shipped` email (seller, order
    code, tracking, order link) under the same caps as receipts.
  - Later calls only update `tracking`; no second email.
- `GET /orders?fulfilment=unfulfilled|shipped` filters the list. `unfulfilled` means paid and not shipped: the "To
  ship" tab. `counts.to_ship` is the number of paid, unshipped orders; it follows `q` but not the filters.
- `orders.csv` adds Shipping fee, Fulfilment, Tracking, Shipped at, Ship to, Phone, Address, City, Postal code, Country
  and Delivery notes.

**Migration** 000022 (`orders.shipping`, `shipping_fee`, `fulfilment`, `tracking`, `shipped_at`). It must deploy after
000021 (tautan-6c's email change), because golang-migrate won't run a lower version once a higher one has run.

**Not in (c):** stock counts (the `sold_out` badge stays), shipping on single `product` cards, and per-region or
weight-based shipping rates.

#### Dev-only `fake` payment provider (for e2e without provider keys; migration 000023)
- Strictly opt-in: offered only with `PAYMENTS_FAKE=1` **and** `APP_ENV` not `production`. With `PAYMENTS_FAKE=1` and
  `APP_ENV=production`, the API refuses to start. When it's off, the provider isn't listed, `PUT /payments/providers/fake`,
  its webhook and `/dev/fake-checkout/*` all answer 404, and the server refuses to build the adapter.
- Sandbox only. `secret_key` is any string starting with `fake_`, with no webhook secret. It accepts IDR and USD.
- The checkout URL is `{PUBLIC_BASE_URL}/dev/fake-checkout/{ref}` (ref `fk_…`). That page is served by the API, shows the
  items and total, and has four buttons: Pay, Fail, Expire, and Pay half (amount mismatch). Each one records the outcome in
  memory and delivers `{ref, order_id}` to `POST /webhooks/fake/{profile_id}`. That handler confirms the outcome with
  GetStatus, like Xendit and Midtrans, so the amount and currency checks apply. The buyer is then redirected to the
  order page (`&cancelled=1` after Fail or Expire).
- Fake checkouts live in memory, so after an API restart, older fake orders stay pending.
- Each fake checkout settles once. After Pay half (amount mismatch), that order stays pending for good, so start a new order
  to pay.
- Never set `PAYMENTS_FAKE` on an internet-facing box (docs/ENV.md). A seller who turned the provider on would give buyers
  free downloads. Both compose files pin it to `"0"`.

## Performance budget for the public page `/[username]` (user requirement, 2026-11-16)

The page visitors open from a bio link has to feel instant on a mid-range Android on 4G. Every PR that touches the public page states its numbers.

| Metric (Lighthouse mobile, simulated Slow 4G, Moto G-class CPU) | Budget |
|---|---|
| LCP | ≤ 1.8 s (stretch 1.2 s); worst-case all-cards pages (`/heavy`, `/heavy_all`): ≤ 2.2 s (stretch 1.8 s), user decision 2026-11-18 |
| TBT | ≤ 150 ms |
| CLS | ≤ 0.05 |
| First-load JS for `/[username]` | ≤ 110 kB gzip (today: 165 kB) |
| TTFB, cached | ≤ 200 ms; uncached ≤ 600 ms |
| Lighthouse Performance score | ≥ 90 |

How we hit it:
- **Server components by default.** Cards that need no interaction render as zero-JS server HTML. Interactive bits are small client islands: copy button, variant chips, WhatsApp order form, poll, lightbox and carousel. Each island is loaded with `next/dynamic` when the card is near the viewport.
- **Caching.** `/[username]` uses ISR (`revalidate` with on-demand `revalidateTag('profile:{username}')` on publish, block changes and theme changes). The backend's `GET /public/{u}` sends `ETag` + `Cache-Control: public, s-maxage=60, stale-while-revalidate=600`. A publish triggers revalidation through a signed webhook to the frontend (`POST /api/revalidate`).
- **Images.** Uploads are resized on the backend into AVIF/WebP variants (`?w=` 160/320/640/1080) with stored width and height so layout never shifts. Images below the fold are lazy-loaded; the avatar and the first product image are `priority`. Use `sizes` correctly.
- **Embeds** (YouTube, Spotify, Calendly, maps, oEmbed posts) are click-to-load facades with poster images. No third-party iframe or script loads before interaction.
- **Fonts.** At most 2 families via `next/font`, subset to Latin, `display: swap`, preloaded.
- **Analytics and pixels.** Events go through `navigator.sendBeacon` after `load`. GA4 and Meta Pixel use `next/script` with `strategy="lazyOnload"`.
- **No layout shift** from the announcement banner (reserve its space server-side), the countdown (fixed-width digits) or the open-now badge (computed server-side, then corrected on the client without resizing).
- **CI/perf check:** a Lighthouse CI config (`lighthouserc.json`) with the budgets above, run against a seeded heavy page that has every card type. It asserts LCP ≤ 2.2 s because `/heavy` is the all-cards worst case. On those pages the simulated LCP sits at 2.0–2.2 s even with no JS, so the cost comes from requests the image and CSS make before LCP (perf audit 2026-11-17).

### Performance: implemented (BE)
- **2a Caching (tautan-be PR #14).** `GET /public/{u}` returns 200 with:
  - `ETag: "<base64url of sha256(body), 24 chars>"`;
  - `Cache-Control: public, s-maxage=60, stale-while-revalidate=600`.
  A matching `If-None-Match` (a list, `W/` and `*` all work) gets `304` with an empty body. The ETag hashes the response body, so any edit, publish or scheduled block flipping visibility changes it. The 301 for old usernames and the 404 are unchanged.
- **2b Image variants (tautan-be PR #15).**
  - **Encoding.** Every PNG, JPEG or WebP upload gets AVIF (q50) and WebP (q75) copies at each of 160/320/640/1080 that is narrower than the original. The encoders are WASM builds of libavif and libwebp (gen2brain/avif and gen2brain/webp on wazero), so there is no CGO and no system library. JPEG EXIF orientation is applied. GIFs (animation) and images over 40 MP get no variants. Encoding runs in one background worker after the upload returns, taking about 1–3 s per image. Unfinished images are re-queued on startup, and images uploaded before this change are backfilled the same way.
  - **Upload response.** `POST /uploads` for images now also returns `width` and `height`, the display size after EXIF rotation.
  - **Serving.** `GET /uploads/<name>?w=<N>` serves the smallest ready variant at least N wide. The format comes from `&fm=avif|webp` if given; otherwise it is negotiated from `Accept`, with AVIF preferred, then WebP, and the response sends `Vary: Accept`. It falls back to the original when N is at least the original width, when the browser accepts neither format, or when the variants aren't ready yet; that last case gets `Cache-Control: public, max-age=60`. Variants are also addressable directly as `/uploads/<base>.w320.avif`. Ready variants are `immutable`.
  - **Page reads.** `GET /public/{u}` and `GET /profile/editor` add `media: {"/uploads/<name>": {width, height, widths: [160, 320, …]}}` for every uploaded image the page references: avatar, theme and any block field, including absolute URLs. `widths` lists only the variants that are ready, and is `[]` until they are. To build `srcset`, look up `new URL(src).pathname` and use `src + "?w=" + w` for each entry in `widths`.
- **2c Revalidate webhook (tautan-be PR #16, aligned to the FE route in #17).** BE env: `REVALIDATE_SECRET` turns it on; it is off while empty. The URL comes from `REVALIDATE_URL`, or `FRONTEND_REVALIDATE_URL`, and defaults to `$APP_BASE_URL/api/revalidate`.
  - **When it fires.** After a 2xx from PATCH /profile, POST /import, any tab write (create, order, patch, delete) or any block write (create, order, patch, delete, restore), and only when the page was published before or after the change. Unpublishing therefore fires too. It also fires when an image's variants finish encoding, for the published pages that reference that image. Scheduled flips are left to the FE's 60 s ISR refresh.
  - **Delivery.** Calls are debounced over 750 ms, so a burst of edits sends one request. Each call carries at most 20 names, sorted. It is fire-and-forget with a 2 s timeout and no retries; failures are logged.
  - **Request:**
    ```
    POST /api/revalidate
    Content-Type: application/json
    X-Tautan-Timestamp: 1763300000
    X-Tautan-Signature: sha256=<lowercase hex HMAC-SHA256(REVALIDATE_SECRET, "<timestamp>.<raw body>")>
    {"usernames":["alice","alice2","alice3"]}
    ```
    On a rename, `usernames` includes the new name, the old one and every username that still redirects to the page.
  - **FE responses:** `200 {"revalidated":[...]}`, `400 invalid_body`, `401 bad_signature` (the signature is wrong or the timestamp is more than 5 min off), `503 not_configured`. The FE calls `revalidateTag("profile:<lowercased username>")` for each name.
- **2d Heavy demo page (tautan-be PR #18).**
  - **`make seed-heavy`** (re)creates `/heavy`. It mirrors the 13-block `/perfdemo` page from `tautan-fe docs/perf/baseline-2026-11-16.md`:
    - profile: 600x600 avatar, bio, theme `senja` (gradient + Outfit), 2 socials;
    - blocks: heading, 3 links, YouTube, a 1600x1600 image, discount code, countdown, FAQ×3, testimonials×2, a form with 3 fields, email signup, a mailto link;
    - plus the 9 P3-A sell cards (product, a 4-item shop, marketplace, whatsapp with an order form, price list, store info, payment with QRIS, tip jar, reviews).
  - **`make seed-heavy-all`** (re)creates `/heavy_all`: every block type, plus a folder with children, a second tab and an announcement.
  - **Details:**
    - Login password: `heavy-demo-password`.
    - Images are real JPEG uploads, and their AVIF/WebP variants are encoded before the command returns.
    - Re-running replaces the page, so every run measures identical content (the countdown always ends 30 days out).
    - Database: `DATABASE_URL` if set, otherwise the `make dev` database, which the seed starts if it isn't already running.
    - The seed prints the username and both URLs.
- **N4 Beacon-friendly events (tautan-be PR #19).** `POST /public/{u}/events` reads the body as JSON whatever the `Content-Type`. So `navigator.sendBeacon(url, JSON.stringify(evt))` works: the body is sent as `text/plain;charset=UTF-8`, a CORS simple request with no preflight. The response still carries `Access-Control-Allow-Origin` and `Allow-Credentials` for the FE origin, and the rate limit (120/min per IP+page) and bot filtering are unchanged. It is the only fire-and-forget public endpoint. `/contact`, `/form` and `/subscribe` return results the UI shows, so they stay as normal `fetch` calls with JSON (they also accept `text/plain`, but nothing relies on that).
