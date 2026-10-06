package web

import (
	"testing"

	"github.com/msyavuz/revq/internal/store"
)

func TestBoardFilterMatch(t *testing.T) {
	pr := store.PR{Repo: "acme/atlas", Author: "Dana-Okafor", Number: 806, Title: "fix(auth): expire refresh tokens"}
	cases := []struct {
		name string
		f    boardFilter
		want bool
	}{
		{"no filter", boardFilter{}, true},
		{"repo", boardFilter{Repo: "acme/atlas"}, true},
		{"other repo", boardFilter{Repo: "acme/other"}, false},
		{"author ignores case", boardFilter{Author: "dana-okafor"}, true},
		{"other author", boardFilter{Author: "priya-n"}, false},
		{"title word", boardFilter{Q: "refresh"}, true},
		{"all words must match", boardFilter{Q: "refresh geocoder"}, false},
		{"words in any order, any case", boardFilter{Q: "Tokens AUTH"}, true},
		{"number", boardFilter{Q: "806"}, true},
		{"number with hash", boardFilter{Q: "#806"}, true},
		{"wrong number", boardFilter{Q: "#807"}, false},
		{"combined", boardFilter{Repo: "acme/atlas", Author: "dana-okafor", Q: "auth"}, true},
		{"combined, one miss", boardFilter{Repo: "acme/atlas", Author: "priya-n", Q: "auth"}, false},
	}
	for _, c := range cases {
		if got := c.f.match(pr); got != c.want {
			t.Errorf("%s: match = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestBoardFilterQuery(t *testing.T) {
	if q := (boardFilter{}).Query(); q != "" {
		t.Errorf("empty filter query = %q, want empty", q)
	}
	got := boardFilter{Repo: "acme/atlas", Q: "fix auth"}.Query()
	if want := "?q=fix+auth&repo=acme%2Fatlas"; got != want {
		t.Errorf("query = %q, want %q", got, want)
	}
}
