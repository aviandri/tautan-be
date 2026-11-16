package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

// Card-level gates. A gate lives in blocks.gate (not in data), so a password
// hash can never reach a page read. Public reads replace a gated card's data
// (and its folder children's) with a locked stub, which keeps cached pages
// safe; POST /public/{u}/blocks/{id}/unlock returns the data plus a signed
// token the FE keeps and sends back (?unlock=) to re-open the card or use its
// endpoints. Changing the gate invalidates old tokens.

type gateStored struct {
	Kind           string `json:"kind"` // password | email | age_18 | follow
	Message        string `json:"message,omitempty"`
	PasswordHash   string `json:"password_hash,omitempty"`
	FollowPlatform string `json:"follow_platform,omitempty"`
	FollowURL      string `json:"follow_url,omitempty"`
	Subscribe      string `json:"subscribe,omitempty"` // email: everyone | ask | none
}

type GateFollow struct {
	Platform string `json:"platform"`
	URL      string `json:"url"`
}

// GateView is what reads expose: the editor also gets subscribe and has_password.
type GateView struct {
	Kind        string      `json:"kind"`
	Message     string      `json:"message,omitempty"`
	Follow      *GateFollow `json:"follow,omitempty"`
	Subscribe   string      `json:"subscribe,omitempty"`
	HasPassword bool        `json:"has_password,omitempty"`
}

var gateKinds = set("password", "email", "age_18", "follow")

const unlockTTL = 30 * 24 * time.Hour

func (b *Block) gate() *gateStored {
	if len(b.gateRaw) == 0 || string(b.gateRaw) == "null" {
		return nil
	}
	var g gateStored
	if json.Unmarshal(b.gateRaw, &g) != nil || g.Kind == "" {
		return nil
	}
	return &g
}

// setGateView fills b.Gate for owner (full) or public reads.
func (b *Block) setGateView(public bool) {
	g := b.gate()
	if g == nil {
		b.Gate = nil
		return
	}
	v := &GateView{Kind: g.Kind, Message: g.Message}
	if g.FollowURL != "" {
		v.Follow = &GateFollow{Platform: g.FollowPlatform, URL: g.FollowURL}
	}
	if !public {
		v.Subscribe, v.HasPassword = g.Subscribe, g.PasswordHash != ""
	}
	b.Gate = v
}

func gated(b Block) bool { return b.gate() != nil }

