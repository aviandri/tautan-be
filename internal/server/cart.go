package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/aviandri/tautan-be/internal/mail"
	"github.com/aviandri/tautan-be/internal/payments"
)

// Payments (c): shop cart checkout. A cart is several lines from one shop
// card, in the shop's one checkout currency. Physical items ("ship") take a
// quantity and a shipping address; the shop's flat shipping fee is added once.
// Prices, variants and the fee always come from the stored card.

const (
	maxCartLines = 20
	maxShipQty   = 20
)

// ShippingAddress is the buyer's delivery address. It's personal data: only
// the page owner sees it (order detail, CSV); it's never logged and never
// sent to the payment provider.
type ShippingAddress struct {
	Name       string `json:"name"`
	Phone      string `json:"phone"`
	Address    string `json:"address"`
	City       string `json:"city"`
	PostalCode string `json:"postal_code"`
	Country    string `json:"country"` // ISO 3166-1 alpha-2, default ID
	Notes      string `json:"notes"`
}

type cartLineIn struct {
	ItemID   string            `json:"item_id"`
	Quantity int               `json:"quantity"`
	Options  map[string]string `json:"options"`
}

// cartError is a 400 with a stable code and a per-field detail.
type cartError struct{ code, field, msg string }

func (e cartError) Error() string { return e.msg }

