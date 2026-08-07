package session

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/gaarutyunov/agentiq/generated/client"
)

// The read half of the mapping: the generated client's row types back into the
// [EventRows] the pure decoder in rows.go understands.
//
// It is a separate step rather than a decoder written directly against the
// generated types, and that is deliberate. `decodeEvent` is the inverse of
// `encodeEvent` and the two are what SPEC.md §7.2's property is a statement
// about; a second decoder reading the client's shapes would be a second opinion
// about the same mapping, and the corpus test — which runs without a database —
// would no longer be testing the code the database path uses.
//
// Two representations have to be undone here, both introduced by the transport
// rather than by the mapping:
//
//   - The `json` columns arrive as `*any` — the driver has already parsed them.
//     Re-marshalling gets back to the bytes [decodeEvent] expects. Key order
//     may differ from what was stored, and that is harmless: both sides of §7.2
//     end at the same Go type, and it is that type's marshalling that is
//     compared, not the intermediate document's.
//   - The two byte columns are `text` holding base64 (see the M2 commit that
//     moved them off `bytea`: pgx returns `bytea` as `[]uint8` and the
//     generated assembler cannot read that as a string, which made every event
//     with a thought signature writable and unreadable).

// eventRowsFromSession converts one event of a `Query.session` result.
//
// The parts are sorted by `part_index` here, because SPEC.md §7.2 rule 2 makes
// that the read-back order and the generated traversal does not provide it: its
// ORDER BY is on each selection's key columns, and `Part`'s key in the property
// graph is a surrogate uuid, so the parts of an event arrive in uuid order,
// which is no order at all. Observed as 0, 2, 1.
//
// `decodeEvent` deliberately does not sort — sorting there would hide a reader
// that lost the order — so the sort belongs here, where the rows are read.
func eventRowsFromSession(sessionID string, e client.SessionSessionEvents) (EventRows, error) {
	row := EventRow{
		ADKID:          e.AdkId,
		SessionID:      sessionID,
		Sequence:       int(e.Sequence),
		InvocationID:   e.InvocationId,
		Author:         e.Author,
		Branch:         deref(e.Branch),
		Timestamp:      e.Timestamp,
		TurnComplete:   e.TurnComplete,
		Interrupted:    e.Interrupted,
		IsolationScope: deref(e.IsolationScope),
		ErrorCode:      deref(e.ErrorCode),
		ErrorMessage:   deref(e.ErrorMessage),
		ContentRole:    e.ContentRole,
		WorkflowUUID:   e.WorkflowUuid,
	}

	// A NULL array and an empty one are different documents: `nil` marshals as
	// `null` and `[]string{}` as `[]`, and the pointer is what keeps them apart.
	if e.LongRunningToolIds != nil {
		ids := e.LongRunningToolIds
		row.LongRunningToolIDs = &ids
	}
	if e.StepFunctionId != nil {
		n := int(*e.StepFunctionId)
		row.StepFunctionID = &n
	}

	var err error
	for _, f := range []struct {
		dst *json.RawMessage
		src *any
		of  string
	}{
		{&row.Routes, e.Routes, "routes"},
		{&row.RequestedInput, e.RequestedInput, "requestedInput"},
		{&row.NodeInfo, e.NodeInfo, "nodeInfo"},
		{&row.GroundingMetadata, e.GroundingMetadata, "groundingMetadata"},
		{&row.UsageMetadata, e.UsageMetadata, "usageMetadata"},
		{&row.CitationMetadata, e.CitationMetadata, "citationMetadata"},
		{&row.CustomMetadata, e.CustomMetadata, "customMetadata"},
	} {
		if *f.dst, err = rawFromAny(f.src); err != nil {
			return EventRows{}, fmt.Errorf("session: load %s: %w", f.of, err)
		}
	}

	parts := make([]client.SessionSessionEventsParts, len(e.Parts))
	copy(parts, e.Parts)
	sort.SliceStable(parts, func(i, j int) bool { return parts[i].PartIndex < parts[j].PartIndex })

	rows := EventRows{Event: row}
	for _, p := range parts {
		pr, err := partRowFromSession(p)
		if err != nil {
			return EventRows{}, fmt.Errorf("session: load part %d: %w", p.PartIndex, err)
		}
		rows.Parts = append(rows.Parts, pr)
	}

	// Actions is 0..1 and always written, so a missing row means the event was
	// stored by something other than `agentiq.append_event`. The zero
	// ActionsRow decodes to the zero EventActions, which is what an event with
	// no actions marshals as, so this reads a defect as an absence rather than
	// failing the whole load.
	if len(e.Actions) > 0 {
		a := e.Actions[0]
		rows.Actions = ActionsRow{
			SkipSummarization: derefBool(a.SkipSummarization),
			TransferToAgent:   deref(a.TransferToAgent),
			Escalate:          derefBool(a.Escalate),
		}
		if rows.Actions.RequestedAuthConfigs, err = rawFromAny(a.RequestedAuthConfigs); err != nil {
			return EventRows{}, fmt.Errorf("session: load requestedAuthConfigs: %w", err)
		}
		for _, d := range a.StateDeltas {
			value, err := rawFromAny(&d.Value)
			if err != nil {
				return EventRows{}, fmt.Errorf("session: load state delta %q: %w", d.Key, err)
			}
			rows.StateDeltas = append(rows.StateDeltas, StateDeltaRow{
				Scope: d.Scope, Key: d.Key, Value: value,
			})
		}
		for _, ad := range a.ArtifactDeltas {
			rows.ArtifactDeltas = append(rows.ArtifactDeltas, ArtifactDeltaRow{
				Filename: ad.Filename, Version: int(ad.Version),
			})
		}
	}

	return rows, nil
}

