package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/aviandri/tautan-be/internal/mail"
	"github.com/aviandri/tautan-be/internal/payments"
)

// Payments (b): orders for priced files and checkout products. The buyer pays
// on the provider's hosted page (a redirect, no provider JS on our pages); the
// webhook or a status check on the return page marks the order paid; the
// order page then hands out signed, expiring download links.

const (
	orderTTL          = time.Hour          // how long the hosted checkout stays open
	downloadTTL       = 24 * time.Hour     // signed download links
	maxOrderDownloads = 30                 // per order, across its files
	reconcileEvery    = 10 * time.Second   // min gap between GetStatus calls for one order
	providerTimeout   = 10 * time.Second   // CreateCheckout / GetStatus
	webhookBodyLimit  = 1 << 20            // provider payloads are small
	orderGrace        = 7 * 24 * time.Hour // keep reconciling expired orders this long (late payments)
)

// orderItem is one line of an order. Download is the file delivered after
// payment; it is snapshotted so later edits to the card don't change what a
// buyer already paid for, and it is never sent to the browser.
type orderItem struct {
	BlockID    string        `json:"block_id"`
	ItemID     string        `json:"item_id,omitempty"`
	Kind       string        `json:"kind"` // file | product
	Name       string        `json:"name"`
	Variant    string        `json:"variant,omitempty"`
	UnitAmount int64         `json:"unit_amount"`
	Quantity   int           `json:"quantity"`
	Download   *DownloadFile `json:"download,omitempty"`
}

