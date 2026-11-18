package payments

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeAPI records requests and replies with canned responses keyed by
// "METHOD /path". It stands in for the provider; tests never hit real APIs.
type fakeAPI struct {
	t    *testing.T
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []recorded
	resp map[string]fakeResp
}

type recorded struct {
	Method, Path, Query, Body string
	Header                    http.Header
}

type fakeResp struct {
	status int
	body   string
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{t: t, resp: map[string]fakeResp{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, recorded{r.Method, r.URL.Path, r.URL.RawQuery, string(b), r.Header.Clone()})
		res, ok := f.resp[r.Method+" "+r.URL.Path]
		f.mu.Unlock()
		if !ok {
			res = fakeResp{404, `{"error":"no fake for this route"}`}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(res.status)
		_, _ = io.WriteString(w, res.body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) on(route string, status int, body string) { f.resp[route] = fakeResp{status, body} }

func (f *fakeAPI) last() recorded {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		f.t.Fatal("no request reached the fake provider")
	}
	return f.reqs[len(f.reqs)-1]
}

var fixedNow = time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

func opts(f *fakeAPI) Options {
	return Options{
		BaseURLs: map[string]string{Xendit: f.srv.URL, Midtrans: f.srv.URL, "midtrans_api": f.srv.URL, Stripe: f.srv.URL},
		Now:      func() time.Time { return fixedNow },
	}
}

func sampleCheckout(currency string) Checkout {
	return Checkout{
		OrderID: "6f1c0d7e-0000-4000-8000-000000000001", Amount: 17000000, Currency: currency,
		Description: "Preset pack",
		Items:       []Item{{Name: "Preset pack", UnitAmount: 8500000, Quantity: 2}},
		BuyerEmail:  "buyer@example.com", BuyerName: "Budi",
		SuccessURL: "https://tautan.test/alice/orders/x?t=1", CancelURL: "https://tautan.test/alice",
		NotifyURL: "https://api.tautan.test/webhooks/midtrans/p1",
		ExpiresAt: fixedNow.Add(time.Hour),
	}
}
