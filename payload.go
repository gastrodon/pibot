package main

import (
	"encoding/json"
	"unicode/utf8"
)

// maxDispatchPayload leaves headroom under Nomad's hard-coded 16384-byte
// dispatch payload limit (DispatchPayloadSizeLimit in nomad/job_endpoint.go —
// not a server setting, so we can't raise it). Linear's AgentSessionEvent
// webhook grows with the thread (promptContext embeds the issue + comment
// history), so long-running threads regularly exceed it and Nomad rejects the
// dispatch outright with "Payload exceeds maximum size" — pibot never even
// starts. shrinkPayload keeps that from happening.
const maxDispatchPayload = 15 * 1024

// truncatedMarker prefixes any string field shrinkPayload had to cut down, so
// pi (and anyone reading pi.jsonl) can tell the context it saw was partial.
const truncatedMarker = "…[truncated by pibot: original context exceeded Nomad's dispatch size limit]…"

// shrinkPayload returns raw unchanged if it already fits under
// maxDispatchPayload. Otherwise it walks the top-level webhook object (and one
// level of nesting, to reach fields like agentSession.promptContext) for
// string values and truncates the largest one(s) — keeping each string's tail,
// since Linear assembles context oldest-first, so the newest/most relevant
// text survives — until the whole payload fits. If the body isn't a JSON
// object (unexpected shape), it falls back to a byte-safe hard truncation
// wrapped in a minimal JSON envelope, so a dispatch never fails outright just
// because we can't parse it.
func shrinkPayload(raw []byte) []byte {
	if len(raw) <= maxDispatchPayload {
		return raw
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return fallbackTruncate(raw)
	}

	fields := collectStringFields(top)
	for range fields {
		out, err := json.Marshal(top)
		if err != nil {
			return fallbackTruncate(raw)
		}
		if len(out) <= maxDispatchPayload {
			return out
		}

		// Shrink whichever field is currently largest; re-evaluate each pass
		// since truncating one field changes the overall size.
		largest := largestField(fields)
		if largest == nil || len(largest.value) < 200 {
			break // nothing left worth cutting
		}
		over := len(out) - maxDispatchPayload
		cut := over + over/4 + len(truncatedMarker) // trim extra so we converge, not loop
		if cut >= len(largest.value) {
			largest.set("")
			continue
		}
		largest.set(truncatedMarker + trimToUTF8Boundary(largest.value, cut))
	}

	out, err := json.Marshal(top)
	if err != nil || len(out) > maxDispatchPayload {
		return fallbackTruncate(raw)
	}
	return out
}

// stringField is a mutable handle on one JSON string value somewhere in the
// (at most 2-level-deep) webhook object.
type stringField struct {
	value string
	set   func(string)
}

// collectStringFields gathers every top-level string field plus, for every
// top-level object field, its immediate string sub-fields — covering both
// `.promptContext` and `.agentSession.promptContext`-shaped payloads without
// needing to know Linear's exact webhook schema.
func collectStringFields(top map[string]json.RawMessage) []*stringField {
	var fields []*stringField

	addFrom := func(obj map[string]json.RawMessage, flush func()) {
		for k, v := range obj {
			if len(v) < 2 || v[0] != '"' {
				continue
			}
			var s string
			if json.Unmarshal(v, &s) != nil {
				continue
			}
			k := k
			f := &stringField{value: s}
			f.set = func(ns string) {
				f.value = ns
				nb, _ := json.Marshal(ns)
				obj[k] = nb
				if flush != nil {
					flush()
				}
			}
			fields = append(fields, f)
		}
	}

	addFrom(top, nil)
	for k, v := range top {
		if len(v) < 2 || v[0] != '{' {
			continue
		}
		var nested map[string]json.RawMessage
		if json.Unmarshal(v, &nested) != nil {
			continue
		}
		k := k
		addFrom(nested, func() {
			nb, _ := json.Marshal(nested)
			top[k] = nb
		})
	}
	return fields
}

// largestField returns the field currently holding the most bytes.
func largestField(fields []*stringField) *stringField {
	var largest *stringField
	for _, f := range fields {
		if largest == nil || len(f.value) > len(largest.value) {
			largest = f
		}
	}
	return largest
}

// trimToUTF8Boundary drops the first n bytes of s, nudged forward to the next
// rune boundary so we never split a multi-byte UTF-8 character.
func trimToUTF8Boundary(s string, n int) string {
	if n <= 0 {
		return s
	}
	if n >= len(s) {
		return ""
	}
	for n < len(s) && !utf8.RuneStart(s[n]) {
		n++
	}
	return s[n:]
}

// fallbackTruncate handles payloads shrinkPayload couldn't safely parse as a
// JSON object: hard-truncate the raw bytes to a UTF-8-safe boundary and wrap
// them as a single JSON string field, so the dispatch still carries something
// under the size limit instead of failing outright.
func fallbackTruncate(raw []byte) []byte {
	const envelope = `{"promptContext":""}` // fixed overhead around the string value
	budget := maxDispatchPayload - len(envelope) - len(truncatedMarker) - 16
	if budget < 0 {
		budget = 0
	}
	s := trimToUTF8Boundary(string(raw), len(raw)-budget)
	for {
		out, err := json.Marshal(map[string]string{"promptContext": truncatedMarker + s})
		if err != nil {
			return []byte(`{"promptContext":""}`)
		}
		if len(out) <= maxDispatchPayload || s == "" {
			return out
		}
		// Escaping (quotes, control chars) can inflate the marshaled size beyond
		// our estimate; back off further until it actually fits.
		s = trimToUTF8Boundary(s, len(out)-maxDispatchPayload+16)
	}
}