// orderItemView is what the API returns: the file name, never its URL.
type orderItemView struct {
	BlockID    string `json:"block_id"`
	ItemID     string `json:"item_id,omitempty"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Variant    string `json:"variant,omitempty"`
	UnitAmount int64  `json:"unit_amount"`
	Quantity   int    `json:"quantity"`
	FileName   string `json:"file_name,omitempty"`
}

type orderRow struct {
	ID, Code, ProfileID   string
	BuyerEmail, BuyerName string
	Items                 []orderItem
	Amount                int64
	Currency              string
	Provider, Mode        string
	Status                payments.Status
	ProviderRef           *string
	PaymentRef            *string
	CheckoutURL           *string
	Downloads             int
	ExpiresAt             time.Time
	PaidAt, RefundedAt    *time.Time
	LastCheckedAt         *time.Time
	CreatedAt             time.Time
}

const orderCols = `id, code, profile_id, buyer_email, buyer_name, items, amount, currency, provider, mode, status,
	provider_ref, payment_ref, checkout_url, downloads, expires_at, paid_at, refunded_at, last_checked_at, created_at`

func scanOrder(row pgx.Row) (orderRow, error) {
	var o orderRow
	err := row.Scan(&o.ID, &o.Code, &o.ProfileID, &o.BuyerEmail, &o.BuyerName, &o.Items, &o.Amount, &o.Currency,
		&o.Provider, &o.Mode, &o.Status, &o.ProviderRef, &o.PaymentRef, &o.CheckoutURL, &o.Downloads,
		&o.ExpiresAt, &o.PaidAt, &o.RefundedAt, &o.LastCheckedAt, &o.CreatedAt)
	return o, err
}

// Order is the owner's view of an order.
type Order struct {
	ID          string          `json:"id"`
	Code        string          `json:"code"`
	Status      payments.Status `json:"status"`
	BuyerEmail  string          `json:"buyer_email"`
	BuyerName   string          `json:"buyer_name"`
	Items       []orderItemView `json:"items"`
	Amount      int64           `json:"amount"`
	Currency    string          `json:"currency"`
	Provider    string          `json:"provider"`
	Mode        string          `json:"mode"`
	ProviderRef *string         `json:"provider_ref"`
	Downloads   int             `json:"downloads"`
	CreatedAt   time.Time       `json:"created_at"`
	ExpiresAt   time.Time       `json:"expires_at"`
	PaidAt      *time.Time      `json:"paid_at"`
	RefundedAt  *time.Time      `json:"refunded_at"`
}

func (o orderRow) itemViews() []orderItemView {
	out := make([]orderItemView, 0, len(o.Items))
	for _, it := range o.Items {
		v := orderItemView{BlockID: it.BlockID, ItemID: it.ItemID, Kind: it.Kind, Name: it.Name, Variant: it.Variant,
			UnitAmount: it.UnitAmount, Quantity: it.Quantity}
		if it.Download != nil {
			v.FileName = downloadName(*it.Download)
		}
		out = append(out, v)
	}
	return out
}

func (o orderRow) view() Order {
	return Order{ID: o.ID, Code: o.Code, Status: o.Status, BuyerEmail: o.BuyerEmail, BuyerName: o.BuyerName,
		Items: o.itemViews(), Amount: o.Amount, Currency: o.Currency, Provider: o.Provider, Mode: o.Mode,
		ProviderRef: o.ProviderRef, Downloads: o.Downloads, CreatedAt: o.CreatedAt, ExpiresAt: o.ExpiresAt,
		PaidAt: o.PaidAt, RefundedAt: o.RefundedAt}
}

func downloadName(d DownloadFile) string {
	if d.FileName != "" {
		return d.FileName
	}
	return path.Base(strings.SplitN(d.URL, "?", 2)[0])
}

// ---------- codes and tokens ----------

const codeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" // Crockford base32: no I, L, O, U

// newOrderCode returns a code like K7Q2-M9XD (40 random bits).
func newOrderCode() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	out := make([]byte, 0, 9)
	for i, c := range b {
		if i == 4 {
			out = append(out, '-')
		}
		out = append(out, codeAlphabet[int(c)%32])
	}
	return string(out)
}

// normCode accepts what a buyer types: any case, spaces, missing dash, and
// the look-alikes Crockford maps (O→0, I/L→1).
func normCode(s string) string {
	r := strings.NewReplacer("-", "", " ", "", "O", "0", "I", "1", "L", "1")
	c := r.Replace(strings.ToUpper(strings.TrimSpace(s)))
	if len(c) != 8 {
		return ""
	}
	return c[:4] + "-" + c[4:]
}

// orderToken lets the buyer open their order page without an account.
func (s *Server) orderToken(id string) string { return s.sign("order", id) }

func (s *Server) validOrderToken(id, tok string) bool {
	return tok != "" && hmac.Equal([]byte(tok), []byte(s.orderToken(id)))
}

func (s *Server) orderPageURL(username, id string) string {
	return s.cfg.AppBaseURL + "/" + username + "/orders/" + id + "?t=" + s.orderToken(id)
}

// downloadToken is "<exp>.<order id>.<index>.<sig>".
func (s *Server) downloadToken(orderID string, index int, exp time.Time) string {
	payload := strconv.FormatInt(exp.Unix(), 10) + "." + orderID + "." + strconv.Itoa(index)
	return payload + "." + s.sign("order-dl", payload)
}

// ---------- providers available to a page ----------

type pageProvider struct {
	Provider   string   `json:"provider"`
	Currencies []string `json:"currencies"`
}

// pagePayments is `payments` in page reads: the providers buyers can pay with.
type pagePayments struct {
	Providers  []pageProvider `json:"providers"`
	Currencies []string       `json:"currencies"`
}

// enabledProviders lists the page's enabled providers in display order.
func (s *Server) enabledProviders(ctx context.Context, pid string) ([]string, error) {
	if s.sealer == nil {
		return nil, nil
	}
	rows, err := s.db.Query(ctx, `SELECT provider FROM payment_providers WHERE profile_id = $1 AND enabled`, pid)
	if err != nil {
		return nil, err
	}
	have, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range payments.Names {
		for _, h := range have {
			if h == n {
				out = append(out, n)
			}
		}
	}
	return out, nil
}

func (s *Server) pagePayments(ctx context.Context, pid string) (*pagePayments, error) {
	names, err := s.enabledProviders(ctx, pid)
	if err != nil || len(names) == 0 {
		return nil, err
	}
	pp := &pagePayments{}
	seen := map[string]bool{}
	for _, n := range names {
		pp.Providers = append(pp.Providers, pageProvider{Provider: n, Currencies: payments.Currencies(n)})
		for _, c := range payments.Currencies(n) {
			seen[c] = true
		}
	}
	for _, c := range []string{"IDR", "USD"} {
		if seen[c] {
			pp.Currencies = append(pp.Currencies, c)
		}
	}
	return pp, nil
}

// connectedProvider returns the adapter for a page's provider, or nil when it
// isn't connected (or its keys can't be read).
func (s *Server) connectedProvider(ctx context.Context, pid, name string) (payments.Provider, *providerRow, error) {
	if s.sealer == nil {
		return nil, nil, nil
	}
	row, err := s.loadProviderRow(ctx, s.db, pid, name, false)
	if err != nil || row == nil {
		return nil, nil, err
	}
	creds, err := s.openCreds(pid, name, row)
	if err != nil {
		slog.Warn("payments: stored credentials unreadable", "profile_id", pid, "provider", name, "err", err)
		return nil, nil, nil
	}
	p, err := s.provider(name, row.mode, creds)
	if err != nil {
		return nil, nil, err
	}
	return p, row, nil
}

// ---------- public reads: never leak what is sold ----------

// stripPaidContent removes the deliverable from priced cards in public reads:
// a priced file's url and a checkout product's download (shop items too).
func stripPaidContent(b *Block) {
	if b.Data == nil {
		return
	}
	var m map[string]any
	switch b.Type {
	case "file", "product", "shop":
		if json.Unmarshal(b.Data, &m) != nil {
			return
		}
	default:
		return
	}
	changed := false
	stripProduct := func(p map[string]any) {
		if a, ok := p["action"].(map[string]any); ok {
			if _, has := a["download"]; has {
				delete(a, "download")
				changed = true
			}
		}
	}
	switch b.Type {
	case "file":
		if m["price"] != nil {
			m["url"] = ""
			changed = true
		}
	case "product":
		stripProduct(m)
	case "shop":
		if items, ok := m["items"].([]any); ok {
			for _, it := range items {
				if p, ok := it.(map[string]any); ok {
					stripProduct(p)
				}
			}
		}
	}
	if changed {
		if out, err := json.Marshal(m); err == nil {
			b.Data = out
		}
	}
}

// ---------- what can be bought ----------

type purchase struct {
	item     orderItem
	currency string
}

var errNotForSale = errors.New("not for sale")

// purchasable resolves the price and deliverable from the stored card. The
// browser only says which card (and shop item / options); prices come from here.
func purchasable(b Block, itemID string, options map[string]string) (purchase, error) {
	switch b.Type {
	case "file":
		var d FileData
		if json.Unmarshal(b.Data, &d) != nil || d.Price == nil || d.URL == "" {
			return purchase{}, errNotForSale
		}
		return purchase{
			item: orderItem{BlockID: b.ID, Kind: "file", Name: d.Title, UnitAmount: *d.Price, Quantity: 1,
				Download: &DownloadFile{URL: d.URL, FileName: d.FileName}},
			currency: d.Currency,
		}, nil
	case "product", "shop":
		var p ProductData
		if b.Type == "product" {
			if json.Unmarshal(b.Data, &p) != nil {
				return purchase{}, errNotForSale
			}
		} else {
			var shop ShopData
			if json.Unmarshal(b.Data, &shop) != nil {
				return purchase{}, errNotForSale
			}
			found := false
			for _, it := range shop.Items {
				if it.ID != "" && it.ID == itemID {
					p, found = it, true
					break
				}
			}
			if !found {
				return purchase{}, errNotForSale
			}
		}
		if p.Action.Kind != "checkout" || p.Price == nil || p.Action.Download == nil || p.Badge == "sold_out" {
			return purchase{}, errNotForSale
		}
		variant, err := pickVariant(p.Variants, options)
		if err != nil {
			return purchase{}, err
		}
		it := orderItem{BlockID: b.ID, Kind: "product", Name: p.Name, Variant: variant, UnitAmount: *p.Price,
			Quantity: 1, Download: p.Action.Download}
		if b.Type == "shop" {
			it.ItemID = itemID
		}
		return purchase{item: it, currency: p.Currency}, nil
	}
	return purchase{}, errNotForSale
}

type variantError struct{ msg string }

func (e variantError) Error() string { return e.msg }

// pickVariant checks the buyer's choice for every variant group and returns it
// as "Size: M, Colour: Red".
func pickVariant(groups []ProductVariant, chosen map[string]string) (string, error) {
	var parts []string
	for _, g := range groups {
		v, ok := chosen[g.Name]
		if !ok || v == "" {
			return "", variantError{"choose a " + strings.ToLower(g.Name)}
		}
		valid := false
		for _, o := range g.Options {
			if o == v {
				valid = true
				break
			}
		}
		if !valid {
			return "", variantError{fmt.Sprintf("%q isn't an option for %s", v, g.Name)}
		}
		parts = append(parts, g.Name+": "+v)
	}
	return strings.Join(parts, ", "), nil
}

// ---------- POST /public/{username}/orders ----------

type createOrderIn struct {
	BlockID  string            `json:"block_id"`
	ItemID   string            `json:"item_id"`
	Options  map[string]string `json:"options"`
	Email    string            `json:"email"`
	Name     string            `json:"name"`
	Provider string            `json:"provider"`
}

func (s *Server) handleCreateOrder(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	var in createOrderIn
	if !decodeJSON(w, r, &in) {
		return
	}
	in.Email, in.Name = strings.TrimSpace(in.Email), strings.TrimSpace(in.Name)
	if !validEmail(in.Email) {
		writeErrorDetails(w, http.StatusBadRequest, "invalid_email", "enter a valid email address", map[string]string{"email": "enter a valid email address"})
		return
	}
	if len([]rune(in.Name)) > 100 {
		writeErrorDetails(w, http.StatusBadRequest, "invalid_name", "name is too long", map[string]string{"name": "at most 100 characters"})
		return
	}
	ctx := r.Context()
	username := chi.URLParam(r, "username")
	pid, err := s.publishedProfileID(ctx, username)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "page not found")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	b, err := scanBlock(s.db.QueryRow(ctx, `SELECT `+blockCols+` FROM blocks b
		WHERE b.id = $1 AND b.profile_id = $2 AND b.is_visible AND b.deleted_at IS NULL`, in.BlockID, pid))
	if err == nil && !blockSchedule(b.Data).activeAt(s.now()) {
		err = pgx.ErrNoRows
	}
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		writeError(w, http.StatusNotFound, "not_found", "card not found on this page")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	if !s.gateOK(w, r, b) {
		return
	}
	buy, err := purchasable(b, in.ItemID, in.Options)
	var ve variantError
	if errors.As(err, &ve) {
		writeErrorDetails(w, http.StatusBadRequest, "invalid_options", ve.msg, map[string]string{"options": ve.msg})
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "not_for_sale", "this card can't be bought")
		return
	}
	if !s.orderLimiter.Allow(clientIP(r) + "|" + pid) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many checkouts, try again in a few minutes")
		return
	}

	// Pick the provider: the buyer's choice, or the only one that fits.
	names, err := s.enabledProviders(ctx, pid)
	if err != nil {
		s.internalError(w, err)
		return
	}
	var fit []string
	for _, n := range names {
		if payments.Supports(n, buy.currency) {
			fit = append(fit, n)
		}
	}
	provider := in.Provider
	switch {
	case len(fit) == 0:
		writeError(w, http.StatusConflict, "payments_unavailable", "this seller isn't taking payments in "+buy.currency+" right now")
		return
	case provider == "" && len(fit) == 1:
		provider = fit[0]
	case provider == "":
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":     apiError{Code: "provider_required", Message: "choose how to pay"},
			"providers": fit,
		})
		return
	}
	ok := false
	for _, n := range fit {
		ok = ok || n == provider
	}
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_provider", "that payment option isn't available for this item")
		return
	}
	p, row, err := s.connectedProvider(ctx, pid, provider)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if p == nil {
		writeError(w, http.StatusConflict, "payments_unavailable", "this seller isn't taking payments right now")
		return
	}

	now := s.now()
	amount := buy.item.UnitAmount * int64(buy.item.Quantity)
	var o orderRow
	for attempt := 0; ; attempt++ {
		o, err = scanOrder(s.db.QueryRow(ctx, `INSERT INTO orders
			(code, profile_id, buyer_email, buyer_name, items, amount, currency, provider, mode, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING `+orderCols,
			newOrderCode(), pid, strings.ToLower(in.Email), in.Name, []orderItem{buy.item}, amount, buy.currency,
			provider, row.mode, now.Add(orderTTL)))
		if err == nil || attempt == 3 || !strings.Contains(err.Error(), "orders_code_key") {
			break
		}
	}
	if err != nil {
		s.internalError(w, err)
		return
	}

	desc := buy.item.Name
	if buy.item.Variant != "" {
		desc += " (" + buy.item.Variant + ")"
	}
	cctx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()
	sess, err := p.CreateCheckout(cctx, payments.Checkout{
		OrderID:     o.ID,
		Amount:      amount,
		Currency:    buy.currency,
		Description: desc + " · order " + o.Code,
		Items:       []payments.Item{{Name: desc, UnitAmount: buy.item.UnitAmount, Quantity: buy.item.Quantity}},
		BuyerEmail:  o.BuyerEmail,
		BuyerName:   o.BuyerName,
		SuccessURL:  s.orderPageURL(username, o.ID),
		CancelURL:   s.orderPageURL(username, o.ID) + "&cancelled=1",
		NotifyURL:   s.webhookURL(provider, pid),
		ExpiresAt:   o.ExpiresAt,
	})
	if err != nil {
		var pe *payments.Error
		code, msg := "provider_error", err.Error()
		if errors.As(err, &pe) {
			code, msg = pe.Code, pe.Message
		}
		slog.Warn("payments: create checkout failed", "profile_id", pid, "provider", provider, "order_id", o.ID, "code", code, "err", msg)
		_, _ = s.db.Exec(ctx, `UPDATE orders SET status = 'failed', updated_at = now() WHERE id = $1`, o.ID)
		// Tell the creator in Settings; the buyer gets a neutral message.
		_, _ = s.db.Exec(ctx, `UPDATE payment_providers SET last_error = $3 WHERE profile_id = $1 AND provider = $2`,
			pid, provider, providerError{Code: code, Message: "checkout failed: " + msg})
		writeError(w, http.StatusBadGateway, "checkout_failed", "the payment page couldn't be opened; try again or pick another way to pay")
		return
	}
	if _, err := s.db.Exec(ctx, `UPDATE orders SET provider_ref = $2, checkout_url = $3, expires_at = $4, updated_at = now() WHERE id = $1`,
		o.ID, sess.Ref, sess.URL, sess.ExpiresAt); err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"order_id":     o.ID,
		"code":         o.Code,
		"token":        s.orderToken(o.ID),
		"checkout_url": sess.URL,
		"expires_at":   sess.ExpiresAt,
	})
}

// ---------- GET /public/{username}/orders/{id}?t= ----------

type publicDownload struct {
	Index     int       `json:"index"`
	Name      string    `json:"name"`
	FileName  string    `json:"file_name"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// buyerOrder loads an order for a buyer request (token-checked, page-scoped).
func (s *Server) buyerOrder(w http.ResponseWriter, r *http.Request) (orderRow, string, bool) {
	w.Header().Set("Cache-Control", "private, no-store")
	ctx := r.Context()
	username := chi.URLParam(r, "username")
	id := chi.URLParam(r, "id")
	if !s.validOrderToken(id, r.URL.Query().Get("t")) {
		writeError(w, http.StatusNotFound, "not_found", "order not found")
		return orderRow{}, "", false
	}
	pid, err := s.publishedProfileID(ctx, username)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "order not found")
		return orderRow{}, "", false
	}
	if err != nil {
		s.internalError(w, err)
		return orderRow{}, "", false
	}
	o, err := scanOrder(s.db.QueryRow(ctx, `SELECT `+orderCols+` FROM orders WHERE id = $1 AND profile_id = $2`, id, pid))
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		writeError(w, http.StatusNotFound, "not_found", "order not found")
		return orderRow{}, "", false
	}
	if err != nil {
		s.internalError(w, err)
		return orderRow{}, "", false
	}
	return o, username, true
}

