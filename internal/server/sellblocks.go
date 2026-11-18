package server

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata" // store_info.tz must validate in CGO-free/scratch builds
)

// Phase 3-A "Sell" cards, matching "P3-A data contract" in SPEC.md and the zod
// schemas in tautan-fe src/lib/types.ts. Money is an integer in the currency's
// ISO 4217 minor units (IDR has 2: Rp 85.000 = 8500000). An empty currency is
// stored as IDR. Links are http(s) only; image fields also take /uploads/ paths.

type (
	ProductVariant struct {
		Name    string   `json:"name"`
		Options []string `json:"options"`
	}
	// DownloadFile is what a buyer gets after paying for a checkout product.
	DownloadFile struct {
		URL      string `json:"url"`
		FileName string `json:"file_name,omitempty"`
	}
	// SellAction is what a product's buy button does.
	SellAction struct {
		Kind        string `json:"kind"` // url | whatsapp | marketplace | checkout
		URL         string `json:"url,omitempty"`
		WANumber    string `json:"wa_number,omitempty"`
		WATemplate  string `json:"wa_template,omitempty"` // {name} {variant} {price}
		Marketplace string `json:"marketplace,omitempty"`
		Label       string `json:"label,omitempty"`
		// checkout only (payments b): the digital file delivered after payment.
		// Never part of public reads.
		Download *DownloadFile `json:"download,omitempty"`
	}
	ProductData struct {
		ID          string           `json:"id,omitempty"` // shop items only
		Images      []string         `json:"images"`
		Name        string           `json:"name"`
		Description string           `json:"description,omitempty"`
		Price       *int64           `json:"price,omitempty"`
		Currency    string           `json:"currency"`
		CompareAt   *int64           `json:"compare_at,omitempty"`
		Badge       string           `json:"badge,omitempty"`
		Variants    []ProductVariant `json:"variants,omitempty"`
		StockNote   string           `json:"stock_note,omitempty"`
		Category    string           `json:"category,omitempty"` // shop items only
		Action      SellAction       `json:"action"`
		Schedule    *Schedule        `json:"schedule,omitempty"`
	}
	ShopData struct {
		Title      string        `json:"title,omitempty"`
		Categories []string      `json:"categories,omitempty"`
		Items      []ProductData `json:"items"`
		Schedule   *Schedule     `json:"schedule,omitempty"`
	}
	MarketplaceStore struct {
		Platform string `json:"platform"`
		URL      string `json:"url"`
	}
	MarketplaceData struct {
		Title    string             `json:"title,omitempty"`
		Stores   []MarketplaceStore `json:"stores"`
		Schedule *Schedule          `json:"schedule,omitempty"`
	}
	WAOrderItem struct {
		Name  string `json:"name"`
		Price *int64 `json:"price,omitempty"`
	}
	WAOrderForm struct {
		Fields   []string      `json:"fields"`
		Items    []WAOrderItem `json:"items,omitempty"`
		Currency string        `json:"currency,omitempty"`
	}
	WhatsAppData struct {
		Label           string       `json:"label"`
		Number          string       `json:"number"`
		MessageTemplate string       `json:"message_template"` // {items} {name} {address} {note} {total}
		OrderForm       *WAOrderForm `json:"order_form,omitempty"`
		Schedule        *Schedule    `json:"schedule,omitempty"`
	}
	PriceItem struct {
		Name      string `json:"name"`
		Desc      string `json:"desc,omitempty"`
		Price     *int64 `json:"price,omitempty"`
		PriceFrom bool   `json:"price_from,omitempty"`
		ImageURL  string `json:"image_url,omitempty"`
	}
	PriceSection struct {
		Title string      `json:"title"`
		Items []PriceItem `json:"items"`
	}
	TargetAction struct { // price_list.ask, payment.confirm
		Kind   string `json:"kind"`
		Target string `json:"target"`
		Label  string `json:"label,omitempty"`
	}
	PriceListData struct {
		Sections []PriceSection `json:"sections"`
		Currency string         `json:"currency"`
		Note     string         `json:"note,omitempty"`
		Ask      *TargetAction  `json:"ask,omitempty"` // whatsapp | url
		Schedule *Schedule      `json:"schedule,omitempty"`
	}
	StoreHours struct {
		Day   string `json:"day"`   // mon..sun
		Open  string `json:"open"`  // HH:MM
		Close string `json:"close"` // HH:MM; < open means past midnight; 24:00 allowed
	}
	StoreInfoData struct {
		Hours        []StoreHours `json:"hours"`
		TZ           string       `json:"tz"`
		Address      string       `json:"address,omitempty"`
		Area         string       `json:"area,omitempty"`
		Couriers     []string     `json:"couriers,omitempty"`
		DeliveryNote string       `json:"delivery_note,omitempty"`
		Schedule     *Schedule    `json:"schedule,omitempty"`
	}
	PaymentAccount struct {
		Bank   string `json:"bank"`
		Name   string `json:"name"`
		Number string `json:"number"`
	}
	PaymentData struct {
		Title        string           `json:"title,omitempty"`
		QRISImageURL string           `json:"qris_image_url,omitempty"`
		Accounts     []PaymentAccount `json:"accounts"`
		Note         string           `json:"note,omitempty"`
		Confirm      *TargetAction    `json:"confirm,omitempty"` // whatsapp | url | form
		Schedule     *Schedule        `json:"schedule,omitempty"`
	}
	TipPlatform struct {
		Platform string `json:"platform"`
		URL      string `json:"url"`
	}
	TipJarData struct {
		Title       string        `json:"title"`
		Description string        `json:"description,omitempty"`
		Amounts     []int64       `json:"amounts,omitempty"`
		Currency    string        `json:"currency"`
		Platforms   []TipPlatform `json:"platforms"`
		Schedule    *Schedule     `json:"schedule,omitempty"`
	}
	ReviewItem struct {
		Stars     int    `json:"stars"`
		Quote     string `json:"quote"`
		Author    string `json:"author"`
		SourceURL string `json:"source_url,omitempty"`
	}
	ReviewsData struct {
		Title        string       `json:"title,omitempty"`
		Average      *float64     `json:"average,omitempty"`
		Count        *int         `json:"count,omitempty"`
		Distribution []int        `json:"distribution,omitempty"` // 5 counts: 5 stars first
		Source       string       `json:"source,omitempty"`
		Items        []ReviewItem `json:"items"`
		Schedule     *Schedule    `json:"schedule,omitempty"`
	}
)

