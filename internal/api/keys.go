package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/DiegohNY/costlane/internal/auth"
	"github.com/DiegohNY/costlane/internal/store"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// maxBodyBytes caps a request body. Admin payloads are small, and an
// unbounded read is an easy way to exhaust memory.
const maxBodyBytes = 1 << 20

// createKeyRequest is the body of a key creation.
//
// Fields that must distinguish absent from null are json.RawMessage, decoded
// in a second pass: encoding/json cannot otherwise tell "the client omitted
// this" from "the client sent null", and those mean opposite things.
type createKeyRequest struct {
	Label            string         `json:"label"`
	Metadata         map[string]any `json:"metadata"`
	AllowedModels    *[]string      `json:"allowed_models"`
	DisconnectPolicy string         `json:"disconnect_policy"`
	DrainTimeoutMS   *int           `json:"drain_timeout_ms"`
	LimitUSD         *string        `json:"limit_usd"`
}

// keyResponse is a key as returned. It never carries the hash, which is
// enough to verify a guess offline.
type keyResponse struct {
	ID               uuid.UUID         `json:"id"`
	Prefix           string            `json:"key_prefix"`
	Label            string            `json:"label"`
	Metadata         map[string]string `json:"metadata"`
	AllowedModels    *[]string         `json:"allowed_models"`
	DisconnectPolicy string            `json:"disconnect_policy"`
	DrainTimeoutMS   *int              `json:"drain_timeout_ms"`
	LimitUSD         *string           `json:"limit_usd"`
	RevokedAt        *time.Time        `json:"revoked_at"`
	CreatedAt        time.Time         `json:"created_at"`

	// Key carries the secret and is populated only by creation. It is
	// omitted everywhere else, so the value exists in exactly one response.
	Key string `json:"key,omitempty"`
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var req createKeyRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Label == "" {
		WriteError(w, http.StatusBadRequest, ErrorTypeInvalidRequest,
			"label is required: a key without one cannot be attributed to anything")
		return
	}

	metadata, err := flatMetadata(req.Metadata)
	if err != nil {
		WriteError(w, http.StatusBadRequest, ErrorTypeInvalidRequest, err.Error())
		return
	}

	var limit *decimal.Decimal
	if req.LimitUSD != nil {
		d, err := decimal.NewFromString(*req.LimitUSD)
		if err != nil || d.IsNegative() {
			WriteError(w, http.StatusBadRequest, ErrorTypeInvalidRequest,
				"limit_usd must be a non-negative decimal, or null for unlimited")
			return
		}
		limit = &d
	}

	key, err := auth.NewKey()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, ErrorTypeInternal,
			"could not generate a key")
		return
	}

	in := store.CreateKeyInput{
		Hash:             key.Hash,
		Prefix:           key.Prefix,
		Label:            req.Label,
		Metadata:         metadata,
		DisconnectPolicy: req.DisconnectPolicy,
		DrainTimeoutMS:   req.DrainTimeoutMS,
		LimitUSD:         limit,
	}
	if req.AllowedModels != nil {
		in.AllowedModels = *req.AllowedModels
	}

	rec, err := s.db.CreateKey(r.Context(), in)
	if err != nil {
		WriteError(w, http.StatusBadRequest, ErrorTypeInvalidRequest, err.Error())
		return
	}

	body := toKeyResponse(rec)
	// The one moment the secret exists outside the client's own storage.
	body.Key = key.Secret.Expose()
	WriteJSON(w, http.StatusCreated, body)
}

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.db.ListKeys(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, ErrorTypeInternal, "could not list keys")
		return
	}
	out := make([]keyResponse, 0, len(keys))
	for _, k := range keys {
		out = append(out, toKeyResponse(k))
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (s *Server) handleUpdateKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}

	// Decoding into a map first is what makes absent distinguishable from
	// null: a key that is present with a null value appears here, while an
	// omitted one does not.
	var raw map[string]json.RawMessage
	if !decodeBody(w, r, &raw) {
		return
	}

	in := store.UpdateKeyInput{MaxDrainMS: s.maxDrainMS}

	if v, present := raw["label"]; present {
		var label string
		if err := json.Unmarshal(v, &label); err != nil || label == "" {
			WriteError(w, http.StatusBadRequest, ErrorTypeInvalidRequest,
				"label must be a non-empty string")
			return
		}
		in.Label = &label
	}
	if v, present := raw["disconnect_policy"]; present {
		var policy string
		if err := json.Unmarshal(v, &policy); err != nil ||
			(policy != "cancel" && policy != "drain") {
			WriteError(w, http.StatusBadRequest, ErrorTypeInvalidRequest,
				`disconnect_policy must be "cancel" or "drain"`)
			return
		}
		in.DisconnectPolicy = &policy
	}
	if v, present := raw["metadata"]; present {
		field := &store.FieldValue[map[string]string]{Set: true}
		if isJSONNull(v) {
			field.Null = true
		} else {
			var loose map[string]any
			if err := json.Unmarshal(v, &loose); err != nil {
				WriteError(w, http.StatusBadRequest, ErrorTypeInvalidRequest,
					"metadata must be an object")
				return
			}
			flat, err := flatMetadata(loose)
			if err != nil {
				WriteError(w, http.StatusBadRequest, ErrorTypeInvalidRequest, err.Error())
				return
			}
			field.Value = flat
		}
		in.Metadata = field
	}
	if v, present := raw["allowed_models"]; present {
		field := &store.FieldValue[[]string]{Set: true}
		if isJSONNull(v) {
			// Null means every model, which is the opposite of an empty
			// list meaning none.
			field.Null = true
		} else {
			var models []string
			if err := json.Unmarshal(v, &models); err != nil {
				WriteError(w, http.StatusBadRequest, ErrorTypeInvalidRequest,
					"allowed_models must be an array of strings, [] for none, or null for all")
				return
			}
			if models == nil {
				models = []string{}
			}
			field.Value = models
		}
		in.AllowedModels = field
	}
	if v, present := raw["drain_timeout_ms"]; present {
		field := &store.FieldValue[int]{Set: true}
		if isJSONNull(v) {
			field.Null = true
		} else if err := json.Unmarshal(v, &field.Value); err != nil {
			WriteError(w, http.StatusBadRequest, ErrorTypeInvalidRequest,
				"drain_timeout_ms must be a positive integer or null")
			return
		}
		in.DrainTimeoutMS = field
	}
	if v, present := raw["limit_usd"]; present {
		field := &store.FieldValue[decimal.Decimal]{Set: true}
		if isJSONNull(v) {
			field.Null = true
		} else {
			var text string
			if err := json.Unmarshal(v, &text); err != nil {
				WriteError(w, http.StatusBadRequest, ErrorTypeInvalidRequest,
					"limit_usd must be a decimal string, or null for unlimited")
				return
			}
			d, err := decimal.NewFromString(text)
			if err != nil || d.IsNegative() {
				WriteError(w, http.StatusBadRequest, ErrorTypeInvalidRequest,
					"limit_usd must be a non-negative decimal")
				return
			}
			field.Value = d
		}
		in.LimitUSD = field
	}

	rec, err := s.db.UpdateKey(r.Context(), id, in)
	switch {
	case errors.Is(err, store.ErrKeyNotFound):
		WriteError(w, http.StatusNotFound, ErrorTypeInvalidRequest, "no such key")
		return
	case err != nil:
		WriteError(w, http.StatusBadRequest, ErrorTypeInvalidRequest, err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, toKeyResponse(rec))
}