func (s *Server) handlePublicOrder(w http.ResponseWriter, r *http.Request) {
	o, username, ok := s.buyerOrder(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	// The webhook may be late (or not set up): ask the provider directly.
	if s.shouldReconcile(o) {
		if updated, err := s.reconcile(ctx, o); err == nil {
			o = updated
		}
	}
	s.writeBuyerOrder(w, o, username)
}

func (s *Server) writeBuyerOrder(w http.ResponseWriter, o orderRow, username string) {
	now := s.now()
	view := map[string]any{
		"id": o.ID, "code": o.Code, "status": o.Status, "amount": o.Amount, "currency": o.Currency,
		"items": o.itemViews(), "buyer_email": o.BuyerEmail, "buyer_name": o.BuyerName, "provider": o.Provider,
		"mode": o.Mode, "created_at": o.CreatedAt, "paid_at": o.PaidAt, "expires_at": o.ExpiresAt, "checkout_url": nil,
	}
	if o.Status == payments.StatusPending && o.CheckoutURL != nil && now.Before(o.ExpiresAt) {
		view["checkout_url"] = *o.CheckoutURL
	}
	downloads := []publicDownload{}
	if o.Status == payments.StatusPaid {
		exp := now.Add(downloadTTL).Truncate(time.Second)
		for i, it := range o.Items {
			if it.Download == nil {
				continue
			}
			downloads = append(downloads, publicDownload{
				Index: i, Name: it.Name, FileName: downloadName(*it.Download), ExpiresAt: exp,
				URL: s.cfg.PublicBaseURL + "/api/v1/public/" + username + "/orders/download/" + s.downloadToken(o.ID, i, exp),
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"order": view, "downloads": downloads})
}

func (s *Server) shouldReconcile(o orderRow) bool {
	now := s.now()
	switch o.Status {
	case payments.StatusPending:
	case payments.StatusExpired, payments.StatusFailed:
		// A late payment can still land on an expired invoice for a while.
		if now.Sub(o.ExpiresAt) > orderGrace {
			return false
		}
	default:
		return false
	}
	if o.ProviderRef == nil {
		return false
	}
	return o.LastCheckedAt == nil || now.Sub(*o.LastCheckedAt) >= reconcileEvery
}

// reconcile asks the provider for the order's status and applies it.
func (s *Server) reconcile(ctx context.Context, o orderRow) (orderRow, error) {
	if _, err := s.db.Exec(ctx, `UPDATE orders SET last_checked_at = now() WHERE id = $1`, o.ID); err != nil {
		return o, err
	}
	p, row, err := s.connectedProvider(ctx, o.ProfileID, o.Provider)
	if err != nil || p == nil || row.mode != o.Mode || o.ProviderRef == nil {
		return o, err
	}
	cctx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()
	ev, err := p.GetStatus(cctx, *o.ProviderRef)
	if err != nil {
		return o, err
	}
	// A pending order whose checkout has expired at the provider stays pending
	// until the provider says so; status checks only move forward.
	return s.applyEvent(ctx, o.ID, ev, "")
}

// ---------- POST /public/{username}/orders/lookup ----------

func (s *Server) handleOrderLookup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	var in struct {
		Email string `json:"email"`
		Code  string `json:"code"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if !s.lookupLimiter.Allow(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many lookups, try again in a few minutes")
		return
	}
	code := normCode(in.Code)
	if code == "" || !validEmail(strings.TrimSpace(in.Email)) {
		writeError(w, http.StatusNotFound, "not_found", "no order matches that email and code")
		return
	}
	ctx := r.Context()
	pid, err := s.publishedProfileID(ctx, chi.URLParam(r, "username"))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "no order matches that email and code")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	var id string
	err = s.db.QueryRow(ctx, `SELECT id FROM orders WHERE profile_id = $1 AND code = $2 AND buyer_email = $3`,
		pid, code, strings.ToLower(strings.TrimSpace(in.Email))).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "no order matches that email and code")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"order_id": id, "token": s.orderToken(id)})
}

// ---------- GET /public/{username}/orders/download/{token} ----------

func (s *Server) handleOrderDownload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	parts := strings.Split(chi.URLParam(r, "token"), ".")
	if len(parts) != 4 {
		writeError(w, http.StatusForbidden, "invalid_token", "download link is invalid")
		return
	}
	payload := strings.Join(parts[:3], ".")
	if !hmac.Equal([]byte(parts[3]), []byte(s.sign("order-dl", payload))) {
		writeError(w, http.StatusForbidden, "invalid_token", "download link is invalid")
		return
	}
	exp, err1 := strconv.ParseInt(parts[0], 10, 64)
	index, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil {
		writeError(w, http.StatusForbidden, "invalid_token", "download link is invalid")
		return
	}
	if s.now().Unix() > exp {
		writeError(w, http.StatusGone, "expired", "this download link has expired; open your order page for a fresh one")
		return
	}
	ctx := r.Context()
	pid, err := s.publishedProfileID(ctx, chi.URLParam(r, "username"))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "file not found")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	// A fresh GET counts as a download; HEAD and resumed or partial Range
	// requests (download managers, video seeking) don't. Counting and the
	// paid/limit check happen in one statement.
	inc := 0
	if r.Method == http.MethodGet && countsAsDownload(r.Header.Get("Range")) {
		inc = 1
	}
	var items []orderItem
	err = s.db.QueryRow(ctx, `UPDATE orders SET downloads = downloads + $4
		WHERE id = $1 AND profile_id = $2 AND status = 'paid' AND downloads < $3 RETURNING items`,
		parts[1], pid, maxOrderDownloads, inc).Scan(&items)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		var status string
		var n int
		if e := s.db.QueryRow(ctx, `SELECT status, downloads FROM orders WHERE id = $1 AND profile_id = $2`, parts[1], pid).Scan(&status, &n); e == nil {
			if status == "paid" && n >= maxOrderDownloads {
				writeError(w, http.StatusTooManyRequests, "download_limit", "this order has reached its download limit; contact the seller")
				return
			}
			writeError(w, http.StatusForbidden, "not_paid", "this order isn't paid")
			return
		}
		writeError(w, http.StatusNotFound, "not_found", "file not found")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	if index < 0 || index >= len(items) || items[index].Download == nil {
		writeError(w, http.StatusNotFound, "not_found", "file not found")
		return
	}
	s.serveDownload(w, r, *items[index].Download)
}

// countsAsDownload: no Range, or a Range starting at byte 0.
func countsAsDownload(rng string) bool {
	rng = strings.TrimSpace(rng)
	return rng == "" || strings.HasPrefix(rng, "bytes=0-")
}

var unsafeFileName = regexp.MustCompile(`[^\w.\- ()]+`)

// serveDownload streams our own uploads as an attachment and redirects to
// external URLs.
func (s *Server) serveDownload(w http.ResponseWriter, r *http.Request, d DownloadFile) {
	local := strings.TrimPrefix(d.URL, s.cfg.PublicBaseURL)
	if !strings.HasPrefix(local, "/uploads/") {
		http.Redirect(w, r, d.URL, http.StatusFound)
		return
	}
	name := path.Base(strings.SplitN(local, "?", 2)[0])
	f, err := s.uploads().Open(r.Context(), name)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "file not found")
		return
	}
	defer f.Close()
	dl := unsafeFileName.ReplaceAllString(downloadName(d), "_")
	if dl == "" {
		dl = name
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+dl+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, name, f.ModTime(), f)
}

// ---------- POST /webhooks/{provider}/{profile_id} ----------

func (s *Server) handlePaymentWebhook(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "provider")
	pid := chi.URLParam(r, "profile_id")
	if !payments.Valid(name) {
		writeError(w, http.StatusNotFound, "not_found", "unknown provider")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, webhookBodyLimit))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "payload too large")
		return
	}
	ctx := r.Context()
	p, row, err := s.connectedProvider(ctx, pid, name)
	if isInvalidUUID(err) {
		err, p = nil, nil
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "not_configured", "this provider isn't connected for this page")
		return
	}
	ev, err := p.VerifyWebhook(r, body)
	if errors.Is(err, payments.ErrBadSignature) {
		slog.Warn("payments: webhook signature rejected", "profile_id", pid, "provider", name)
		writeError(w, http.StatusUnauthorized, "bad_signature", "webhook signature is invalid")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_payload", "unrecognised webhook payload")
		return
	}
	if ev.Live != nil && *ev.Live != (row.mode == payments.Live) {
		// e.g. a Stripe test event while the page is in live mode.
		writeJSON(w, http.StatusOK, map[string]string{"result": "ignored", "reason": "mode_mismatch"})
		return
	}
	if ev.Confirm {
		// The payload's status isn't authenticated (Midtrans, Xendit): treat the
		// webhook as a trigger and act on what the provider's API says, with the
		// creator's own key.
		cctx, cancel := context.WithTimeout(ctx, providerTimeout)
		st, err := p.GetStatus(cctx, ev.Ref)
		cancel()
		if errors.Is(err, payments.ErrNotFound) {
			writeJSON(w, http.StatusOK, map[string]string{"result": "unknown_order"})
			return
		}
		if err != nil {
			// 502: the provider retries the notification later.
			writeError(w, http.StatusBadGateway, "provider_unreachable", "couldn't confirm the payment status with the provider")
			return
		}
		if st.OrderID == "" {
			st.OrderID = ev.OrderID
		}
		// The key is made of what the provider confirmed, so a later, different
		// confirmed state (e.g. the amount now reported) isn't a duplicate.
		st.Key = fmt.Sprintf("%s:%s:%s:%d%s", ev.Key, st.Type, st.Status, st.Amount, st.Currency)
		ev = st
	}

	var result string
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO payment_events (profile_id, provider, event_key, type, status)
			VALUES ($1, $2, $3, $4, $5) ON CONFLICT (profile_id, provider, event_key) DO NOTHING`,
			pid, name, ev.Key, ev.Type, string(ev.Status))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			result = "duplicate"
			return nil
		}
		id, err := s.findOrder(ctx, tx, pid, name, ev)
		if errors.Is(err, pgx.ErrNoRows) {
			result = "unknown_order"
			_, err = tx.Exec(ctx, `UPDATE payment_events SET status = 'ignored' WHERE profile_id = $1 AND provider = $2 AND event_key = $3`, pid, name, ev.Key)
			return err
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_events SET order_id = $4 WHERE profile_id = $1 AND provider = $2 AND event_key = $3`,
			pid, name, ev.Key, id); err != nil {
			return err
		}
		o, err := s.applyEventTx(ctx, tx, id, ev, ev.Key)
		result = string(o.Status)
		return err
	})
	if err != nil {
		s.internalError(w, err) // 500: the provider retries, and the event isn't recorded
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"result": result})
}

func (s *Server) findOrder(ctx context.Context, tx pgx.Tx, pid, provider string, ev payments.Event) (string, error) {
	var id string
	if ev.OrderID != "" {
		err := tx.QueryRow(ctx, `SELECT id FROM orders WHERE id::text = $1 AND profile_id = $2 AND provider = $3`, ev.OrderID, pid, provider).Scan(&id)
		if err == nil || !errors.Is(err, pgx.ErrNoRows) {
			return id, err
		}
	}
	if ev.Ref != "" {
		err := tx.QueryRow(ctx, `SELECT id FROM orders WHERE provider = $1 AND provider_ref = $2 AND profile_id = $3`, provider, ev.Ref, pid).Scan(&id)
		if err == nil || !errors.Is(err, pgx.ErrNoRows) {
			return id, err
		}
	}
	if ev.PaymentRef != "" {
		return id, tx.QueryRow(ctx, `SELECT id FROM orders WHERE provider = $1 AND payment_ref = $2 AND profile_id = $3`, provider, ev.PaymentRef, pid).Scan(&id)
	}
	return "", pgx.ErrNoRows
}

// ---------- status transitions ----------

// nextStatus says whether an order may move from cur to want. Status only
// moves forward; a late payment beats an expiry or failure.
func nextStatus(cur, want payments.Status) bool {
	if want == "" || want == cur {
		return false
	}
	switch cur {
	case payments.StatusPending:
		return want != payments.StatusRefunded
	case payments.StatusExpired, payments.StatusFailed:
		return want == payments.StatusPaid
	case payments.StatusPaid:
		return want == payments.StatusRefunded
	}
	return false
}

func (s *Server) applyEvent(ctx context.Context, orderID string, ev payments.Event, eventKey string) (orderRow, error) {
	var o orderRow
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		o, err = s.applyEventTx(ctx, tx, orderID, ev, eventKey)
		return err
	})
	return o, err
}

// applyEventTx moves an order to the event's status when allowed, records the
// provider refs, and runs the paid side effects exactly once.
func (s *Server) applyEventTx(ctx context.Context, tx pgx.Tx, orderID string, ev payments.Event, eventKey string) (orderRow, error) {
	o, err := scanOrder(tx.QueryRow(ctx, `SELECT `+orderCols+` FROM orders WHERE id = $1 FOR UPDATE`, orderID))
	if err != nil {
		return o, err
	}
	if ev.Ref != "" && o.ProviderRef == nil {
		o.ProviderRef = &ev.Ref
	}
	if ev.PaymentRef != "" {
		o.PaymentRef = &ev.PaymentRef
	}
	want := ev.Status
	// Never paid without a provider-reported amount and currency that match;
	// unknown counts as a mismatch (the order stays pending for reconcile).
	if want == payments.StatusPaid && (ev.Amount != o.Amount || ev.Currency != o.Currency) {
		slog.Error("payments: paid amount doesn't match the order", "order_id", o.ID, "provider", o.Provider,
			"want_amount", o.Amount, "want_currency", o.Currency, "got_amount", ev.Amount, "got_currency", ev.Currency)
		want = ""
		if eventKey != "" {
			if _, err := tx.Exec(ctx, `UPDATE payment_events SET status = 'amount_mismatch' WHERE profile_id = $1 AND provider = $2 AND event_key = $3`,
				o.ProfileID, o.Provider, eventKey); err != nil {
				return o, err
			}
		}
	}
	now := s.now()
	moved := nextStatus(o.Status, want)
	if moved {
		o.Status = want
		switch want {
		case payments.StatusPaid:
			o.PaidAt = &now
		case payments.StatusRefunded:
			o.RefundedAt = &now
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE orders SET status = $2, provider_ref = $3, payment_ref = $4, paid_at = $5,
		refunded_at = $6, updated_at = now() WHERE id = $1`,
		o.ID, o.Status, o.ProviderRef, o.PaymentRef, o.PaidAt, o.RefundedAt); err != nil {
		return o, err
	}
	if moved && want == payments.StatusPaid {
		// Paid buyers join Audience (not subscribed), sourced from the card they bought.
		blockID := ""
		if len(o.Items) > 0 {
			blockID = o.Items[0].BlockID
		}
		cid, err := upsertContact(ctx, tx, o.ProfileID, o.BuyerEmail, o.BuyerName, false, blockID, now)
		if err != nil {
			return o, err
		}
		if _, err := tx.Exec(ctx, `UPDATE orders SET contact_id = $2 WHERE id = $1`, o.ID, cid); err != nil {
			return o, err
		}
		// The receipt goes out only if this transaction commits. Capped per
		// page and per recipient: a seller faking their own webhooks must not
		// turn Tautan into a mailer.
		if s.receiptPageLimiter.Allow(o.ProfileID) && s.receiptToLimiter.Allow(o.ProfileID+"|"+o.BuyerEmail) {
			if err := s.enqueueReceipt(ctx, tx, o); err != nil {
				return o, err
			}
		} else {
			slog.Warn("payments: receipt not sent (rate limit)", "profile_id", o.ProfileID, "order_id", o.ID)
		}
	}
	return o, nil
}

func (s *Server) enqueueReceipt(ctx context.Context, tx pgx.Tx, o orderRow) error {
	var username, display string
	if err := tx.QueryRow(ctx, `SELECT username, display_name FROM profiles WHERE id = $1`, o.ProfileID).Scan(&username, &display); err != nil {
		return err
	}
	seller := strings.TrimSpace(display)
	if seller == "" {
		seller = "@" + username
	}
	r := mail.Receipt{Seller: seller, Code: o.Code, Total: mail.Money(o.Amount, o.Currency), Link: s.orderPageURL(username, o.ID)}
	for _, it := range o.Items {
		n := it.Name
		if it.Variant != "" {
			n += " (" + it.Variant + ")"
		}
		r.Lines = append(r.Lines, mail.ReceiptLine{Name: n, Quantity: it.Quantity, Amount: mail.Money(it.UnitAmount*int64(it.Quantity), o.Currency)})
	}
	return s.enqueueEmail(ctx, tx, "order_receipt", mail.OrderReceipt(o.BuyerEmail, r))
}

// ---------- owner: GET /orders, /orders/{id}, /orders.csv, POST /orders/{id}/refresh ----------

var orderStatuses = []string{"pending", "paid", "expired", "failed", "refunded"}

func orderFilter(r *http.Request, pid string) (string, []any, error) {
	where := []string{"profile_id = $1"}
	args := []any{pid}
	if st := r.URL.Query().Get("status"); st != "" && st != "all" {
		ok := false
		for _, v := range orderStatuses {
			ok = ok || v == st
		}
		if !ok {
			return "", nil, errors.New("status must be all, pending, paid, expired, failed or refunded")
		}
		args = append(args, st)
		where = append(where, fmt.Sprintf("status = $%d", len(args)))
	}
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		args = append(args, "%"+strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(q)+"%")
		n := len(args)
		where = append(where, fmt.Sprintf("(buyer_email ILIKE $%d OR buyer_name ILIKE $%d OR code ILIKE $%d)", n, n, n))
	}
	return strings.Join(where, " AND "), args, nil
}