// parseGate validates a gate from a create/patch body. null removes it. On a
// password gate the password may be omitted to keep the current one.
func parseGate(raw json.RawMessage, prev *gateStored) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil, nil
	}
	var in struct {
		Kind      string      `json:"kind"`
		Message   string      `json:"message"`
		Password  *string     `json:"password"`
		Follow    *GateFollow `json:"follow"`
		Subscribe string      `json:"subscribe"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, ferr("gate", "gate must be an object or null")
	}
	if !gateKinds[in.Kind] {
		return nil, ferr("gate.kind", "gate.kind must be password, email, age_18 or follow")
	}
	if err := maxLen("gate.message", in.Message, 200); err != nil {
		return nil, err
	}
	g := gateStored{Kind: in.Kind, Message: strings.TrimSpace(in.Message)}
	switch in.Kind {
	case "password":
		switch {
		case in.Password != nil:
			p := *in.Password
			if len([]rune(p)) < 4 || len(p) > 72 {
				return nil, ferr("gate.password", "gate.password must be 4 to 72 characters")
			}
			h, err := bcrypt.GenerateFromPassword([]byte(p), bcrypt.DefaultCost)
			if err != nil {
				return nil, err
			}
			g.PasswordHash = string(h)
		case prev != nil && prev.PasswordHash != "":
			g.PasswordHash = prev.PasswordHash
		default:
			return nil, ferr("gate.password", "gate.password is required")
		}
	case "email":
		g.Subscribe = in.Subscribe
		if g.Subscribe == "" {
			g.Subscribe = "ask"
		}
		if err := oneOf("gate.subscribe", g.Subscribe, subscribeModes); err != nil {
			return nil, err
		}
	case "follow":
		if in.Follow == nil {
			return nil, ferr("gate.follow", "gate.follow {platform, url} is required")
		}
		if err := firstErr(checkText("gate.follow.platform", in.Follow.Platform, 30), checkWebURL("gate.follow.url", in.Follow.URL, true)); err != nil {
			return nil, err
		}
		if !platformRE.MatchString(in.Follow.Platform) {
			return nil, ferr("gate.follow.platform", "gate.follow.platform must be a lowercase id such as instagram")
		}
		g.FollowPlatform, g.FollowURL = in.Follow.Platform, strings.TrimSpace(in.Follow.URL)
	}
	return json.Marshal(g)
}

// gateFingerprint changes whenever the gate does, so old unlock tokens die.
func gateFingerprint(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:6])
}

func (s *Server) unlockToken(b Block, exp time.Time) string {
	payload := strconv.FormatInt(exp.Unix(), 10) + "." + b.ID + "." + gateFingerprint(b.gateRaw)
	return payload + "." + s.sign("unlock", payload)
}

func (s *Server) validUnlockToken(tok string, b Block) bool {
	parts := strings.Split(tok, ".")
	if len(parts) != 4 || parts[1] != b.ID || parts[2] != gateFingerprint(b.gateRaw) {
		return false
	}
	if !hmac.Equal([]byte(parts[3]), []byte(s.sign("unlock", strings.Join(parts[:3], ".")))) {
		return false
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	return err == nil && s.now().Unix() <= exp
}

// unlocked reports whether the request carries a valid unlock token for b
// (?unlock=<token>), for b's own gate or its gated folder's.
func (s *Server) unlocked(r *http.Request, b Block) bool {
	tok := r.URL.Query().Get("unlock")
	if tok == "" {
		return false
	}
	if gated(b) {
		return s.validUnlockToken(tok, b)
	}
	if b.ParentID != nil {
		if parent, err := scanBlock(s.db.QueryRow(r.Context(), `SELECT `+blockCols+` FROM blocks b WHERE b.id = $1`, *b.ParentID)); err == nil {
			return s.validUnlockToken(tok, parent)
		}
	}
	return false
}

// lockedByGate reports whether b is gated itself or sits in a gated folder.
func (s *Server) lockedByGate(ctx context.Context, b Block) bool {
	if gated(b) {
		return true
	}
	if b.ParentID == nil {
		return false
	}
	var raw []byte
	if err := s.db.QueryRow(ctx, `SELECT gate FROM blocks WHERE id = $1`, *b.ParentID).Scan(&raw); err != nil {
		return false
	}
	p := Block{gateRaw: raw}
	return gated(p)
}

// gateOK writes 403 locked unless b is ungated or the request unlocks it.
func (s *Server) gateOK(w http.ResponseWriter, r *http.Request, b Block) bool {
	if s.lockedByGate(r.Context(), b) && !s.unlocked(r, b) {
		writeError(w, http.StatusForbidden, "locked", "unlock this card first")
		return false
	}
	return true
}

// lockBlocks stubs gated cards and children of gated folders in public reads,
// except ids in open.
func lockBlocks(blocks []Block, open map[string]bool) {
	lockedFolder := map[string]bool{}
	for _, b := range blocks {
		if gated(b) && !open[b.ID] {
			lockedFolder[b.ID] = true
		}
	}
	for i := range blocks {
		b := &blocks[i]
		b.setGateView(true)
		if (gated(*b) && !open[b.ID]) || (b.ParentID != nil && lockedFolder[*b.ParentID]) {
			b.Data, b.Locked = json.RawMessage("null"), true
		}
	}
}

// handleUnlock checks the credential for a gated card and returns its data
// (and its children, for a folder) with a token to re-open it later.
func (s *Server) handleUnlock(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token      string `json:"token"`
		Password   string `json:"password"`
		Email      string `json:"email"`
		Name       string `json:"name"`
		Subscribe  bool   `json:"subscribe"`
		ConfirmAge bool   `json:"confirm_age"`
		Followed   bool   `json:"followed"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	ctx := r.Context()
	pid, err := s.publishedProfileID(ctx, chi.URLParam(r, "username"))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "page not found")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	b, err := scanBlock(s.db.QueryRow(ctx, `SELECT `+blockCols+` FROM blocks b
		WHERE b.id = $1 AND b.profile_id = $2 AND b.is_visible AND b.deleted_at IS NULL`, chi.URLParam(r, "id"), pid))
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) || (err == nil && !blockSchedule(b.Data).activeAt(s.now())) {
		writeError(w, http.StatusNotFound, "not_found", "card not found on this page")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	g := b.gate()
	if g == nil {
		writeError(w, http.StatusBadRequest, "not_gated", "this card is not locked")
		return
	}
	if in.Token == "" || !s.validUnlockToken(in.Token, b) {
		if !s.unlockLimiter.Allow(clientIP(r) + "|" + b.ID) {
			writeError(w, http.StatusTooManyRequests, "rate_limited", "too many attempts, try again later")
			return
		}
		switch g.Kind {
		case "password":
			if bcrypt.CompareHashAndPassword([]byte(g.PasswordHash), []byte(in.Password)) != nil {
				writeError(w, http.StatusForbidden, "wrong_password", "that password is not right")
				return
			}
		case "email":
			ci := contactIn{Email: in.Email, Name: in.Name, Subscribe: in.Subscribe}
			if !ci.valid(w) {
				return
			}
			sub := g.Subscribe == "everyone" || (g.Subscribe == "ask" && ci.Subscribe)
			if _, err := upsertContact(ctx, s.db, pid, ci.Email, ci.Name, sub, b.ID, s.now()); err != nil {
				s.internalError(w, err)
				return
			}
		case "age_18":
			if !in.ConfirmAge {
				writeError(w, http.StatusBadRequest, "confirm_age", "confirm_age must be true")
				return
			}
		case "follow":
			if !in.Followed {
				writeError(w, http.StatusBadRequest, "followed", "followed must be true")
				return
			}
		}
	}
	page, err := s.loadPageOpen(ctx, pid, map[string]bool{b.ID: true})
	if err != nil {
		s.internalError(w, err)
		return
	}
	var card *Block
	children := []Block{}
	for i := range page.Blocks {
		pb := page.Blocks[i]
		if pb.ID == b.ID {
			card = &pb
		} else if pb.ParentID != nil && *pb.ParentID == b.ID {
			children = append(children, pb)
		}
	}
	if card == nil { // e.g. inside a folder that is itself hidden
		writeError(w, http.StatusNotFound, "not_found", "card not found on this page")
		return
	}
	media, err := s.pageMedia(ctx, pageResponse{Blocks: append([]Block{*card}, children...)})
	if err != nil {
		s.internalError(w, err)
		return
	}
	exp := s.now().Add(unlockTTL)
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"block":        card,
		"children":     children,
		"media":        media,
		"unlock_token": s.unlockToken(b, exp),
		"expires_at":   exp.UTC().Truncate(time.Second),
	})
}
