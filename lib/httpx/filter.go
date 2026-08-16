package httpx

// FilterOption is one selectable value of a list filter, exposed by the envelope
// (docs/erd-backend.md §4e.3) so the FE can render the filter chips without knowing
// the selectable set a priori. Label is what the FE displays (the localizable string);
// Value is the query-param value the FE echoes back. For closed enums label == value
// (the FE localizes the label); for free-text options the DB value is the display.
type FilterOption struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// Filters maps a query-param key → its selectable options. It is additive: a key is
// present only when the list has options for it (zero options → key omitted), so the
// serialized object is {} — never null — for a list with no filters.
type Filters map[string][]FilterOption

// Set assigns key's options, skipping an empty option set so a list with no values
// for a filter omits the key rather than carrying a null/empty array.
func (f Filters) Set(key string, opts ...FilterOption) {
	if len(opts) == 0 {
		return
	}
	f[key] = opts
}

// SetEnum assigns the options of a closed enum in its canonical order. label == value
// (the FE localizes the label), so it is a pure pass-through of the enum's own typed
// constants — no query. Zero values skip the key.
func (f Filters) SetEnum(key string, values ...string) {
	if len(values) == 0 {
		return
	}
	opts := make([]FilterOption, 0, len(values))
	for _, v := range values {
		opts = append(opts, FilterOption{Label: v, Value: v})
	}
	f[key] = opts
}

// NonNil returns a non-nil Filters so an empty envelope serializes as {} (never null).
// The page builders call it on every Result's Filters before wrapping the page.
func (f Filters) NonNil() Filters {
	if f == nil {
		return Filters{}
	}
	return f
}

// OptionsFromStrings maps distinct DB values (courts, degrees, kinds) to label==value
// filter options, in the given order. Empty values are skipped — a blank option is
// never selectable. Shared by the slices that expose free-text options via DISTINCT.
func OptionsFromStrings(values []string) []FilterOption {
	opts := make([]FilterOption, 0, len(values))
	for _, v := range values {
		if v == "" {
			continue
		}
		opts = append(opts, FilterOption{Label: v, Value: v})
	}
	return opts
}