func (s *Server) handleListOrders(w http.ResponseWriter, r *http.Request) {
	pid := profileID(r)
	ctx := r.Context()
	where, args, err := orderFilter(r, pid)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_status", err.Error())
		return
	}
	cond := where
	if c := r.URL.Query().Get("cursor"); c != "" {
		t, id, ok := decodeCursor(c)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "cursor is invalid")
			return
		}
		args = append(args, t, id)
		cond += fmt.Sprintf(" AND (created_at, id) < ($%d, $%d::uuid)", len(args)-1, len(args))
	}
	rows, err := s.db.Query(ctx, `SELECT `+orderCols+` FROM orders WHERE `+cond+` ORDER BY created_at DESC, id DESC LIMIT 21`, args...)
	if err != nil {
		s.internalError(w, err)
		return
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (orderRow, error) { return scanOrder(r) })
	if err != nil {
		s.internalError(w, err)
		return
	}
	var next *string
	if len(list) > 20 {
		list = list[:20]
		c := encodeCursor(list[19].CreatedAt, list[19].ID)
		next = &c
	}
	items := make([]Order, 0, len(list))
	for _, o := range list {
		items = append(items, o.view())
	}
	counts := map[string]int{"all": 0}
	for _, st := range orderStatuses {
		counts[st] = 0
	}
	// Counts follow the search but not the status filter (for the tabs).
	qr := r.Clone(ctx)
	qv := qr.URL.Query()
	qv.Del("status")
	qr.URL.RawQuery = qv.Encode()
	cw, cargs, _ := orderFilter(qr, pid)
	crows, err := s.db.Query(ctx, `SELECT status, count(*) FROM orders WHERE `+cw+` GROUP BY status`, cargs...)
	if err != nil {
		s.internalError(w, err)
		return
	}
	defer crows.Close()
	for crows.Next() {
		var st string
		var n int
		if err := crows.Scan(&st, &n); err != nil {
			s.internalError(w, err)
			return
		}
		counts[st] = n
		counts["all"] += n
	}
	if err := crows.Err(); err != nil {
		s.internalError(w, err)
		return
	}
	// Paid revenue per currency, for the page header (refunds excluded).
	revenue := map[string]int64{}
	rrows, err := s.db.Query(ctx, `SELECT currency, sum(amount) FROM orders WHERE profile_id = $1 AND status = 'paid' GROUP BY currency`, pid)
	if err != nil {
		s.internalError(w, err)
		return
	}
	defer rrows.Close()
	for rrows.Next() {
		var c string
		var n int64
		if err := rrows.Scan(&c, &n); err != nil {
			s.internalError(w, err)
			return
		}
		revenue[c] = n
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next, "counts": counts, "revenue": revenue})
}

