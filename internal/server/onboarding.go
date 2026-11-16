package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const tempUsernamePrefix = "tmp."

var (
	onboardingIntents = set("local", "sell", "audience", "other")
	onboardingSources = set("import", "scratch", "instagram")
	stepRE            = regexp.MustCompile(`^[a-z_]{1,30}$`)
	nonUsernameChars  = regexp.MustCompile(`[^a-z0-9_.]+`)
	repeatedDots      = regexp.MustCompile(`\.{2,}`)
)

// Onboarding is stored in profiles.onboarding. username_claimed and
// suggested_username are server-managed.
type Onboarding struct {
	Completed         bool       `json:"completed"`
	CompletedAt       *time.Time `json:"completed_at,omitempty"`
	Step              string     `json:"step,omitempty"`
	Intent            string     `json:"intent,omitempty"`
	Source            string     `json:"source,omitempty"`
	UsernameClaimed   bool       `json:"username_claimed"`
	SuggestedUsername string     `json:"suggested_username,omitempty"`
}

func tempUsername() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return tempUsernamePrefix + string(b)
}

// suggestUsername derives an available username from an email's local part.
func (s *Server) suggestUsername(ctx context.Context, email string) string {
	local, _, _ := strings.Cut(strings.ToLower(email), "@")
	base := nonUsernameChars.ReplaceAllString(strings.ReplaceAll(local, "-", "."), ".")
	base = strings.Trim(repeatedDots.ReplaceAllString(base, "."), "._")
	if len(base) > 26 {
		base = strings.Trim(base[:26], "._")
	}
	for len(base) < 3 && base != "" {
		base += "1"
	}
	if base == "" {
		return ""
	}
	for i := 1; i <= 20; i++ {
		cand := base
		if i > 1 {
			cand = base + strconv.Itoa(i)
		}
		u, ok := validUsername(cand)
		if !ok {
			continue
		}
		taken, err := s.usernameTaken(ctx, u, "")
		if err == nil && !taken {
			return u
		}
	}
	return ""
}

// parseOnboardingPatch validates a client patch to profile.onboarding and
// returns the keys to merge.
func parseOnboardingPatch(raw json.RawMessage, now time.Time) (map[string]any, error) {
	var in struct {
		Completed *bool   `json:"completed"`
		Step      *string `json:"step"`
		Intent    *string `json:"intent"`
		Source    *string `json:"source"`
	}
	if !isJSONObject(raw) || json.Unmarshal(raw, &in) != nil {
		return nil, fmt.Errorf("onboarding must be {completed?, step?, intent?, source?}")
	}
	out := map[string]any{}
	if in.Step != nil {
		if *in.Step != "" && !stepRE.MatchString(*in.Step) {
			return nil, fmt.Errorf("onboarding.step must be a short lowercase id")
		}
		out["step"] = *in.Step
	}
	if in.Intent != nil {
		if err := oneOf("onboarding.intent", *in.Intent, onboardingIntents); err != nil {
			return nil, err
		}
		out["intent"] = *in.Intent
	}
	if in.Source != nil {
		if err := oneOf("onboarding.source", *in.Source, onboardingSources); err != nil {
			return nil, err
		}
		out["source"] = *in.Source
	}
	if in.Completed != nil {
		out["completed"] = *in.Completed
		if *in.Completed {
			out["completed_at"] = now.UTC()
		} else {
			out["completed_at"] = nil
		}
	}
	return out, nil
}
