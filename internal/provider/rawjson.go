package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// SetField adds or replaces one top-level field, leaving everything else in
// the document exactly as it arrived.
//
// The obvious alternative — decode into a struct, set the field, encode — is
// wrong for a proxy. Every field the struct does not declare disappears, so a
// parameter added by the provider last week is silently dropped; the
// remaining keys come back in a different order; and numbers are reformatted
// through float64. A gateway that quietly rewrites requests is worse than no
// gateway, because the damage is invisible from both ends.
func SetField(body []byte, name string, value any) ([]byte, error) {
	var doc map[string]json.RawMessage
	if len(bytes.TrimSpace(body)) == 0 {
		doc = map[string]json.RawMessage{}
	} else if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("provider: request body is not a JSON object: %w", err)
	}

	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("provider: encoding %s: %w", name, err)
	}
	doc[name] = encoded

	// Marshalling a map orders keys alphabetically, which reorders the
	// document. That is acceptable — a JSON object is unordered — while
	// losing a field is not, and this keeps every value's own bytes intact.
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("provider: re-encoding request: %w", err)
	}
	return out, nil
}

// Field returns one top-level field's raw bytes, reporting whether it was
// present.
func Field(body []byte, name string) (json.RawMessage, bool) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, false
	}
	v, ok := doc[name]
	return v, ok
}

// FieldNames lists the top-level keys, so a translator can reject what it
// does not understand rather than ignoring it.
func FieldNames(body []byte) ([]string, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("provider: request body is not a JSON object: %w", err)
	}
	names := make([]string, 0, len(doc))
	for k := range doc {
		names = append(names, k)
	}
	return names, nil
}

// WithRoutedModel replaces the body's model with the one routing resolved,
// and only when the two differ.
//
// A "provider/model" prefix is the caller's way of disambiguating a model two
// providers both serve. Routing strips it and passes the bare name in
// Request.Model; the body still carries the prefixed one, and a provider
// asked for "anthropic/claude-sonnet-5" has never heard of it. Rewriting only
// on a difference keeps the byte-for-byte promise for every request that did
// not use a prefix.
func WithRoutedModel(body []byte, model string) ([]byte, error) {
	if model == "" {
		return body, nil
	}
	raw, ok := Field(body, "model")
	if !ok {
		return body, nil
	}
	var current string
	if err := json.Unmarshal(raw, &current); err == nil && current == model {
		return body, nil
	}
	return SetField(body, "model", model)
}
