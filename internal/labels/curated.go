package labels

import (
	"fmt"
	"os"
	"strings"

	"github.com/mozer/tether-risk/internal/chain/tron"
	"gopkg.in/yaml.v3"
)

// Curated in-repo labels (SPEC.md §6.5).
//
// This is the only source a human writes by hand, which makes it the one where
// a wrong entry is most dangerous: curated labels carry high confidence,
// terminate traversal (SPEC.md §7), and will be believed. Validation here is
// therefore stricter than for machine-generated sources — in particular every
// entry must carry evidence a reviewer can check independently.

type curatedFile struct {
	Version int           `yaml:"version"`
	Labels  []curatedItem `yaml:"labels"`
}

type curatedItem struct {
	Chain    string `yaml:"chain"`
	Address  string `yaml:"address"`
	Entity   string `yaml:"entity"`
	Category string `yaml:"category"`
	Evidence string `yaml:"evidence"`
	Verified string `yaml:"verified"`
	Note     string `yaml:"note"`
}

// LoadCurated reads and validates the curated label file.
//
// `validCategory` is injected rather than imported so this stays testable
// without a full config; in production it is Resolver.ValidateCategory.
func LoadCurated(path string, validCategory func(string) error) ([]Label, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read curated labels: %w", err)
	}

	var f curatedFile
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parse curated labels: %w", err)
	}

	var (
		out  []Label
		errs []string
		seen = map[string]bool{}
	)

	for i, item := range f.Labels {
		where := fmt.Sprintf("labels[%d] (%s)", i, item.Address)

		switch {
		case item.Address == "":
			errs = append(errs, where+": address is required")
			continue
		case item.Chain == "":
			errs = append(errs, where+": chain is required")
			continue
		case item.Entity == "":
			errs = append(errs, where+": entity is required")
		case item.Category == "":
			errs = append(errs, where+": category is required")
		}

		// The rule that matters. An unverifiable curated label is a claim
		// nobody can check, carrying high confidence into every score that
		// touches it.
		if item.Evidence == "" {
			errs = append(errs, where+": evidence URL is required; a curated label "+
				"nobody can independently verify must not carry high confidence")
		}
		if item.Verified == "" {
			errs = append(errs, where+": verified date is required")
		}

		if item.Category != "" && validCategory != nil {
			if err := validCategory(item.Category); err != nil {
				errs = append(errs, where+": "+err.Error())
			}
		}

		canonical, err := canonicalise(item.Chain, item.Address)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", where, err))
			continue
		}

		key := item.Chain + "/" + canonical
		if seen[key] {
			errs = append(errs, where+": duplicate entry for this address")
			continue
		}
		seen[key] = true

		out = append(out, Label{
			Chain:      item.Chain,
			Address:    canonical,
			Entity:     item.Entity,
			Category:   item.Category,
			Confidence: 0.95,
			Source:     "curated",
			Evidence: map[string]any{
				"evidence_url": item.Evidence,
				"verified":     item.Verified,
				"note":         item.Note,
				"listed_as":    item.Address,
			},
		})
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid curated labels:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return out, nil
}

// ValidTronAddress is exported for the curated-file build check.
func ValidTronAddress(addr string) bool { return tron.IsValid(addr) }
