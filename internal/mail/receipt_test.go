package mail

import (
	"strings"
	"testing"
)

func TestMoney(t *testing.T) {
	for in, want := range map[[2]any]string{
		{int64(8500000), "IDR"}:   "Rp 85.000",
		{int64(150000000), "IDR"}: "Rp 1.500.000",
		{int64(1999), "USD"}:      "US$19.99",
		{int64(123456789), "USD"}: "US$1,234,567.89",
		{int64(5), "USD"}:         "US$0.05",
	} {
		if got := Money(in[0].(int64), in[1].(string)); got != want {
			t.Errorf("Money(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestOrderReceipt(t *testing.T) {
	m := OrderReceipt("b@example.com", Receipt{Seller: "Ayu <Studio>", Code: "K7Q2-M9XD",
		Lines: []ReceiptLine{{Name: "Presets", Quantity: 1, Amount: "Rp 85.000"}}, Total: "Rp 85.000", Link: "https://t.test/o?t=1"})
	if m.To != "b@example.com" || !strings.Contains(m.Subject, "K7Q2-M9XD") || !strings.Contains(m.Text, "https://t.test/o?t=1") {
		t.Fatalf("message = %+v", m)
	}
	if strings.Contains(m.HTML, "<Studio>") || !strings.Contains(m.HTML, "Ayu &lt;Studio&gt;") {
		t.Fatal("seller name not escaped in HTML")
	}
}