func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	err := s.db.RevokeKey(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrKeyNotFound):
		WriteError(w, http.StatusNotFound, ErrorTypeInvalidRequest, "no such key")
	case err != nil:
		WriteError(w, http.StatusInternalServerError, ErrorTypeInternal, "could not revoke the key")
	default:
		// Revocation is idempotent, so a repeat reports the same success:
		// a 404 on the second call would make a retry look like a failure.
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	}
}

func toKeyResponse(rec store.KeyRecord) keyResponse {
	out := keyResponse{
		ID:               rec.ID,
		Prefix:           rec.Prefix,
		Label:            rec.Label,
		Metadata:         rec.Metadata,
		DisconnectPolicy: rec.DisconnectPolicy,
		DrainTimeoutMS:   rec.DrainTimeoutMS,
		RevokedAt:        rec.RevokedAt,
		CreatedAt:        rec.CreatedAt,
	}
	if rec.Metadata == nil {
		out.Metadata = map[string]string{}
	}
	// nil means every model and travels as JSON null; an empty slice means
	// none and travels as [].
	if rec.AllowedModels != nil {
		models := rec.AllowedModels
		out.AllowedModels = &models
	}
	if rec.LimitUSD != nil {
		text := rec.LimitUSD.String()
		out.LimitUSD = &text
	}
	return out
}

// flatMetadata enforces a flat string map, so the spend aggregations stay
// ordinary queries rather than jsonb traversals.
func flatMetadata(loose map[string]any) (map[string]string, error) {
	if loose == nil {
		return nil, nil
	}
	out := make(map[string]string, len(loose))
	for k, v := range loose {
		text, ok := v.(string)
		if !ok {
			return nil, errors.New(
				"metadata must map strings to strings: nested objects, numbers and arrays are rejected " +
					"so that spend aggregation stays a simple query")
		}
		out[k] = text
	}
	return out, nil
}

func decodeBody(w http.ResponseWriter, r *http.Request, into any) bool {
	body := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil && !errors.Is(err, io.EOF) {
		WriteError(w, http.StatusBadRequest, ErrorTypeInvalidRequest,
			"the request body is not valid JSON for this endpoint")
		return false
	}
	return true
}

func pathUUID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		WriteError(w, http.StatusBadRequest, ErrorTypeInvalidRequest,
			"the key id must be a UUID")
		return uuid.Nil, false
	}
	return id, true
}

func isJSONNull(raw json.RawMessage) bool {
	return string(raw) == "null"
}
