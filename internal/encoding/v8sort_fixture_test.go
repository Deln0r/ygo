package encoding

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestV8Sort_Fixtures: the port of V8's sort gives the order Node's V8 gives
// arrays sorted with a comparator that, like yjs mergeUpdatesV2's, is not
// consistent at an equal key (testdata/v8-sort-fixtures.json).
func TestV8Sort_Fixtures(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "v8-sort-fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		V8    string `json:"v8"`
		Cases []struct {
			N     int      `json:"n"`
			Keys  int      `json:"keys"`
			Shape string   `json:"shape"`
			Input [][2]int `json:"input"`
			Order []int    `json:"order"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if len(fx.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range fx.Cases {
		idx := make([]int, len(c.Input))
		for i := range idx {
			idx[i] = i
		}
		v8Sort(idx, func(x, y int) int {
			a, b := c.Input[x], c.Input[y]
			if a[0] == b[0] {
				if a[1] == b[1] {
					return 0
				}
				return -1
			}
			return a[0] - b[0]
		})
		if !slices.Equal(idx, c.Order) {
			t.Fatalf("n %d, %d keys, %s: order\n got  %v\n want %v", c.N, c.Keys, c.Shape, idx, c.Order)
		}
	}
}
