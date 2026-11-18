package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
)

// Plain HTML forms for checkout and order lookup (payments b). The FE's
// checkout and lookup pages post straight here, with no JS. That keeps the
// buyer's own IP for rate limits (a server-side proxy would make every buyer
// look like the FE server), and the answer is a 303: to the provider's hosted
// checkout or the order page on success, back to the FE page with ?error=
// otherwise. JSON clients keep the JSON API on the same paths.

var itemIDRE = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)

func isFormPost(r *http.Request) bool {
	return strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/x-www-form-urlencoded")
}

// bufferedWriter captures a handler's JSON answer.
type bufferedWriter struct {
	h      http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedWriter) Header() http.Header         { return b.h }
func (b *bufferedWriter) Write(p []byte) (int, error) { return b.body.Write(p) }
func (b *bufferedWriter) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}

// runAsJSON calls h with body as its JSON request and returns the status and
// decoded answer.
func runAsJSON(h http.HandlerFunc, r *http.Request, body any) (int, map[string]any) {
	b, _ := json.Marshal(body)
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(b))
	r2.Header.Set("Content-Type", "application/json")
	bw := &bufferedWriter{h: http.Header{}}
	h(bw, r2)
	if bw.status == 0 {
		bw.status = http.StatusOK
	}
	var out map[string]any
	_ = json.Unmarshal(bw.body.Bytes(), &out)
	return bw.status, out
}

func errCode(out map[string]any) string {
	if e, ok := out["error"].(map[string]any); ok {
		if c, ok := e["code"].(string); ok {
			return c
		}
	}
	return "error"
}

func seeOther(w http.ResponseWriter, r *http.Request, to string) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// POST /public/{username}/orders as a form: block_id, item_id?, email, name?,
// provider?, and option.<variant name>=<choice> for each variant group.
func (s *Server) handleCreateOrderForm(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		seeOther(w, r, s.cfg.AppBaseURL+"/"+url.PathEscape(username)+"/checkout?error=invalid_form")
		return
	}
	f := r.PostForm
	in := createOrderIn{BlockID: f.Get("block_id"), ItemID: f.Get("item_id"), Email: f.Get("email"),
		Name: f.Get("name"), Provider: f.Get("provider"), Options: map[string]string{}}
	for k, v := range f {
		if name, ok := strings.CutPrefix(k, "option."); ok && len(v) > 0 {
			in.Options[name] = v[0]
		}
	}
	in.Items = formCartLines(f)
	if hasShippingFields(f) {
		in.Shipping = &ShippingAddress{Name: f.Get("shipping.name"), Phone: f.Get("shipping.phone"),
			Address: f.Get("shipping.address"), City: f.Get("shipping.city"), PostalCode: f.Get("shipping.postal_code"),
			Country: f.Get("shipping.country"), Notes: f.Get("shipping.notes")}
	}
	status, out := runAsJSON(s.handleCreateOrder, r, in)
	if status == http.StatusCreated {
		if u, _ := out["checkout_url"].(string); strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") {
			seeOther(w, r, u)
			return
		}
	}
	// block and item come from the buyer's form: echo them back only when they
	// look like ours (a uuid, a shop item id), so the Location header stays small.
	q := url.Values{"error": {errCode(out)}}
	if uuidRE.MatchString(in.BlockID) {
		q.Set("block", in.BlockID)
	}
	if itemIDRE.MatchString(in.ItemID) {
		q.Set("item", in.ItemID)
	}
	// The cart comes back in the same field names (bounded). Contact and
	// shipping fields never go in the URL.
	if len(in.Items) > 0 {
		for i, l := range in.Items {
			if !itemIDRE.MatchString(l.ItemID) {
				continue
			}
			n := "line." + strconv.Itoa(i) + "."
			q.Set(n+"item", l.ItemID)
			if l.Quantity > 0 && l.Quantity <= maxShipQty {
				q.Set(n+"qty", strconv.Itoa(l.Quantity))
			}
			kept := 0
			for name, v := range l.Options {
				if kept < 3 && utf8.RuneCountInString(name) <= 30 && utf8.RuneCountInString(v) <= 30 {
					q.Set(n+"option."+name, v)
					kept++
				}
			}
		}
	}
	// Which field failed (keys are ours, never the buyer's values).
	if e, ok := out["error"].(map[string]any); ok {
		if d, ok := e["details"].(map[string]any); ok {
			for k := range d {
				if strings.HasPrefix(k, "items[") || strings.HasPrefix(k, "shipping.") {
					q.Add("field", k)
				}
			}
		}
	}
	seeOther(w, r, s.cfg.AppBaseURL+"/"+url.PathEscape(username)+"/checkout?"+q.Encode())
}

// formCartLines reads line.N.item, line.N.qty and line.N.option.<name> for
// N in 0..19, in order; gaps are skipped.
func formCartLines(f url.Values) []cartLineIn {
	var out []cartLineIn
	for i := 0; i < maxCartLines; i++ {
		n := "line." + strconv.Itoa(i) + "."
		id := f.Get(n + "item")
		if id == "" {
			continue
		}
		l := cartLineIn{ItemID: id, Options: map[string]string{}}
		if q := f.Get(n + "qty"); q != "" {
			v, err := strconv.Atoi(q)
			if err != nil || v < 1 {
				v = -1 // rejected as invalid_quantity
			}
			l.Quantity = v
		}
		for k, v := range f {
			if name, ok := strings.CutPrefix(k, n+"option."); ok && len(v) > 0 {
				l.Options[name] = v[0]
			}
		}
		out = append(out, l)
	}
	return out
}

func hasShippingFields(f url.Values) bool {
	for k := range f {
		if strings.HasPrefix(k, "shipping.") {
			return true
		}
	}
	return false
}

// POST /public/{username}/orders/lookup as a form: email, code.
func (s *Server) handleOrderLookupForm(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		seeOther(w, r, s.cfg.AppBaseURL+"/"+url.PathEscape(username)+"/orders?error=invalid_form")
		return
	}
	status, out := runAsJSON(s.handleOrderLookup, r, map[string]string{"email": r.PostForm.Get("email"), "code": r.PostForm.Get("code")})
	if id, _ := out["order_id"].(string); status == http.StatusOK && id != "" {
		seeOther(w, r, s.orderPageURL(username, id))
		return
	}
	seeOther(w, r, s.cfg.AppBaseURL+"/"+url.PathEscape(username)+"/orders?error="+url.QueryEscape(errCode(out)))
}

// formOr routes form posts to form and everything else to json.
func formOr(form, json http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if isFormPost(r) {
			form(w, r)
			return
		}
		json(w, r)
	}
}