var (
	sellTypes = []string{"product", "shop", "marketplace", "whatsapp", "price_list", "store_info", "payment", "tip_jar", "reviews"}

	marketplacePlatforms = set("shopee", "tokopedia", "tiktok_shop", "lazada", "blibli", "bukalapak", "etsy", "amazon", "gumroad")
	tipPlatforms         = set("saweria", "trakteer", "kofi", "paypal", "buymeacoffee")
	productBadges        = set("new", "best_seller", "pre_order", "sold_out")
	sellActionKinds      = set("url", "whatsapp", "marketplace", "checkout")
	waOrderFields        = set("items", "name", "address", "note")
	weekDays             = set("mon", "tue", "wed", "thu", "fri", "sat", "sun")
	couriers             = set("gosend", "grabexpress", "jne", "jnt", "sicepat", "anteraja", "pos", "shopee_express", "lalamove", "paxel", "pickup")
	currencies           = set("IDR", "USD", "SGD", "MYR")

	e164RE     = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)
	phoneSepRE = regexp.MustCompile(`[\s\-().]`)
	hhmmRE     = regexp.MustCompile(`^(([01][0-9]|2[0-3]):[0-5][0-9]|24:00)$`)
	shopIDRE   = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)
)

const maxMoney = 100_000_000_000_000 // 1e12 major units at 2 decimals

func init() {
	for _, t := range sellTypes {
		blockTypes[t] = true
	}
}

// normE164 strips spaces, dashes, dots and brackets, then requires +<cc><number>.
func normE164(field string, v *string) error {
	n := phoneSepRE.ReplaceAllString(strings.TrimSpace(*v), "")
	if n == "" {
		return ferr(field, "%s is required", field)
	}
	if !e164RE.MatchString(n) {
		return ferr(field, "%s must be an E.164 phone number such as +6281234567890", field)
	}
	*v = n
	return nil
}

// checkWebURL allows only http(s) links with a host.
func checkWebURL(field, s string, required bool) error {
	s = strings.TrimSpace(s)
	if s == "" {
		if required {
			return ferr(field, "%s is required", field)
		}
		return nil
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ferr(field, "%s must be an http(s) URL", field)
	}
	return nil
}

// checkImageURL allows http(s) links or our own /uploads/ paths.
func checkImageURL(field, s string, required bool) error {
	if strings.HasPrefix(strings.TrimSpace(s), "/uploads/") {
		return nil
	}
	return checkWebURL(field, s, required)
}

