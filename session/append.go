package session

import (
	"encoding/json"
	"fmt"

	adksession "google.golang.org/adk/v2/session"
)

// ErrPartialEvent is returned when a partial event is offered for storage.
//
// D5 and SPEC.md §9.4: partial events are never stored, and `sessiontestsuite`
// asserts zero stored events for them. It is an error rather than a silent skip
// so that a caller which meant to store something learns that it did not — the
// silent version is what makes "the session is missing an event" a debugging
// session instead of a message.
var ErrPartialEvent = fmt.Errorf("session: a partial event is never stored")

// AppendDocuments is one event rendered as the five JSON arguments of
// `agentiq.append_event` (SPEC.md §8.3).
//
// They are JSON and not typed arguments because the rows carry around seventy
// columns between them, and Postgres unpacks each document with
// `json_populate_record` — so every field name is checked by the server against
// the real table rather than by position in a seventy-argument signature. The
// row types' `json` tags are those column names, which is why they are not
// decorative.
type AppendDocuments struct {
	Event          json.RawMessage
	Parts          json.RawMessage
	Actions        json.RawMessage
	StateDeltas    json.RawMessage
	ArtifactDeltas json.RawMessage
}

// EncodeForAppend renders e as the arguments of `Mutation.appendEvent`.
//
// sessionID is the *surrogate* uuid of the session row, not ADK's session id.
//
// It rejects a partial event (D5). That check lives here, at the boundary
// between the mapping and the write, because it is a policy about appends and
// not a property of the mapping — [encodeEvent] will happily render a partial
// event, and does, for the round-trip corpus.
func EncodeForAppend(sessionID string, e *adksession.Event) (AppendDocuments, error) {
	if e == nil {
		return AppendDocuments{}, fmt.Errorf("session: append a nil event")
	}
	if e.Partial {
		return AppendDocuments{}, ErrPartialEvent
	}

	rows, err := encodeEvent(sessionID, e)
	if err != nil {
		return AppendDocuments{}, err
	}

	// Each is marshalled separately rather than as one document, because
	// `json_populate_record` takes one object and `json_populate_recordset`
	// one array — a single wrapper object would have to be taken apart again
	// inside the function, in SQL, for no gain.
	//
	// The three list arguments are never null: a `json_populate_recordset` over
	// SQL NULL yields no rows, which is the same result an empty array gives,
	// but an empty array says "there were none" where NULL says "unknown". The
	// function's arguments are declared `json` NOT NULL-able only by
	// convention, so the difference is one the caller has to keep.
	docs := AppendDocuments{
		Parts:          json.RawMessage("[]"),
		StateDeltas:    json.RawMessage("[]"),
		ArtifactDeltas: json.RawMessage("[]"),
	}

	if docs.Event, err = json.Marshal(rows.Event); err != nil {
		return AppendDocuments{}, fmt.Errorf("session: encode the event document: %w", err)
	}
	if docs.Actions, err = json.Marshal(rows.Actions); err != nil {
		return AppendDocuments{}, fmt.Errorf("session: encode the actions document: %w", err)
	}
	if len(rows.Parts) > 0 {
		if docs.Parts, err = json.Marshal(rows.Parts); err != nil {
			return AppendDocuments{}, fmt.Errorf("session: encode the parts document: %w", err)
		}
	}
	if len(rows.StateDeltas) > 0 {
		if docs.StateDeltas, err = json.Marshal(rows.StateDeltas); err != nil {
			return AppendDocuments{}, fmt.Errorf("session: encode the state deltas document: %w", err)
		}
	}
	if len(rows.ArtifactDeltas) > 0 {
		if docs.ArtifactDeltas, err = json.Marshal(rows.ArtifactDeltas); err != nil {
			return AppendDocuments{}, fmt.Errorf("session: encode the artifact deltas document: %w", err)
		}
	}

	return docs, nil
}
