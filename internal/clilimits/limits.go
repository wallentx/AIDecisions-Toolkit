// Package clilimits keeps numeric CLI diagnostics and help ranges in sync.
package clilimits

import (
	"flag"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"
)

// Range is inclusive unless OpenMin is set. NoMax preserves lower-only bounds.
// Durations are measured in seconds.
type Range struct {
	Min, Max float64
	OpenMin  bool
	NoMax    bool
}

type Table map[string]Range

func (r Range) String() string {
	if r.NoMax {
		return "at least " + number(r.Min)
	}
	if r.OpenMin {
		return "greater than " + number(r.Min) + " and at most " + number(r.Max)
	}
	return number(r.Min) + ".." + number(r.Max)
}

func (r Range) Check(label string, value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < r.Min || (r.OpenMin && value == r.Min) || (!r.NoMax && value > r.Max) {
		return fmt.Errorf("%s must be %s (got %s)", label, r, number(value))
	}
	return nil
}

// Help annotates registered flags. Table entries without a flag are config-only.
func (table Table) Help(flags *flag.FlagSet) {
	for name, bound := range table {
		if f := flags.Lookup(name); f != nil {
			unit := ""
			if _, ok := f.Value.(flag.Getter).Get().(time.Duration); ok {
				unit = " seconds"
			}
			f.Usage += " (" + bound.String() + unit + ")"
		}
	}
}

// Validate checks effective flag values after configuration has been applied.
// Sorted names ensure the first diagnostic is stable across runs.
func (table Table) Validate(flags *flag.FlagSet) error {
	names := make([]string, 0, len(table))
	for name := range table {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		f := flags.Lookup(name)
		if f == nil {
			continue
		}
		var value float64
		switch v := f.Value.(flag.Getter).Get().(type) {
		case int:
			value = float64(v)
		case int64:
			value = float64(v)
		case float64:
			value = v
		case time.Duration:
			value = v.Seconds()
		default:
			return fmt.Errorf("--%s has an unsupported numeric type", name)
		}
		if err := table[name].Check("--"+name, value); err != nil {
			return err
		}
	}
	return nil
}

func number(value float64) string { return strconv.FormatFloat(value, 'f', -1, 64) }