func checkMoney(field string, v *int64) error {
	if v != nil && (*v < 0 || *v > maxMoney) {
		return ferr(field, "%s must be a non-negative integer in minor units", field)
	}
	return nil
}

// normCurrency defaults an empty currency to IDR.
func normCurrency(field string, c *string) error {
	if *c == "" {
		*c = "IDR"
	}
	if !currencies[*c] {
		return ferr(field, "%s must be IDR, USD, SGD or MYR", field)
	}
	return nil
}

func checkCount(field string, n, min, max int) error {
	if n < min || n > max {
		if min == 0 {
			return ferr(field, "%s can have at most %d entries", field, max)
		}
		return ferr(field, "%s needs %d to %d entries", field, min, max)
	}
	return nil
}

// checkText is required + maxLen.
func checkText(field, v string, n int) error {
	return firstErr(required(field, v), maxLen(field, v, n))
}

func checkSellAction(p string, a *SellAction) error {
	if !sellActionKinds[a.Kind] {
		return ferr(p+".kind", "%s.kind must be url, whatsapp, marketplace or checkout", p)
	}
	if err := maxLen(p+".label", a.Label, 40); err != nil {
		return err
	}
	if a.Kind != "checkout" {
		a.Download = nil
	}
	switch a.Kind {
	case "checkout":
		a.URL, a.WANumber, a.WATemplate, a.Marketplace = "", "", "", ""
		if a.Download == nil {
			return ferr(p+".download.url", "%s.download.url is required for checkout", p)
		}
		return firstErr(checkImageURL(p+".download.url", a.Download.URL, true), maxLen(p+".download.file_name", a.Download.FileName, 200))
	case "url":
		a.WANumber, a.WATemplate, a.Marketplace = "", "", ""
		return checkWebURL(p+".url", a.URL, true)
	case "whatsapp":
		a.URL, a.Marketplace = "", ""
		return firstErr(normE164(p+".wa_number", &a.WANumber), maxLen(p+".wa_template", a.WATemplate, 500))
	default: // marketplace
		a.WANumber, a.WATemplate = "", ""
		if !marketplacePlatforms[a.Marketplace] {
			return ferr(p+".marketplace", "%s.marketplace has an unsupported value %q", p, a.Marketplace)
		}
		return checkWebURL(p+".url", a.URL, true)
	}
}

func checkProduct(p string, d *ProductData) error {
	if d.Images == nil {
		d.Images = make([]string, 0)
	}
	if err := checkCount(p+"images", len(d.Images), 0, 8); err != nil {
		return err
	}
	for i, im := range d.Images {
		if err := checkImageURL(fmt.Sprintf("%simages[%d]", p, i), im, true); err != nil {
			return err
		}
	}
	if err := firstErr(
		checkText(p+"name", d.Name, 100),
		maxLen(p+"description", d.Description, 1000),
		checkMoney(p+"price", d.Price), normCurrency(p+"currency", &d.Currency),
		checkMoney(p+"compare_at", d.CompareAt),
		oneOf(p+"badge", d.Badge, productBadges),
		maxLen(p+"stock_note", d.StockNote, 60),
		checkCount(p+"variants", len(d.Variants), 0, 3),
	); err != nil {
		return err
	}
	if d.CompareAt != nil && (d.Price == nil || *d.CompareAt <= *d.Price) {
		return ferr(p+"compare_at", "%scompare_at must be greater than price", p)
	}
	for i := range d.Variants {
		v := &d.Variants[i]
		f := fmt.Sprintf("%svariants[%d]", p, i)
		if v.Options == nil {
			v.Options = make([]string, 0)
		}
		if err := firstErr(checkText(f+".name", v.Name, 30), checkCount(f+".options", len(v.Options), 1, 20)); err != nil {
			return err
		}
		for j, o := range v.Options {
			if err := checkText(fmt.Sprintf("%s.options[%d]", f, j), o, 30); err != nil {
				return err
			}
		}
	}
	if err := checkSellAction(p+"action", &d.Action); err != nil {
		return err
	}
	if d.Action.Kind == "checkout" {
		if d.Price == nil {
			return ferr(p+"price", "%sprice is required for checkout", p)
		}
		return checkPaidPrice(p, d.Price, &d.Currency)
	}
	return nil
}