var (
	phoneCleanRE  = regexp.MustCompile(`[\s\-().]`)
	shipPhoneRE   = regexp.MustCompile(`^\+?[0-9]{7,15}$`)
	postalRE      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 \-]{1,11}$`)
	shipCountryRE = regexp.MustCompile(`^[A-Z]{2}$`)
	idPostalRE    = regexp.MustCompile(`^[0-9]{5}$`)
)

// normShipping trims and checks an address; the phone check is loose (local
// 08…, 62…, +62…, or any international number) because couriers call it.
func normShipping(a *ShippingAddress) map[string]string {
	errs := map[string]string{}
	a.Name, a.Address, a.City = strings.TrimSpace(a.Name), strings.TrimSpace(a.Address), strings.TrimSpace(a.City)
	a.PostalCode, a.Notes = strings.TrimSpace(a.PostalCode), strings.TrimSpace(a.Notes)
	a.Phone = phoneCleanRE.ReplaceAllString(strings.TrimSpace(a.Phone), "")
	a.Country = strings.ToUpper(strings.TrimSpace(a.Country))
	if a.Country == "" {
		a.Country = "ID"
	}
	text := func(field, v string, min, max int, label string) {
		n := len([]rune(v))
		switch {
		case n < min:
			errs["shipping."+field] = "enter the " + label
		case n > max:
			errs["shipping."+field] = fmt.Sprintf("at most %d characters", max)
		}
	}
	text("name", a.Name, 1, 100, "recipient's name")
	text("address", a.Address, 1, 300, "street address")
	text("city", a.City, 1, 80, "city")
	text("notes", a.Notes, 0, 300, "notes")
	if !shipPhoneRE.MatchString(a.Phone) {
		errs["shipping.phone"] = "enter a phone number the courier can call, e.g. 0812 3456 7890"
	}
	if !shipCountryRE.MatchString(a.Country) {
		errs["shipping.country"] = "choose a country"
	}
	switch {
	case a.PostalCode == "" && a.Country == "ID":
		errs["shipping.postal_code"] = "enter the postal code"
	case a.PostalCode != "" && !postalRE.MatchString(a.PostalCode):
		errs["shipping.postal_code"] = "enter a valid postal code"
	case a.Country == "ID" && !idPostalRE.MatchString(a.PostalCode):
		errs["shipping.postal_code"] = "Indonesian postal codes have 5 digits"
	}
	return errs
}

// cartLines resolves a cart against the stored shop card: each line's price
// and variant come from the card; equal lines merge.
func cartLines(b Block, in []cartLineIn) ([]orderItem, string, error) {
	if len(in) == 0 || len(in) > maxCartLines {
		return nil, "", cartError{"invalid_items", "items", fmt.Sprintf("a cart has 1 to %d lines", maxCartLines)}
	}
	var out []orderItem
	index := map[string]int{}
	currency := ""
	for i, l := range in {
		field := fmt.Sprintf("items[%d]", i)
		buy, err := purchasable(b, l.ItemID, l.Options)
		var ve variantError
		if errors.As(err, &ve) {
			return nil, "", cartError{"invalid_options", field + ".options", ve.msg}
		}
		if err != nil {
			return nil, "", cartError{"not_for_sale", field, "one of the items in your cart can't be bought any more"}
		}
		if currency == "" {
			currency = buy.currency
		} else if buy.currency != currency {
			return nil, "", cartError{"not_for_sale", field, "items in one cart must share a currency"}
		}
		q := l.Quantity
		if q == 0 {
			q = 1
		}
		it := buy.item
		switch {
		case it.Delivery == "ship" && (q < 1 || q > maxShipQty):
			return nil, "", cartError{"invalid_quantity", field + ".quantity", fmt.Sprintf("quantity must be 1 to %d", maxShipQty)}
		case it.Delivery != "ship" && q != 1:
			return nil, "", cartError{"invalid_quantity", field + ".quantity", "digital items are bought once"}
		}
		it.Quantity = q
		key := it.ItemID + "|" + it.Variant
		if j, dup := index[key]; dup {
			if it.Delivery == "ship" {
				out[j].Quantity += q
				if out[j].Quantity > maxShipQty {
					return nil, "", cartError{"invalid_quantity", field + ".quantity", fmt.Sprintf("quantity must be 1 to %d", maxShipQty)}
				}
			}
			continue
		}
		index[key] = len(out)
		out = append(out, it)
	}
	return out, currency, nil
}

// shopShippingFee is the shop's flat fee (0 for other cards or free shipping).
func shopShippingFee(b Block) int64 {
	if b.Type != "shop" {
		return 0
	}
	var d ShopData
	if json.Unmarshal(b.Data, &d) != nil || d.ShippingFee == nil || *d.ShippingFee < 0 {
		return 0
	}
	return *d.ShippingFee
}

func linesShip(items []orderItem) bool {
	for _, it := range items {
		if it.Delivery == "ship" {
			return true
		}
	}
	return false
}

func linesDownload(items []orderItem) bool {
	for _, it := range items {
		if it.Download != nil {
			return true
		}
	}
	return false
}

// providersInRange keeps the providers whose checkout takes this total.
func providersInRange(names []string, amount int64, currency string) []string {
	var out []string
	for _, n := range names {
		min, max := payments.AmountRange(n, currency)
		if amount >= min && amount <= max {
			out = append(out, n)
		}
	}
	return out
}

// rangeError explains why no provider takes the total.
func rangeError(names []string, amount int64, currency string) cartError {
	lo, hi := int64(-1), int64(0)
	for _, n := range names {
		min, max := payments.AmountRange(n, currency)
		if lo < 0 || min < lo {
			lo = min
		}
		if max > hi {
			hi = max
		}
	}
	if amount < lo {
		return cartError{"amount_too_small", "amount", "the total must be at least " + mail.Money(lo, currency) + " to pay online"}
	}
	return cartError{"amount_too_large", "amount", "the total can be at most " + mail.Money(hi, currency) + " for one order; split it into smaller orders"}
}

// ---------- owner: POST /orders/{id}/ship ----------

// handleShipOrder marks a paid order with physical items as shipped, with an
// optional tracking number, and emails the buyer once. Sending it again only
// updates the tracking number.
func (s *Server) handleShipOrder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Tracking string `json:"tracking"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	in.Tracking = strings.TrimSpace(in.Tracking)
	if len([]rune(in.Tracking)) > 100 {
		writeErrorDetails(w, http.StatusBadRequest, "invalid_data", "tracking is too long", map[string]string{"tracking": "at most 100 characters"})
		return
	}
	o, ok := s.ownerOrder(w, r)
	if !ok {
		return
	}
	if o.Fulfilment == nil || o.Status != payments.StatusPaid {
		writeError(w, http.StatusConflict, "not_shippable", "only paid orders with items to ship can be marked shipped")
		return
	}
	ctx := r.Context()
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		cur, err := scanOrder(tx.QueryRow(ctx, `SELECT `+orderCols+` FROM orders WHERE id = $1 FOR UPDATE`, o.ID))
		if err != nil {
			return err
		}
		first := cur.Fulfilment != nil && *cur.Fulfilment == "unfulfilled"
		o, err = scanOrder(tx.QueryRow(ctx, `UPDATE orders SET fulfilment = 'shipped', tracking = $2,
			shipped_at = COALESCE(shipped_at, now()), updated_at = now() WHERE id = $1 RETURNING `+orderCols, o.ID, in.Tracking))
		if err != nil || !first {
			return err
		}
		// Same caps as receipts: the owner can't turn this into a mailer.
		if !s.receiptPageLimiter.Allow(o.ProfileID) || !s.receiptToLimiter.Allow(o.ProfileID+"|"+o.BuyerEmail) {
			slog.Warn("payments: shipped email not sent (rate limit)", "profile_id", o.ProfileID, "order_id", o.ID)
			return nil
		}
		return s.enqueueShipped(ctx, tx, o)
	})
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.writeOwnerOrder(w, r, o)
}

func (s *Server) enqueueShipped(ctx context.Context, tx pgx.Tx, o orderRow) error {
	username, seller, err := sellerName(ctx, tx, o.ProfileID)
	if err != nil {
		return err
	}
	return s.enqueueEmail(ctx, tx, "order_shipped", mail.OrderShipped(o.BuyerEmail, mail.Shipped{
		Seller: seller, Code: o.Code, Tracking: o.Tracking, Link: s.orderPageURL(username, o.ID),
	}))
}

func sellerName(ctx context.Context, q pgx.Tx, pid string) (username, seller string, err error) {
	var display string
	if err = q.QueryRow(ctx, `SELECT username, display_name FROM profiles WHERE id = $1`, pid).Scan(&username, &display); err != nil {
		return
	}
	seller = strings.TrimSpace(display)
	if seller == "" {
		seller = "@" + username
	}
	return
}
