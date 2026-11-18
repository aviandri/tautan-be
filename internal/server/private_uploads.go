package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"path"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Paid deliverables (payments b) live under private names, "p_<32 hex>.<ext>".
// /uploads never serves them (storedNameRE doesn't match); only the signed
// order download does. private_uploads records which profile owns each one, so
// a card can't point at another seller's private file.

var privateNameRE = regexp.MustCompile(`^p_[0-9a-f]{32}\.[a-z0-9]{2,5}$`)

func newPrivateName(ext string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "p_" + hex.EncodeToString(b) + ext
}

// uploadName returns the stored name for one of our upload URLs (absolute or
// /uploads/ path), or "" for anything else.
func (s *Server) uploadName(u string) string {
	local := strings.TrimPrefix(strings.TrimSpace(u), s.cfg.PublicBaseURL)
	if !strings.HasPrefix(local, "/uploads/") {
		return ""
	}
	return path.Base(strings.SplitN(local, "?", 2)[0])
}

type execQueryRower interface {
	queryRower
	execer
}

// recordPrivate records that pid owns the private upload name.
func (s *Server) recordPrivate(ctx context.Context, q execer, pid, name string) error {
	_, err := q.Exec(ctx, `INSERT INTO private_uploads (name, profile_id) VALUES ($1, $2)`, name, pid)
	return err
}

// privatize returns a private URL for a deliverable: a private file this
// profile owns stays; a public upload is copied to a new private name (the
// copy's name has never been published); external URLs are left alone.
func (s *Server) privatize(ctx context.Context, q execQueryRower, pid, field, u string) (string, error) {
	name := s.uploadName(u)
	switch {
	case name == "":
		return u, nil
	case privateNameRE.MatchString(name):
		var owner string
		err := q.QueryRow(ctx, `SELECT profile_id FROM private_uploads WHERE name = $1`, name).Scan(&owner)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && owner != pid) {
			return "", ferr(field, "%s: file not found; upload it again", field)
		}
		return u, err
	case storedNameRE.MatchString(name):
		f, err := s.uploads().Open(ctx, name)
		if err != nil {
			return "", ferr(field, "%s: file not found; upload it again", field)
		}
		defer f.Close()
		ext := path.Ext(name)
		priv := newPrivateName(ext)
		if err := s.uploads().Put(ctx, priv, f, mime.TypeByExtension(ext)); err != nil {
			return "", fmt.Errorf("copy deliverable: %w", err)
		}
		if err := s.recordPrivate(ctx, q, pid, priv); err != nil {
			return "", err
		}
		return s.cfg.PublicBaseURL + "/uploads/" + priv, nil
	}
	return "", ferr(field, "%s: file not found; upload it again", field)
}

// privatizeDeliverables rewrites the deliverable URLs of priced cards (file
// with a price, checkout products and shop items) to private copies.
func (s *Server) privatizeDeliverables(ctx context.Context, q execQueryRower, pid, typ string, data json.RawMessage) (json.RawMessage, error) {
	var m map[string]any
	switch typ {
	case "file", "product", "shop":
		if json.Unmarshal(data, &m) != nil {
			return data, nil
		}
	default:
		return data, nil
	}
	changed := false
	fix := func(obj map[string]any, key, field string) error {
		u, _ := obj[key].(string)
		if u == "" {
			return nil
		}
		nu, err := s.privatize(ctx, q, pid, field, u)
		if err != nil {
			return err
		}
		if nu != u {
			obj[key], changed = nu, true
		}
		return nil
	}
	product := func(p map[string]any, prefix string) error {
		a, _ := p["action"].(map[string]any)
		if a == nil || a["kind"] != "checkout" {
			return nil
		}
		if d, ok := a["download"].(map[string]any); ok {
			return fix(d, "url", prefix+"action.download.url")
		}
		return nil
	}
	var err error
	switch typ {
	case "file":
		if m["price"] != nil {
			err = fix(m, "url", "url")
		}
	case "product":
		err = product(m, "")
	case "shop":
		items, _ := m["items"].([]any)
		for i, it := range items {
			if p, ok := it.(map[string]any); ok {
				if err = product(p, fmt.Sprintf("items[%d].", i)); err != nil {
					break
				}
			}
		}
	}
	if err != nil || !changed {
		return data, err
	}
	return json.Marshal(m)
}

// dataErrOr maps field errors to 400 invalid_data and passes others through.
func dataErrOr(err error) error {
	var fe *fieldError
	if errors.As(err, &fe) {
		return dataErr(err)
	}
	return err
}
