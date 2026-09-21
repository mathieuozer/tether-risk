package chain

import "github.com/shopspring/decimal"

// Decimal is the value type used for USD amounts throughout the engine.
//
// docs/DECISIONS.md D5: float64 is not used for value arithmetic. SPEC.md §2
// requires that identical input produces an identical score and that every
// score be reconstructible from stored data. Binary floating point makes both
// harder to guarantee — the haircut formula multiplies many fractional shares
// together, and the order of accumulation would become observable in the
// result. Decimal arithmetic keeps the numbers exactly what a reviewer would
// get recomputing them by hand from the stored path set.
//
// Floats appear only at the presentation boundary.
type Decimal = decimal.Decimal

// NewDecimal builds a Decimal from an unscaled integer and exponent.
func NewDecimal(value int64, exp int32) Decimal { return decimal.New(value, exp) }

// DecimalFromString parses a decimal string.
func DecimalFromString(s string) (Decimal, error) { return decimal.NewFromString(s) }

// ZeroDecimal is the additive identity.
func ZeroDecimal() Decimal { return decimal.Zero }