func partRowFromSession(p client.SessionSessionEventsParts) (PartRow, error) {
	row := PartRow{
		PartIndex:                int(p.PartIndex),
		Text:                     deref(p.Text),
		Thought:                  derefBool(p.Thought),
		FunctionCallID:           p.FunctionCallId,
		FunctionCallName:         p.FunctionCallName,
		FunctionResponseID:       p.FunctionResponseId,
		FunctionResponseName:     p.FunctionResponseName,
		InlineDataMIMEType:       p.InlineDataMimeType,
		InlineDataDisplayName:    p.InlineDataDisplayName,
		FileDataMIMEType:         p.FileDataMimeType,
		FileDataURI:              p.FileDataUri,
		FileDataDisplayName:      p.FileDataDisplayName,
		ExecutableCodeLanguage:   p.ExecutableCodeLanguage,
		ExecutableCodeCode:       p.ExecutableCodeCode,
		CodeExecutionOutcome:     p.CodeExecutionOutcome,
		CodeExecutionOutput:      p.CodeExecutionOutput,
		VideoMetadataStartOffset: p.VideoMetadataStartOffset,
		VideoMetadataEndOffset:   p.VideoMetadataEndOffset,
		VideoMetadataFPS:         p.VideoMetadataFps,
		MediaResolution:          p.MediaResolution,
	}

	var err error
	if row.ThoughtSignature, err = bytesFromBase64(p.ThoughtSignature); err != nil {
		return PartRow{}, fmt.Errorf("thoughtSignature: %w", err)
	}
	if row.InlineDataBytes, err = bytesFromBase64(p.InlineDataBytes); err != nil {
		return PartRow{}, fmt.Errorf("inlineDataBytes: %w", err)
	}

	for _, f := range []struct {
		dst *json.RawMessage
		src *any
		of  string
	}{
		{&row.FunctionCallArgs, p.FunctionCallArgs, "functionCallArgs"},
		{&row.FunctionResponseResponse, p.FunctionResponseResponse, "functionResponseResponse"},
		{&row.AudioTranscription, p.AudioTranscription, "audioTranscription"},
		{&row.ToolCall, p.ToolCall, "toolCall"},
		{&row.ToolResponse, p.ToolResponse, "toolResponse"},
		{&row.PartMetadata, p.PartMetadata, "partMetadata"},
	} {
		if *f.dst, err = rawFromAny(f.src); err != nil {
			return PartRow{}, fmt.Errorf("%s: %w", f.of, err)
		}
	}

	return row, nil
}

// rawFromAny renders a parsed `json` column back as the bytes the decoder
// reads. A nil pointer is SQL NULL and stays nil, which is what tells
// [unmarshalNullable] to leave its destination alone.
func rawFromAny(v *any) (json.RawMessage, error) {
	if v == nil || *v == nil {
		return nil, nil
	}
	raw, err := json.Marshal(*v)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// bytesFromBase64 decodes one of the two `text` columns that hold bytes.
//
// The encoder never writes anything else there: `json.Marshal` renders a Go
// `[]byte` as base64, and that is what crossed the wire into
// `json_populate_record`. A column that does not decode was written by
// something else, and saying so is better than handing back bytes that are not
// the ones stored.
func bytesFromBase64(s *string) ([]byte, error) {
	if s == nil {
		return nil, nil
	}
	if *s == "" {
		// `[]byte{}` and `nil` marshal differently ("" against null), and an
		// empty column is the former: the encoder writes "" for an empty,
		// non-nil slice.
		return []byte{}, nil
	}
	return base64.StdEncoding.DecodeString(*s)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefBool(b *bool) bool {
	return b != nil && *b
}
