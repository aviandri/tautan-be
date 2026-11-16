package server

import (
	"testing"
)

func TestSeedHeavy(t *testing.T) {
	s, h := newTestServer(t)
	s.cfg.UploadDir = t.TempDir()
	for _, full := range []bool{false, true} {
		u, err := s.SeedHeavy(t.Context(), full)
		if err != nil {
			t.Fatal(err)
		}
		if !full {
			if _, err := s.SeedHeavy(t.Context(), full); err != nil { // re-seeding replaces the page
				t.Fatalf("reseed: %v", err)
			}
		}
		var page pageResponse
		do(t, h, "GET", "/api/v1/public/"+u, nil).expect(200).json(&page)
		types := map[string]bool{}
		for _, b := range page.Blocks {
			types[b.Type] = true
		}
		want := 19 // the 13 baseline blocks use 10 types, plus 9 sell types
		if full {
			want = len(blockTypes) // every type
		}
		if len(types) != want {
			t.Errorf("%s: %d distinct types, want %d: %v", u, len(types), want, types)
		}
		if !full && len(page.Blocks) != 22 {
			t.Errorf("heavy has %d blocks, want 13 + 9", len(page.Blocks))
		}
		ready := 0
		for _, m := range page.Media {
			if len(m.Widths) > 0 {
				ready++
			}
		}
		if ready < 5 {
			t.Errorf("%s: %d images with variants ready (media %v)", u, ready, page.Media)
		}
	}
}