// checkPaidPrice validates a price that goes through checkout (payments b):
// above zero, IDR or USD (what every provider here can take), and whole
// rupiah for IDR because Xendit and Midtrans don't accept sen.
func checkPaidPrice(p string, price *int64, currency *string) error {
	if price == nil {
		return nil
	}
	if *price <= 0 || *price > maxMoney {
		return ferr(p+"price", "%sprice must be greater than 0 (minor units)", p)
	}
	if *currency == "" {
		*currency = "IDR"
	}
	if *currency != "IDR" && *currency != "USD" {
		return ferr(p+"currency", "%scurrency must be IDR or USD for checkout", p)
	}
	if *currency == "IDR" && *price%100 != 0 {
		return ferr(p+"price", "%sprice must be whole rupiah (a multiple of 100 minor units)", p)
	}
	return nil
}

func checkShop(d *ShopData) error {
	if d.Items == nil {
		d.Items = make([]ProductData, 0)
	}
	if err := firstErr(maxLen("title", d.Title, 100), checkCount("categories", len(d.Categories), 0, 12),
		checkCount("items", len(d.Items), 1, 60)); err != nil {
		return err
	}
	cats := map[string]bool{}
	for i, c := range d.Categories {
		f := fmt.Sprintf("categories[%d]", i)
		if err := checkText(f, c, 30); err != nil {
			return err
		}
		if cats[c] {
			return ferr(f, "%s is a duplicate", f)
		}
		cats[c] = true
	}
	ids := map[string]bool{}
	for i := range d.Items {
		it := &d.Items[i]
		p := fmt.Sprintf("items[%d].", i)
		it.Schedule = nil
		if !shopIDRE.MatchString(it.ID) || ids[it.ID] {
			return ferr(p+"id", "%sid must be a unique id of 1-40 characters a-z, 0-9, _ or -", p)
		}
		ids[it.ID] = true
		if err := checkProduct(p, it); err != nil {
			return err
		}
		if it.Category != "" && !cats[it.Category] {
			return ferr(p+"category", "%scategory must be one of categories", p)
		}
	}
	return nil
}

func checkMarketplace(d *MarketplaceData) error {
	if d.Stores == nil {
		d.Stores = make([]MarketplaceStore, 0)
	}
	if err := firstErr(maxLen("title", d.Title, 60), checkCount("stores", len(d.Stores), 1, 9)); err != nil {
		return err
	}
	seen := map[string]bool{}
	for i, st := range d.Stores {
		f := fmt.Sprintf("stores[%d]", i)
		if !marketplacePlatforms[st.Platform] {
			return ferr(f+".platform", "%s.platform has an unsupported value %q", f, st.Platform)
		}
		if seen[st.Platform] {
			return ferr(f+".platform", "%s.platform is a duplicate", f)
		}
		seen[st.Platform] = true
		if err := checkWebURL(f+".url", st.URL, true); err != nil {
			return err
		}
	}
	return nil
}

func checkWhatsApp(d *WhatsAppData) error {
	if err := firstErr(checkText("label", d.Label, 40), normE164("number", &d.Number),
		checkText("message_template", d.MessageTemplate, 1000)); err != nil {
		return err
	}
	f := d.OrderForm
	if f == nil {
		return nil
	}
	if f.Fields == nil {
		f.Fields = make([]string, 0)
	}
	if err := checkCount("order_form.fields", len(f.Fields), 1, 4); err != nil {
		return err
	}
	seen := map[string]bool{}
	for i, k := range f.Fields {
		p := fmt.Sprintf("order_form.fields[%d]", i)
		if !waOrderFields[k] {
			return ferr(p, "%s must be items, name, address or note", p)
		}
		if seen[k] {
			return ferr(p, "%s is a duplicate", p)
		}
		seen[k] = true
	}
	if seen["items"] || len(f.Items) > 0 {
		if err := checkCount("order_form.items", len(f.Items), 1, 30); err != nil {
			return err
		}
	}
	for i, it := range f.Items {
		p := fmt.Sprintf("order_form.items[%d]", i)
		if err := firstErr(checkText(p+".name", it.Name, 80), checkMoney(p+".price", it.Price)); err != nil {
			return err
		}
	}
	if f.Currency != "" {
		return normCurrency("order_form.currency", &f.Currency)
	}
	return nil
}

