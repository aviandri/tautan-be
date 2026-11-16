package server

import (
	"encoding/csv"
	"strings"
	"testing"
)

const testFormData = `{"title":"Work with me","success_message":"Thanks!","subscribe":"ask","fields":[
 {"id":"name","type":"text","label":"Your name","required":true},
 {"id":"email","type":"email","label":"Email","required":true},
 {"id":"phone","type":"phone","label":"Phone"},
 {"id":"budget","type":"select","label":"Budget","options":["<1k","1k-5k"]},
 {"id":"size","type":"number","label":"Team size"},
 {"id":"start","type":"date","label":"Start date"},
 {"id":"consent","type":"checkbox","label":"I agree","required":true},
 {"id":"msg","type":"textarea","label":"Message"}]}`

func TestFormBlockValidation(t *testing.T) {
	_, h := newTestServer(t)
	a := signup(t, h, "alice")
	tab := editor(t, h, a.AccessToken).Tabs[0]
	post := func(data string) resp {
		return do(t, h, "POST", "/api/v1/blocks", `{"tab_id":"`+tab.ID+`","type":"form","data":`+data+`}`, bearer(a.AccessToken))
	}
	post(testFormData).expect(201)
	for _, bad := range []string{
		`{"fields":[]}`,
		`{"fields":[{"id":"a","type":"color","label":"x"}]}`,
		`{"fields":[{"id":"a","type":"text","label":""}]}`,
		`{"fields":[{"id":"a","type":"text","label":"x"},{"id":"a","type":"text","label":"y"}]}`,
		`{"fields":[{"id":"Bad Id","type":"text","label":"x"}]}`,
		`{"fields":[{"id":"a","type":"select","label":"x","options":[]}]}`,
		`{"subscribe":"maybe","fields":[{"id":"a","type":"text","label":"x"}]}`,
	} {
		if r := post(bad); r.Code != 400 {
			t.Errorf("%s: %d", bad, r.Code)
		}
	}
}