func (s *Server) ownerOrder(w http.ResponseWriter, r *http.Request) (orderRow, bool) {
	o, err := scanOrder(s.db.QueryRow(r.Context(), `SELECT `+orderCols+` FROM orders WHERE id = $1 AND profile_id = $2`,
		chi.URLParam(r, "id"), profileID(r)))
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		writeError(w, http.StatusNotFound, "not_found", "order not found")
		return o, false
	}
	if err != nil {
		s.internalError(w, err)
		return o, false
	}
	return o, true
}

type orderEvent struct {
	Type       string    `json:"type"`
	Status     string    `json:"status"`
	ReceivedAt time.Time `json:"received_at"`
}

func (s *Server) writeOwnerOrder(w http.ResponseWriter, r *http.Request, o orderRow) {
	rows, err := s.db.Query(r.Context(), `SELECT type, status, received_at FROM payment_events WHERE order_id = $1 ORDER BY received_at, id LIMIT 50`, o.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	events, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (orderEvent, error) {
		var e orderEvent
		return e, r.Scan(&e.Type, &e.Status, &e.ReceivedAt)
	})
	if err != nil {
		s.internalError(w, err)
		return
	}
	username := ""
	_ = s.db.QueryRow(r.Context(), `SELECT username FROM profiles WHERE id = $1`, o.ProfileID).Scan(&username)
	writeJSON(w, http.StatusOK, map[string]any{"order": o.view(), "events": events, "order_page_url": s.orderPageURL(username, o.ID)})
}

