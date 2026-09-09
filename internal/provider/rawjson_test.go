package provider_test

import (
	"encoding/json"
	"testing"

	"github.com/DiegohNY/costlane/internal/provider"
)

// A proxy that decodes into a struct and re-encodes loses everything the
// struct does not declare. This is the test that keeps that from happening.
func TestSetFieldPreservesUnknownFields(t *testing.T) {
	body := []byte(`{
		"model": "gpt-6-astra",
		"messages": [{"role": "user", "content": "hello"}],
		"a_parameter_added_last_week": {"nested": [1, 2, 3]},
		"another_one": "value",
		"third": 42,
		"fourth": null,
		"fifth": true,
		"sixth": 1.5,
		"seventh": [],
		"eighth": {},
		"ninth": "unicode: éè",
		"tenth": -0.000001
	}`)

	out, err := provider.SetField(body, "stream_options", map[string]any{"include_usage": true})
	if err != nil {
		t.Fatalf("SetField: %v", err)
	}

	var before, after map[string]any
	if err := json.Unmarshal(body, &before); err != nil {
		t.Fatalf("decoding original: %v", err)
	}
	if err := json.Unmarshal(out, &after); err != nil {
		t.Fatalf("decoding result: %v", err)
	}

	// Every field that went in must come out, unchanged.
	for name, want := range before {
		got, present := after[name]
		if !present {
			t.Errorf("field %q was dropped", name)
			continue
		}
		wantJSON, _ := json.Marshal(want)
		gotJSON, _ := json.Marshal(got)
		if string(wantJSON) != string(gotJSON) {
			t.Errorf("field %q changed: %s -> %s", name, wantJSON, gotJSON)
		}
	}

	// And exactly one field is new.
	if len(after) != len(before)+1 {
		t.Errorf("%d fields out, want %d", len(after), len(before)+1)
	}
	if _, ok := after["stream_options"]; !ok {
		t.Error("the injected field is missing")
	}
}

// Numbers must survive without passing through float64, where a large
// integer or a long decimal loses precision.
func TestSetFieldPreservesNumericPrecision(t *testing.T) {
	body := []byte(`{"model":"m","seed":9007199254740993,"temperature":0.10000000000000001}`)

	out, err := provider.SetField(body, "stream", false)
	if err != nil {
		t.Fatalf("SetField: %v", err)
	}
	seed, ok := provider.Field(out, "seed")
	if !ok {
		t.Fatal("seed was dropped")
	}
	if string(seed) != "9007199254740993" {
		t.Errorf("seed = %s, want the original digits", seed)
	}
	temp, _ := provider.Field(out, "temperature")
	if string(temp) != "0.10000000000000001" {
		t.Errorf("temperature = %s, want the original digits", temp)
	}
}

func TestSetFieldReplacesAnExistingValue(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":100}`)
	out, err := provider.SetField(body, "max_tokens", 4096)
	if err != nil {
		t.Fatalf("SetField: %v", err)
	}
	v, _ := provider.Field(out, "max_tokens")
	if string(v) != "4096" {
		t.Errorf("max_tokens = %s, want 4096", v)
	}
}

func TestSetFieldRejectsNonObjects(t *testing.T) {
	for _, body := range []string{`[1,2,3]`, `"a string"`, `42`, `not json`} {
		if _, err := provider.SetField([]byte(body), "x", 1); err == nil {
			t.Errorf("body %q must be rejected", body)
		}
	}
}

func TestFieldNamesEnumeratesTopLevelKeys(t *testing.T) {
	names, err := provider.FieldNames([]byte(`{"a":1,"b":2,"c":3}`))
	if err != nil {
		t.Fatalf("FieldNames: %v", err)
	}
	if len(names) != 3 {
		t.Errorf("%d names, want 3", len(names))
	}
}