func TestFormSubmissionsAndAudience(t *testing.T) {
	s, h := newTestServer(t)
	f := setupPage(t, h, "alice", true)
	tok := bearer(f.tok)
	s.formLimiter = newRateLimiter(1000, 60e9)
	s.contactLimiter = newRateLimiter(1000, 60e9)
	var form, signupBlock Block
	do(t, h, "POST", "/api/v1/blocks", `{"tab_id":"`+f.tab.ID+`","type":"form","data":`+testFormData+`}`, tok).expect(201).json(&form)
	do(t, h, "POST", "/api/v1/blocks", `{"tab_id":"`+f.tab.ID+`","type":"email_signup","data":{"title":"Newsletter","success_message":"You're in"}}`, tok).expect(201).json(&signupBlock)

	submit := func(body string) resp { return do(t, h, "POST", "/api/v1/public/alice/form", body) }

	// Validation with per-field details.
	r := submit(`{"block_id":"` + form.ID + `","answers":{"email":"nope","budget":"10k","size":"many","start":"31/12/2026","phone":"call me"}}`).expect(400)
	var e struct {
		Error struct {
			Code    string
			Details map[string]string
		}
	}
	r.json(&e)
	for _, k := range []string{"name", "email", "budget", "size", "start", "phone", "consent"} {
		if e.Error.Details[k] == "" {
			t.Errorf("missing detail for %s: %v", k, e.Error.Details)
		}
	}
	if e.Error.Code != "invalid_answers" {
		t.Errorf("code = %s", e.Error.Code)
	}

	var ok struct {
		ID             string `json:"id"`
		SuccessMessage string `json:"success_message"`
	}
	submit(`{"block_id":"` + form.ID + `","answers":{"name":"Vee","email":"Vee@Example.com","budget":"1k-5k","size":"12","start":"2026-12-01","consent":true,"msg":"=HYPERLINK(\"x\")","junk":"dropped"},"subscribe":false}`).
		expect(201).json(&ok)
	if ok.SuccessMessage != "Thanks!" || ok.ID == "" {
		t.Errorf("submit = %+v", ok)
	}
	// Same person later opts in; then uses the email signup and the contact form.
	submit(`{"block_id":"` + form.ID + `","answers":{"name":"Vee","email":"vee@example.com","consent":true},"subscribe":true}`).expect(201)
	do(t, h, "POST", "/api/v1/public/alice/subscribe", `{"block_id":"`+signupBlock.ID+`","email":"VEE@example.com"}`).expect(201)
	do(t, h, "POST", "/api/v1/public/alice/contact", map[string]any{"block_id": f.form.ID, "name": "Vee", "email": "vee@example.com", "message": "hi"}).expect(201)
	// Another person via signup only.
	var sub struct {
		SuccessMessage string `json:"success_message"`
	}
	do(t, h, "POST", "/api/v1/public/alice/subscribe", `{"block_id":"`+signupBlock.ID+`","email":"z@example.com","name":"Zed"}`).expect(201).json(&sub)
	if sub.SuccessMessage != "You're in" {
		t.Errorf("signup msg = %q", sub.SuccessMessage)
	}
	// Wrong block types / bad input.
	do(t, h, "POST", "/api/v1/public/alice/form", `{"block_id":"`+signupBlock.ID+`","answers":{}}`).expect(400)
	do(t, h, "POST", "/api/v1/public/alice/subscribe", `{"block_id":"`+form.ID+`","email":"a@b.co"}`).expect(400)
	do(t, h, "POST", "/api/v1/public/alice/subscribe", `{"block_id":"`+signupBlock.ID+`","email":"bad"}`).expect(400)
	do(t, h, "POST", "/api/v1/public/nobody/form", `{"block_id":"`+form.ID+`","answers":{}}`).expect(404)

	// Forms list.
	var forms struct{ Items []formSummary }
	do(t, h, "GET", "/api/v1/forms", nil, tok).expect(200).json(&forms)
	counts := map[string]int{}
	for _, it := range forms.Items {
		counts[it.BlockID] = it.Responses
	}
	if counts[form.ID] != 2 || counts[signupBlock.ID] != 2 || len(forms.Items) != 2 {
		t.Errorf("forms = %+v", forms.Items)
	}

	// Submissions list.
	var subs struct {
		Items      []FormSubmission
		NextCursor *string `json:"next_cursor"`
	}
	do(t, h, "GET", "/api/v1/forms/"+form.ID+"/submissions", nil, tok).expect(200).json(&subs)
	if len(subs.Items) != 2 || subs.NextCursor != nil || subs.Items[1].ContactID == nil {
		t.Fatalf("subs = %+v", subs)
	}
	if !strings.Contains(string(subs.Items[1].Answers), `"value":12`) || strings.Contains(string(subs.Items[1].Answers), "junk") {
		t.Errorf("answers = %s", subs.Items[1].Answers)
	}

	// CSV export with formula escaping.
	r = do(t, h, "GET", "/api/v1/forms/"+form.ID+"/submissions.csv", nil, tok).expect(200)
	if !strings.HasPrefix(r.Header().Get("Content-Type"), "text/csv") {
		t.Errorf("csv content-type = %s", r.Header().Get("Content-Type"))
	}
	recs, err := csv.NewReader(strings.NewReader(r.Body.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(recs[0], "|") != "Submitted at|Your name|Email|Phone|Budget|Team size|Start date|I agree|Message" || len(recs) != 3 {
		t.Fatalf("csv header/rows = %v", recs)
	}
	last := recs[2]
	if last[1] != "Vee" || last[5] != "12" || last[7] != "yes" || last[8] != `'=HYPERLINK("x")` {
		t.Errorf("csv row = %v", last)
	}

	// Audience: two contacts, Vee deduped across 3 sources and 4 responses, subscribed (sticky).
	var aud struct {
		Items  []Contact
		Counts map[string]int
	}
	do(t, h, "GET", "/api/v1/audience", nil, tok).expect(200).json(&aud)
	if aud.Counts["all"] != 2 || aud.Counts["subscribed"] != 2 || len(aud.Items) != 2 {
		t.Fatalf("audience = %+v", aud)
	}
	var vee Contact
	for _, c := range aud.Items {
		if c.Email == "vee@example.com" {
			vee = c
		}
	}
	if vee.Name != "Vee" || !vee.Subscribed || vee.SubscribedAt == nil || vee.Responses != 4 || len(vee.Sources) != 3 {
		t.Errorf("vee = %+v", vee)
	}
	do(t, h, "GET", "/api/v1/audience?status=unsubscribed", nil, tok).expect(200).json(&aud)
	if len(aud.Items) != 0 {
		t.Errorf("unsubscribed = %d", len(aud.Items))
	}
	do(t, h, "GET", "/api/v1/audience?q=zed", nil, tok).expect(200).json(&aud)
	if len(aud.Items) != 1 || aud.Items[0].Email != "z@example.com" {
		t.Errorf("search = %+v", aud.Items)
	}
	do(t, h, "GET", "/api/v1/audience?status=weird", nil, tok).expect(400)

	var detail struct {
		Contact  Contact
		Activity []activity
	}
	do(t, h, "GET", "/api/v1/audience/"+vee.ID, nil, tok).expect(200).json(&detail)
	kinds := map[string]int{}
	for _, a := range detail.Activity {
		kinds[a.Kind]++
	}
	if kinds["form"] != 3 || kinds["contact"] != 1 {
		t.Errorf("activity = %v", kinds)
	}

	r = do(t, h, "GET", "/api/v1/audience.csv?status=subscribed", nil, tok).expect(200)
	recs, _ = csv.NewReader(strings.NewReader(r.Body.String())).ReadAll()
	if len(recs) != 3 || recs[0][0] != "Email" {
		t.Errorf("audience csv = %v", recs)
	}
	r = do(t, h, "GET", "/api/v1/contact-submissions.csv", nil, tok).expect(200)
	recs, _ = csv.NewReader(strings.NewReader(r.Body.String())).ReadAll()
	if len(recs) != 2 || recs[1][3] != "hi" {
		t.Errorf("contact csv = %v", recs)
	}

	// Isolation.
	b := signup(t, h, "bob")
	do(t, h, "GET", "/api/v1/forms/"+form.ID+"/submissions", nil, bearer(b.AccessToken)).expect(404)
	do(t, h, "GET", "/api/v1/audience/"+vee.ID, nil, bearer(b.AccessToken)).expect(404)
	do(t, h, "GET", "/api/v1/audience", nil, bearer(b.AccessToken)).expect(200).json(&aud)
	if aud.Counts["all"] != 0 {
		t.Error("bob sees alice's contacts")
	}

	// Rate limit.
	s.formLimiter = newRateLimiter(1, 60e9)
	do(t, h, "POST", "/api/v1/public/alice/subscribe", `{"block_id":"`+signupBlock.ID+`","email":"r1@example.com"}`).expect(201)
	do(t, h, "POST", "/api/v1/public/alice/subscribe", `{"block_id":"`+signupBlock.ID+`","email":"r2@example.com"}`).expect(429)
}

func TestFormCreateContactsOff(t *testing.T) {
	s, h := newTestServer(t)
	f := setupPage(t, h, "alice", true)
	s.formLimiter = newRateLimiter(1000, 60e9)
	var form Block
	do(t, h, "POST", "/api/v1/blocks", `{"tab_id":"`+f.tab.ID+`","type":"form","data":{"create_contacts":false,"fields":[{"id":"email","type":"email","label":"Email"}]}}`, bearer(f.tok)).expect(201).json(&form)
	do(t, h, "POST", "/api/v1/public/alice/form", `{"block_id":"`+form.ID+`","answers":{"email":"x@example.com"}}`).expect(201)
	var n int
	testPool.QueryRow(t.Context(), `SELECT count(*) FROM contacts`).Scan(&n)
	if n != 0 {
		t.Errorf("contacts = %d, want 0", n)
	}
}
