package server

import (
	"encoding/json"
	"time"
)

type User struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

type Profile struct {
	ID          string          `json:"id"`
	UserID      string          `json:"user_id"`
	Username    string          `json:"username"`
	DisplayName string          `json:"display_name"`
	Bio         string          `json:"bio"`
	AvatarURL   *string         `json:"avatar_url"`
	Theme       json.RawMessage `json:"theme"`
	SEO         json.RawMessage `json:"seo"`
	IsPublished bool            `json:"is_published"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

type Tab struct {
	ID        string `json:"id"`
	ProfileID string `json:"profile_id"`
	Title     string `json:"title"`
	Slug      string `json:"slug"`
	Position  int    `json:"position"`
}

type Block struct {
	ID        string          `json:"id"`
	ProfileID string          `json:"profile_id"`
	TabID     string          `json:"tab_id"`
	ParentID  *string         `json:"parent_id"`
	Type      string          `json:"type"`
	Position  int             `json:"position"`
	IsVisible bool            `json:"is_visible"`
	Data      json.RawMessage `json:"data"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// DefaultTheme is applied to new profiles; it must satisfy the full Theme shape.
var DefaultTheme = json.RawMessage(`{"preset":"classic","bg":{"type":"color","value":"#f7f5f0"},"button":{"style":"fill","radius":12,"color":"#1f2937","text_color":"#ffffff"},"font":"Inter","text_color":"#1f2937"}`)

const profileCols = `id, user_id, username, display_name, bio, NULLIF(avatar_url, ''), theme, seo, is_published, created_at, updated_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanProfile(r rowScanner) (Profile, error) {
	var p Profile
	err := r.Scan(&p.ID, &p.UserID, &p.Username, &p.DisplayName, &p.Bio, &p.AvatarURL, &p.Theme, &p.SEO, &p.IsPublished, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}