func (s *Server) handleGetOrder(w http.ResponseWriter, r *http.Request) {
	if o, ok := s.ownerOrder(w, r); ok {
		s.writeOwnerOrder(w, r, o)
	}
}

// handleRefreshOrder asks the provider for the latest status (owner button).
func (s *Server) handleRefreshOrder(w http.ResponseWriter, r *http.Request) {
	o, ok := s.ownerOrder(w, r)
	if !ok {
		return
	}
	if o.ProviderRef != nil && (o.LastCheckedAt == nil || s.now().Sub(*o.LastCheckedAt) >= reconcileEvery) {
		updated, err := s.reconcile(r.Context(), o)
		if err != nil {
			var pe *payments.Error
			msg := "couldn't reach the provider"
			if errors.As(err, &pe) {
				msg = pe.Message
			}
			if !errors.Is(err, payments.ErrNotFound) {
				writeError(w, http.StatusBadGateway, "provider_error", msg)
				return
			}
		} else {
			o = updated
		}
	}
	s.writeOwnerOrder(w, r, o)
}

func (s *Server) handleOrdersCSV(w http.ResponseWriter, r *http.Request) {
	pid := profileID(r)
	where, args, err := orderFilter(r, pid)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_status", err.Error())
		return
	}
	rows, err := s.db.Query(r.Context(), `SELECT `+orderCols+` FROM orders WHERE `+where+` ORDER BY created_at DESC, id DESC LIMIT 50000`, args...)
	if err != nil {
		s.internalError(w, err)
		return
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (orderRow, error) { return scanOrder(r) })
	if err != nil {
		s.internalError(w, err)
		return
	}
	out := make([][]string, 0, len(list))
	for _, o := range list {
		var names []string
		for _, it := range o.Items {
			n := it.Name
			if it.Variant != "" {
				n += " (" + it.Variant + ")"
			}
			if it.Quantity > 1 {
				n = strconv.Itoa(it.Quantity) + " × " + n
			}
			names = append(names, n)
		}
		paid := ""
		if o.PaidAt != nil {
			paid = ts(*o.PaidAt)
		}
		ref := ""
		if o.ProviderRef != nil {
			ref = *o.ProviderRef
		}
		out = append(out, []string{ts(o.CreatedAt), o.Code, string(o.Status), o.BuyerEmail, o.BuyerName,
			strings.Join(names, "; "), majorAmount(o.Amount), o.Currency, o.Provider, o.Mode, paid, ref})
	}
	writeCSV(w, "orders.csv", []string{"Created at", "Order", "Status", "Buyer email", "Buyer name", "Items",
		"Amount", "Currency", "Provider", "Mode", "Paid at", "Provider reference"}, out)
}

// majorAmount formats minor units as a plain decimal ("85000.00").
func majorAmount(minor int64) string {
	return fmt.Sprintf("%d.%02d", minor/100, minor%100)
}
