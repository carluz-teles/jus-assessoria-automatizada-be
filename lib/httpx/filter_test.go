package httpx

import (
	"encoding/json"
	"testing"
)

// The envelope's filters block must serialize as {} — never null — even when a list
// has no selectable options (the FE always reads filters as an object).
func TestFilters_EmptySerializesAsObjectNotNull(t *testing.T) {
	t.Parallel()

	page := Page[int]{Data: []int{1}, Filters: Filters{}.NonNil()}
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["filters"]; !ok {
		t.Fatalf("filters key missing from envelope: %s", raw)
	}
	// json.RawMessage("{}") — assert it is an object, not null.
	if got["filters"] != nil {
		if _, ok := got["filters"].(map[string]any); !ok {
			t.Errorf("filters = %#v, want an object", got["filters"])
		}
	}
}

// Set skips an empty option set, so a list with no values for a filter omits the key
// rather than carrying a null/empty array.
func TestFilters_SetSkipsEmpty(t *testing.T) {
	t.Parallel()

	f := Filters{}
	f.Set("court")
	if _, ok := f["court"]; ok {
		t.Error("empty option set must not create the key")
	}
}

// SetEnum emits one option per enum value with label == value (the FE localizes the
// label), in canonical order.
func TestFilters_SetEnumLabelEqualsValue(t *testing.T) {
	t.Parallel()

	f := Filters{}
	f.SetEnum("status", "OPEN", "DONE", "DISMISSED")

	opts, ok := f["status"]
	if !ok {
		t.Fatal("status key missing")
	}
	if len(opts) != 3 {
		t.Fatalf("len(opts) = %d, want 3", len(opts))
	}
	for i, want := range []string{"OPEN", "DONE", "DISMISSED"} {
		if opts[i].Label != want || opts[i].Value != want {
			t.Errorf("opts[%d] = %+v, want {Label:%q Value:%q}", i, opts[i], want, want)
		}
	}
}

// SetEnum with no values skips the key (same contract as Set).
func TestFilters_SetEnumSkipsEmpty(t *testing.T) {
	t.Parallel()

	f := Filters{}
	f.SetEnum("type")
	if _, ok := f["type"]; ok {
		t.Error("empty enum must not create the key")
	}
}

// NonNil guarantees a marshalable map: a zero Filters (nil map) becomes {} so the
// envelope never serializes filters as null.
func TestFilters_NonNil(t *testing.T) {
	t.Parallel()

	var f Filters
	if f.NonNil() == nil {
		t.Error("NonNil returned nil for a nil Filters")
	}
	raw, err := json.Marshal(f.NonNil())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != "{}" {
		t.Errorf("marshal = %s, want {}", raw)
	}
}

// OptionsFromStrings maps distinct DB values to label==value options, skipping empty
// values so a blank court/degree never renders a chip.
func TestOptionsFromStrings(t *testing.T) {
	t.Parallel()

	opts := OptionsFromStrings([]string{"TJSP", "", "TJMG", "TRT3"})
	if len(opts) != 3 {
		t.Fatalf("len(opts) = %d, want 3 (empty skipped)", len(opts))
	}
	for i, want := range []string{"TJSP", "TJMG", "TRT3"} {
		if opts[i].Label != want || opts[i].Value != want {
			t.Errorf("opts[%d] = %+v, want {Label:%q Value:%q}", i, opts[i], want, want)
		}
	}
}