// checkTarget validates an ask/confirm target for its kind.
func checkTarget(p string, a *TargetAction, kinds ...string) error {
	if err := maxLen(p+".label", a.Label, 40); err != nil {
		return err
	}
	ok := false
	for _, k := range kinds {
		ok = ok || a.Kind == k
	}
	if !ok {
		return ferr(p+".kind", "%s.kind must be one of %s", p, strings.Join(kinds, ", "))
	}
	switch a.Kind {
	case "whatsapp":
		return normE164(p+".target", &a.Target)
	case "url":
		return checkWebURL(p+".target", a.Target, true)
	default: // form
		if !uuidRE.MatchString(a.Target) {
			return ferr(p+".target", "%s.target must be a form block id", p)
		}
		return nil
	}
}

func checkPriceList(d *PriceListData) error {
	if d.Sections == nil {
		d.Sections = make([]PriceSection, 0)
	}
	if err := firstErr(normCurrency("currency", &d.Currency), maxLen("note", d.Note, 300),
		checkCount("sections", len(d.Sections), 1, 10)); err != nil {
		return err
	}
	if d.Ask != nil {
		if err := checkTarget("ask", d.Ask, "whatsapp", "url"); err != nil {
			return err
		}
	}
	for i := range d.Sections {
		s := &d.Sections[i]
		p := fmt.Sprintf("sections[%d]", i)
		if s.Items == nil {
			s.Items = make([]PriceItem, 0)
		}
		if err := firstErr(maxLen(p+".title", s.Title, 60), checkCount(p+".items", len(s.Items), 1, 40)); err != nil {
			return err
		}
		for j, it := range s.Items {
			q := fmt.Sprintf("%s.items[%d]", p, j)
			if err := firstErr(checkText(q+".name", it.Name, 80), maxLen(q+".desc", it.Desc, 200),
				checkMoney(q+".price", it.Price), checkImageURL(q+".image_url", it.ImageURL, false)); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkStoreInfo(d *StoreInfoData) error {
	if d.Hours == nil {
		d.Hours = make([]StoreHours, 0)
	}
	if d.TZ == "" || strings.EqualFold(d.TZ, "local") {
		return ferr("tz", "tz is required (an IANA time zone such as Asia/Jakarta)")
	}
	if _, err := time.LoadLocation(d.TZ); err != nil {
		return ferr("tz", "tz must be an IANA time zone such as Asia/Jakarta")
	}
	if err := firstErr(checkCount("hours", len(d.Hours), 0, 21), maxLen("address", d.Address, 200),
		maxLen("area", d.Area, 100), checkCount("couriers", len(d.Couriers), 0, len(couriers)),
		maxLen("delivery_note", d.DeliveryNote, 300)); err != nil {
		return err
	}
	for i, h := range d.Hours {
		p := fmt.Sprintf("hours[%d]", i)
		if !weekDays[h.Day] {
			return ferr(p+".day", "%s.day must be one of mon, tue, wed, thu, fri, sat, sun", p)
		}
		if !hhmmRE.MatchString(h.Open) || h.Open == "24:00" {
			return ferr(p+".open", "%s.open must be HH:MM", p)
		}
		if !hhmmRE.MatchString(h.Close) || h.Open == h.Close {
			return ferr(p+".close", "%s.close must be HH:MM (24:00 allowed) and differ from open", p)
		}
	}
	seen := map[string]bool{}
	for i, c := range d.Couriers {
		f := fmt.Sprintf("couriers[%d]", i)
		if !couriers[c] || seen[c] {
			return ferr(f, "%s must be a unique courier id such as jne, jnt, gosend or pickup", f)
		}
		seen[c] = true
	}
	return nil
}

func checkPayment(d *PaymentData) error {
	if d.Accounts == nil {
		d.Accounts = make([]PaymentAccount, 0)
	}
	if err := firstErr(maxLen("title", d.Title, 60), checkImageURL("qris_image_url", d.QRISImageURL, false),
		maxLen("note", d.Note, 300), checkCount("accounts", len(d.Accounts), 0, 6)); err != nil {
		return err
	}
	if d.QRISImageURL == "" && len(d.Accounts) == 0 {
		return ferr("accounts", "payment needs a qris_image_url or at least one account")
	}
	for i, a := range d.Accounts {
		p := fmt.Sprintf("accounts[%d]", i)
		if err := firstErr(checkText(p+".bank", a.Bank, 40), checkText(p+".name", a.Name, 60),
			checkText(p+".number", a.Number, 40)); err != nil {
			return err
		}
	}
	if d.Confirm != nil {
		return checkTarget("confirm", d.Confirm, "whatsapp", "url", "form")
	}
	return nil
}

func checkTipJar(d *TipJarData) error {
	if d.Platforms == nil {
		d.Platforms = make([]TipPlatform, 0)
	}
	if err := firstErr(checkText("title", d.Title, 100), maxLen("description", d.Description, 300),
		checkCount("amounts", len(d.Amounts), 0, 4), normCurrency("currency", &d.Currency),
		checkCount("platforms", len(d.Platforms), 1, 5)); err != nil {
		return err
	}
	for i, a := range d.Amounts {
		if a <= 0 || a > maxMoney {
			f := fmt.Sprintf("amounts[%d]", i)
			return ferr(f, "%s must be a positive integer in minor units", f)
		}
	}
	for i, p := range d.Platforms {
		f := fmt.Sprintf("platforms[%d]", i)
		if !tipPlatforms[p.Platform] {
			return ferr(f+".platform", "%s.platform must be saweria, trakteer, kofi, paypal or buymeacoffee", f)
		}
		if err := checkWebURL(f+".url", p.URL, true); err != nil {
			return err
		}
	}
	return nil
}

func checkReviews(d *ReviewsData) error {
	if d.Items == nil {
		d.Items = make([]ReviewItem, 0)
	}
	if d.Average != nil && (*d.Average < 0 || *d.Average > 5) {
		return ferr("average", "average must be between 0 and 5")
	}
	if d.Count != nil && *d.Count < 0 {
		return ferr("count", "count must not be negative")
	}
	if d.Distribution != nil {
		if len(d.Distribution) != 5 {
			return ferr("distribution", "distribution must have exactly 5 counts (5 stars first)")
		}
		for i, n := range d.Distribution {
			if n < 0 {
				return ferr(fmt.Sprintf("distribution[%d]", i), "distribution counts must not be negative")
			}
		}
	}
	if err := firstErr(maxLen("title", d.Title, 60), maxLen("source", d.Source, 40),
		checkCount("items", len(d.Items), 0, 20)); err != nil {
		return err
	}
	if d.Average == nil && len(d.Items) == 0 {
		return ferr("items", "reviews needs an average or at least one item")
	}
	for i, it := range d.Items {
		p := fmt.Sprintf("items[%d]", i)
		if it.Stars < 1 || it.Stars > 5 {
			return ferr(p+".stars", "%s.stars must be 1 to 5", p)
		}
		if err := firstErr(checkText(p+".quote", it.Quote, 500), checkText(p+".author", it.Author, 60),
			checkWebURL(p+".source_url", it.SourceURL, false)); err != nil {
			return err
		}
	}
	return nil
}

// sellBlock returns the target value, validator and schedule getter for a P3-A type.
func sellBlock(typ string) (v any, check func() error, sched func() *Schedule, ok bool) {
	switch typ {
	case "product":
		d := &ProductData{}
		return d, func() error { d.ID, d.Category = "", ""; return checkProduct("", d) }, func() *Schedule { return d.Schedule }, true
	case "shop":
		d := &ShopData{}
		return d, func() error { return checkShop(d) }, func() *Schedule { return d.Schedule }, true
	case "marketplace":
		d := &MarketplaceData{}
		return d, func() error { return checkMarketplace(d) }, func() *Schedule { return d.Schedule }, true
	case "whatsapp":
		d := &WhatsAppData{}
		return d, func() error { return checkWhatsApp(d) }, func() *Schedule { return d.Schedule }, true
	case "price_list":
		d := &PriceListData{}
		return d, func() error { return checkPriceList(d) }, func() *Schedule { return d.Schedule }, true
	case "store_info":
		d := &StoreInfoData{}
		return d, func() error { return checkStoreInfo(d) }, func() *Schedule { return d.Schedule }, true
	case "payment":
		d := &PaymentData{}
		return d, func() error { return checkPayment(d) }, func() *Schedule { return d.Schedule }, true
	case "tip_jar":
		d := &TipJarData{}
		return d, func() error { return checkTipJar(d) }, func() *Schedule { return d.Schedule }, true
	case "reviews":
		d := &ReviewsData{}
		return d, func() error { return checkReviews(d) }, func() *Schedule { return d.Schedule }, true
	}
	return nil, nil, nil, false
}

// clickActionRE bounds meta.action on click events: buy, wa_order,
// copy_account, marketplace:shopee, tip:saweria, …
var clickActionRE = regexp.MustCompile(`^[a-z][a-z_]{0,23}(:[a-z0-9_]{1,32})?$`)
