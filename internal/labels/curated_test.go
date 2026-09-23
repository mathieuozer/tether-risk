package labels

import (
	"path/filepath"
	"testing"

	"github.com/mozer/tether-risk/internal/config"
)

// The shipped curated list must load: every entry with evidence, a known
// category and a valid address. The file's header promised a build-time
// check that did not exist; this is it (docs/DECISIONS.md D37).
func TestShippedCuratedLabelsLoad(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "config"))
	if err != nil {
		t.Fatal(err)
	}
	ls, err := LoadCurated(filepath.Join("..", "..", "config", "curated_labels.yaml"), NewResolver(cfg).ValidateCategory)
	if err != nil {
		t.Fatal(err)
	}
	var terror int
	for _, l := range ls {
		if l.Chain == "tron" && !ValidTronAddress(l.Address) {
			t.Errorf("%s: not a valid TRON address", l.Address)
		}
		if l.Category == "terrorist_financing" {
			terror++
		}
	}
	if terror < 49 {
		t.Errorf("terrorist_financing entries = %d; the DOJ affidavits give 49", terror)
	}
}
