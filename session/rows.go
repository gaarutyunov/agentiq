package session

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// The row types below mirror the seven `agentiq.*` tables of SPEC.md §7.1 one
// for one. They are the boundary between ADK's types and SQL, and they exist
// as their own types — rather than the generated client's input structs —
// because the mapping is the part SPEC.md §7.2 constrains and it has to be
// testable without a database.
//
// # Why so many pointers
//
// SPEC.md §7.2 rule 3: a Go `nil` pointer maps to SQL NULL, and a set-but-empty
// value maps to a non-null empty value. A pointer field here is one where that
// distinction is *observable* after `json.Marshal` — every `genai` and ADK
// pointer, map and slice field, because none of the ones that matter carry
// `omitempty` on the ADK side and a nil marshals as `null` where an empty value
// marshals as `{}` or `[]`.
//
// A plain `string` field is one where it is not observable: `genai.Part.Text`
// and friends are values, not pointers, so "" and absent produce the same JSON
// and a nullable column would be a distinction with no consequence.

// EventRow is one row of `agentiq.event`.
type EventRow struct {
	// ADKID is `adk_id`: ADK's own `Event.ID`, a free-form string. It is not
	// the primary key — gopgql owns that as a surrogate uuid — and it cannot
	// be, because `sessiontestsuite` appends events with IDs like "event1".
	ADKID string `json:"adk_id"`
	// SessionID is the surrogate uuid of the parent session, not its ADK id.
	SessionID string `json:"session_id"`
	// Sequence is monotonic per session and is the ordering guarantee (§7.1).
	// It is allocated by the store, not by the caller: see [Store].
	Sequence int `json:"sequence"`

	InvocationID   string    `json:"invocation_id"`
	Author         string    `json:"author"`
	Branch         string    `json:"branch"`
	Timestamp      time.Time `json:"timestamp"`
	TurnComplete   bool      `json:"turn_complete"`
	Interrupted    bool      `json:"interrupted"`
	IsolationScope string    `json:"isolation_scope"`
	ErrorCode      string    `json:"error_code"`
	ErrorMessage   string    `json:"error_message"`

	// ContentRole is nil exactly when `LLMResponse.Content` is nil. It is what
	// distinguishes an absent Content from a present but empty one, since an
	// empty Content contributes no part rows either.
	ContentRole *string `json:"content_role"`

	LongRunningToolIDs *[]string `json:"long_running_tool_ids"`

	// The `json` columns. A nil RawMessage is SQL NULL; a non-nil one is
	// written verbatim, which is the whole reason §7.2 rule 1 forbids `jsonb`.
	Routes            json.RawMessage `json:"routes"`
	RequestedInput    json.RawMessage `json:"requested_input"`
	NodeInfo          json.RawMessage `json:"node_info"`
	GroundingMetadata json.RawMessage `json:"grounding_metadata"`
	UsageMetadata     json.RawMessage `json:"usage_metadata"`
	CitationMetadata  json.RawMessage `json:"citation_metadata"`
	CustomMetadata    json.RawMessage `json:"custom_metadata"`

	// Provenance (§6.2, §7.3): together these name the `(workflow_uuid,
	// function_id)` of the DBOS step that produced the event.
	StepFunctionID *int    `json:"step_function_id"`
	WorkflowUUID   *string `json:"workflow_uuid"`
}

// PartRow is one row of `agentiq.part` — one wide table, not a discriminated
// union (D4). Several groups may be set on the same row.
type PartRow struct {
	PartIndex int `json:"part_index"`

	Text             string `json:"text"`
	Thought          bool   `json:"thought"`
	ThoughtSignature []byte `json:"thought_signature"`

	FunctionCallID   *string         `json:"function_call_id"`
	FunctionCallName *string         `json:"function_call_name"`
	FunctionCallArgs json.RawMessage `json:"function_call_args"`

	FunctionResponseID       *string         `json:"function_response_id"`
	FunctionResponseName     *string         `json:"function_response_name"`
	FunctionResponseResponse json.RawMessage `json:"function_response_response"`

	InlineDataMIMEType    *string `json:"inline_data_mime_type"`
	InlineDataBytes       []byte  `json:"inline_data_bytes"`
	InlineDataDisplayName *string `json:"inline_data_display_name"`

	FileDataMIMEType    *string `json:"file_data_mime_type"`
	FileDataURI         *string `json:"file_data_uri"`
	FileDataDisplayName *string `json:"file_data_display_name"`

	ExecutableCodeLanguage *string `json:"executable_code_language"`
	ExecutableCodeCode     *string `json:"executable_code_code"`

	CodeExecutionOutcome *string `json:"code_execution_outcome"`
	CodeExecutionOutput  *string `json:"code_execution_output"`

	VideoMetadataStartOffset *string  `json:"video_metadata_start_offset"`
	VideoMetadataEndOffset   *string  `json:"video_metadata_end_offset"`
	VideoMetadataFPS         *float64 `json:"video_metadata_fps"`

	MediaResolution *string `json:"media_resolution"`

	AudioTranscription json.RawMessage `json:"audio_transcription"`
	ToolCall           json.RawMessage `json:"tool_call"`
	ToolResponse       json.RawMessage `json:"tool_response"`
	PartMetadata       json.RawMessage `json:"part_metadata"`
}

// ActionsRow is the 0..1 `agentiq.actions` row of an event.
type ActionsRow struct {
	SkipSummarization    bool            `json:"skip_summarization"`
	TransferToAgent      string          `json:"transfer_to_agent"`
	Escalate             bool            `json:"escalate"`
	RequestedAuthConfigs json.RawMessage `json:"requested_auth_configs"`
}

// StateDeltaRow is one key of an event's `EventActions.StateDelta`, already
// projected onto its §6.4 scope. No row here ever carries `temp`.
type StateDeltaRow struct {
	Scope string          `json:"scope"`
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value_json"`
}

// ArtifactDeltaRow is one entry of an event's `EventActions.ArtifactDelta`.
type ArtifactDeltaRow struct {
	Filename string `json:"filename"`
	Version  int    `json:"version"`
}

// EventRows is everything one `session.Event` becomes: rows across five tables,
// written in one transaction (D2).
type EventRows struct {
	Event          EventRow
	Parts          []PartRow
	Actions        ActionsRow
	StateDeltas    []StateDeltaRow
	ArtifactDeltas []ArtifactDeltaRow
}

// ---------------------------------------------------------------------------
// Encode
// ---------------------------------------------------------------------------

// encodeEvent turns an ADK event into the rows of SPEC.md §7.1.
//
// It does not decide whether the event should be stored at all: a partial event
// is rejected by the caller ([Service.AppendEvent]), because "this event is not
// storable" is a policy about appends (D5) and not a property of the mapping.
func encodeEvent(sessionID string, e *adksession.Event) (EventRows, error) {
	if e == nil {
		return EventRows{}, fmt.Errorf("session: encode a nil event")
	}

	row := EventRow{
		ADKID:          e.ID,
		SessionID:      sessionID,
		InvocationID:   e.InvocationID,
		Author:         e.Author,
		Branch:         e.Branch,
		Timestamp:      e.Timestamp,
		TurnComplete:   e.TurnComplete,
		Interrupted:    e.Interrupted,
		IsolationScope: e.IsolationScope,
		ErrorCode:      e.ErrorCode,
		ErrorMessage:   e.ErrorMessage,
	}

	if e.LongRunningToolIDs != nil {
		ids := e.LongRunningToolIDs
		row.LongRunningToolIDs = &ids
	}

	var err error
	// Each of these is marshalled rather than handed to the driver as a Go
	// value: the column is `json` and the bytes are what round-trips. Letting
	// the driver encode it would put the driver's formatting on the byte
	// equality §7.2 asserts.
	if row.Routes, err = marshalNullable(e.Routes); err != nil {
		return EventRows{}, fmt.Errorf("session: encode routes: %w", err)
	}
	if row.RequestedInput, err = marshalNullable(e.RequestedInput); err != nil {
		return EventRows{}, fmt.Errorf("session: encode requestedInput: %w", err)
	}
	if row.NodeInfo, err = marshalNullable(e.NodeInfo); err != nil {
		return EventRows{}, fmt.Errorf("session: encode nodeInfo: %w", err)
	}
	if row.GroundingMetadata, err = marshalNullable(e.GroundingMetadata); err != nil {
		return EventRows{}, fmt.Errorf("session: encode groundingMetadata: %w", err)
	}
	if row.UsageMetadata, err = marshalNullable(e.UsageMetadata); err != nil {
		return EventRows{}, fmt.Errorf("session: encode usageMetadata: %w", err)
	}
	if row.CitationMetadata, err = marshalNullable(e.CitationMetadata); err != nil {
		return EventRows{}, fmt.Errorf("session: encode citationMetadata: %w", err)
	}
	if row.CustomMetadata, err = marshalNullable(e.CustomMetadata); err != nil {
		return EventRows{}, fmt.Errorf("session: encode customMetadata: %w", err)
	}

	var parts []PartRow
	if e.Content != nil {
		role := e.Content.Role
		row.ContentRole = &role
		parts = make([]PartRow, 0, len(e.Content.Parts))
		for i, p := range e.Content.Parts {
			pr, err := encodePart(i, p)
			if err != nil {
				return EventRows{}, fmt.Errorf("session: encode part %d: %w", i, err)
			}
			parts = append(parts, pr)
		}
	}

	actions, deltas, artifacts, err := encodeActions(e.Actions)
	if err != nil {
		return EventRows{}, err
	}

	return EventRows{
		Event:          row,
		Parts:          parts,
		Actions:        actions,
		StateDeltas:    deltas,
		ArtifactDeltas: artifacts,
	}, nil
}

// encodePart flattens one genai.Part into its row (D4).
//
// partIndex comes from the slice position, never from anything on the part
// itself: SPEC.md §7.2 rule 2 makes it the read-back ordering, so a part's
// position in the event is the only thing that may determine it.
func encodePart(index int, p *genai.Part) (PartRow, error) {
	if p == nil {
		// A nil part in a non-nil slice would come back as a zero part, which
		// is a different document. Refusing is the honest option: nothing in
		// ADK produces one, so this fires only when a caller built the event
		// by hand and meant something the schema cannot say.
		return PartRow{}, fmt.Errorf("session: part %d is nil", index)
	}

	row := PartRow{
		PartIndex:        index,
		Text:             p.Text,
		Thought:          p.Thought,
		ThoughtSignature: p.ThoughtSignature,
	}

	if p.FunctionCall != nil {
		row.FunctionCallID = &p.FunctionCall.ID
		row.FunctionCallName = &p.FunctionCall.Name
		args, err := marshalNullable(p.FunctionCall.Args)
		if err != nil {
			return PartRow{}, fmt.Errorf("functionCall.args: %w", err)
		}
		row.FunctionCallArgs = args
	}

	if p.FunctionResponse != nil {
		row.FunctionResponseID = &p.FunctionResponse.ID
		row.FunctionResponseName = &p.FunctionResponse.Name
		resp, err := marshalNullable(p.FunctionResponse.Response)
		if err != nil {
			return PartRow{}, fmt.Errorf("functionResponse.response: %w", err)
		}
		row.FunctionResponseResponse = resp
	}

	if p.InlineData != nil {
		row.InlineDataMIMEType = &p.InlineData.MIMEType
		row.InlineDataBytes = p.InlineData.Data
		row.InlineDataDisplayName = &p.InlineData.DisplayName
	}

	if p.FileData != nil {
		row.FileDataMIMEType = &p.FileData.MIMEType
		row.FileDataURI = &p.FileData.FileURI
		row.FileDataDisplayName = &p.FileData.DisplayName
	}

	if p.ExecutableCode != nil {
		lang := string(p.ExecutableCode.Language)
		row.ExecutableCodeLanguage = &lang
		row.ExecutableCodeCode = &p.ExecutableCode.Code
	}

	if p.CodeExecutionResult != nil {
		outcome := string(p.CodeExecutionResult.Outcome)
		row.CodeExecutionOutcome = &outcome
		row.CodeExecutionOutput = &p.CodeExecutionResult.Output
	}

	if p.VideoMetadata != nil {
		start := p.VideoMetadata.StartOffset.String()
		end := p.VideoMetadata.EndOffset.String()
		row.VideoMetadataStartOffset = &start
		row.VideoMetadataEndOffset = &end
		// FPS is already a *float64 on the genai side, and its nil-ness is the
		// same nil-ness the column needs. Copying the pointer rather than the
		// value keeps "no frame rate" distinct from "0 fps", which genai marks
		// with `omitempty` and therefore marshals differently.
		row.VideoMetadataFPS = p.VideoMetadata.FPS
	}

	if p.MediaResolution != nil {
		// SPEC.md §7.1 types `mediaResolution` as a single `String` column, but
		// `genai.PartMediaResolution` is a struct — `{Level, NumTokens}`. The
		// column holds the part's JSON rather than just its level, because
		// storing only the level would drop NumTokens silently and §7.2 is a
		// byte-equality property, not a best-effort one. A String column can
		// carry that losslessly; a String column carrying half of it cannot.
		res, err := json.Marshal(p.MediaResolution)
		if err != nil {
			return PartRow{}, fmt.Errorf("mediaResolution: %w", err)
		}
		s := string(res)
		row.MediaResolution = &s
	}

	var err error
	if row.ToolCall, err = marshalNullable(p.ToolCall); err != nil {
		return PartRow{}, fmt.Errorf("toolCall: %w", err)
	}
	if row.ToolResponse, err = marshalNullable(p.ToolResponse); err != nil {
		return PartRow{}, fmt.Errorf("toolResponse: %w", err)
	}
	if row.PartMetadata, err = marshalNullable(p.PartMetadata); err != nil {
		return PartRow{}, fmt.Errorf("partMetadata: %w", err)
	}

	return row, nil
}

// encodeActions splits EventActions across its three tables.
//
// `EventActions` is a value on `session.Event`, not a pointer, so an actions
// row is always written — there is no "absent actions" to represent, and
// `json.Marshal` of an event always emits the object.
func encodeActions(a adksession.EventActions) (ActionsRow, []StateDeltaRow, []ArtifactDeltaRow, error) {
	row := ActionsRow{
		SkipSummarization: a.SkipSummarization,
		TransferToAgent:   a.TransferToAgent,
		Escalate:          a.Escalate,
	}

	confirmations, err := marshalNullable(a.RequestedToolConfirmations)
	if err != nil {
		return ActionsRow{}, nil, nil, fmt.Errorf("session: encode requestedToolConfirmations: %w", err)
	}
	row.RequestedAuthConfigs = confirmations

	// Sorted, not map order. Map iteration is non-deterministic, and these
	// rows are written from inside workflow code (SPEC.md §9.1) where that is
	// forbidden outright — the determinism analyzer would flag a bare range
	// over a map here, and it would be right to.
	var deltas []StateDeltaRow
	if a.StateDelta != nil {
		keys := make([]string, 0, len(a.StateDelta))
		for k := range a.StateDelta {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		deltas = make([]StateDeltaRow, 0, len(keys))
		for _, k := range keys {
			scope, stored, persist := ScopeOf(k)
			if !persist {
				// §7.2 rule 4: a `temp:` key is dropped here and is absent on
				// read. This is the deviation, and it is the only one.
				continue
			}
			value, err := json.Marshal(a.StateDelta[k])
			if err != nil {
				return ActionsRow{}, nil, nil, fmt.Errorf("session: encode stateDelta %q: %w", k, err)
			}
			deltas = append(deltas, StateDeltaRow{Scope: scope, Key: stored, Value: value})
		}
	}

	var artifacts []ArtifactDeltaRow
	if a.ArtifactDelta != nil {
		names := make([]string, 0, len(a.ArtifactDelta))
		for k := range a.ArtifactDelta {
			names = append(names, k)
		}
		sort.Strings(names)
		artifacts = make([]ArtifactDeltaRow, 0, len(names))
		for _, n := range names {
			artifacts = append(artifacts, ArtifactDeltaRow{Filename: n, Version: int(a.ArtifactDelta[n])})
		}
	}

	return row, deltas, artifacts, nil
}

// ---------------------------------------------------------------------------
// Decode
// ---------------------------------------------------------------------------

// decodeEvent rebuilds an ADK event from its rows.
//
// The parts must already be ordered by `part_index` (SPEC.md §7.2 rule 2);
// this does not sort them, because sorting here would hide a store that read
// them back unordered and the ordering is the store's guarantee to keep.
func decodeEvent(rows EventRows) (*adksession.Event, error) {
	e := &adksession.Event{
		ID:             rows.Event.ADKID,
		Timestamp:      rows.Event.Timestamp,
		InvocationID:   rows.Event.InvocationID,
		Author:         rows.Event.Author,
		Branch:         rows.Event.Branch,
		IsolationScope: rows.Event.IsolationScope,
	}
	e.TurnComplete = rows.Event.TurnComplete
	e.Interrupted = rows.Event.Interrupted
	e.ErrorCode = rows.Event.ErrorCode
	e.ErrorMessage = rows.Event.ErrorMessage

	if rows.Event.LongRunningToolIDs != nil {
		e.LongRunningToolIDs = *rows.Event.LongRunningToolIDs
	}

	if err := unmarshalNullable(rows.Event.Routes, &e.Routes); err != nil {
		return nil, fmt.Errorf("session: decode routes: %w", err)
	}
	if err := unmarshalNullable(rows.Event.RequestedInput, &e.RequestedInput); err != nil {
		return nil, fmt.Errorf("session: decode requestedInput: %w", err)
	}
	if err := unmarshalNullable(rows.Event.NodeInfo, &e.NodeInfo); err != nil {
		return nil, fmt.Errorf("session: decode nodeInfo: %w", err)
	}
	if err := unmarshalNullable(rows.Event.GroundingMetadata, &e.GroundingMetadata); err != nil {
		return nil, fmt.Errorf("session: decode groundingMetadata: %w", err)
	}
	if err := unmarshalNullable(rows.Event.UsageMetadata, &e.UsageMetadata); err != nil {
		return nil, fmt.Errorf("session: decode usageMetadata: %w", err)
	}
	if err := unmarshalNullable(rows.Event.CitationMetadata, &e.CitationMetadata); err != nil {
		return nil, fmt.Errorf("session: decode citationMetadata: %w", err)
	}
	if err := unmarshalNullable(rows.Event.CustomMetadata, &e.CustomMetadata); err != nil {
		return nil, fmt.Errorf("session: decode customMetadata: %w", err)
	}

	if rows.Event.ContentRole != nil {
		content := &genai.Content{Role: *rows.Event.ContentRole}
		for _, pr := range rows.Parts {
			p, err := decodePart(pr)
			if err != nil {
				return nil, fmt.Errorf("session: decode part %d: %w", pr.PartIndex, err)
			}
			content.Parts = append(content.Parts, p)
		}
		e.Content = content
	}

	actions, err := decodeActions(rows.Actions, rows.StateDeltas, rows.ArtifactDeltas)
	if err != nil {
		return nil, err
	}
	e.Actions = actions

	return e, nil
}

func decodePart(row PartRow) (*genai.Part, error) {
	p := &genai.Part{
		Text:             row.Text,
		Thought:          row.Thought,
		ThoughtSignature: row.ThoughtSignature,
	}

	if row.FunctionCallID != nil || row.FunctionCallName != nil {
		fc := &genai.FunctionCall{}
		if row.FunctionCallID != nil {
			fc.ID = *row.FunctionCallID
		}
		if row.FunctionCallName != nil {
			fc.Name = *row.FunctionCallName
		}
		if err := unmarshalNullable(row.FunctionCallArgs, &fc.Args); err != nil {
			return nil, fmt.Errorf("functionCall.args: %w", err)
		}
		p.FunctionCall = fc
	}

	if row.FunctionResponseID != nil || row.FunctionResponseName != nil {
		fr := &genai.FunctionResponse{}
		if row.FunctionResponseID != nil {
			fr.ID = *row.FunctionResponseID
		}
		if row.FunctionResponseName != nil {
			fr.Name = *row.FunctionResponseName
		}
		if err := unmarshalNullable(row.FunctionResponseResponse, &fr.Response); err != nil {
			return nil, fmt.Errorf("functionResponse.response: %w", err)
		}
		p.FunctionResponse = fr
	}

	if row.InlineDataMIMEType != nil || row.InlineDataDisplayName != nil || row.InlineDataBytes != nil {
		blob := &genai.Blob{Data: row.InlineDataBytes}
		if row.InlineDataMIMEType != nil {
			blob.MIMEType = *row.InlineDataMIMEType
		}
		if row.InlineDataDisplayName != nil {
			blob.DisplayName = *row.InlineDataDisplayName
		}
		p.InlineData = blob
	}

	if row.FileDataMIMEType != nil || row.FileDataURI != nil || row.FileDataDisplayName != nil {
		fd := &genai.FileData{}
		if row.FileDataMIMEType != nil {
			fd.MIMEType = *row.FileDataMIMEType
		}
		if row.FileDataURI != nil {
			fd.FileURI = *row.FileDataURI
		}
		if row.FileDataDisplayName != nil {
			fd.DisplayName = *row.FileDataDisplayName
		}
		p.FileData = fd
	}

	if row.ExecutableCodeLanguage != nil || row.ExecutableCodeCode != nil {
		ec := &genai.ExecutableCode{}
		if row.ExecutableCodeLanguage != nil {
			ec.Language = genai.Language(*row.ExecutableCodeLanguage)
		}
		if row.ExecutableCodeCode != nil {
			ec.Code = *row.ExecutableCodeCode
		}
		p.ExecutableCode = ec
	}

	if row.CodeExecutionOutcome != nil || row.CodeExecutionOutput != nil {
		cer := &genai.CodeExecutionResult{}
		if row.CodeExecutionOutcome != nil {
			cer.Outcome = genai.Outcome(*row.CodeExecutionOutcome)
		}
		if row.CodeExecutionOutput != nil {
			cer.Output = *row.CodeExecutionOutput
		}
		p.CodeExecutionResult = cer
	}

	if row.VideoMetadataStartOffset != nil || row.VideoMetadataEndOffset != nil || row.VideoMetadataFPS != nil {
		vm := &genai.VideoMetadata{}
		if row.VideoMetadataStartOffset != nil {
			d, err := parseOffset(*row.VideoMetadataStartOffset)
			if err != nil {
				return nil, fmt.Errorf("videoMetadata.startOffset: %w", err)
			}
			vm.StartOffset = d
		}
		if row.VideoMetadataEndOffset != nil {
			d, err := parseOffset(*row.VideoMetadataEndOffset)
			if err != nil {
				return nil, fmt.Errorf("videoMetadata.endOffset: %w", err)
			}
			vm.EndOffset = d
		}
		vm.FPS = row.VideoMetadataFPS
		p.VideoMetadata = vm
	}

	if row.MediaResolution != nil {
		var res genai.PartMediaResolution
		if err := json.Unmarshal([]byte(*row.MediaResolution), &res); err != nil {
			return nil, fmt.Errorf("mediaResolution: %w", err)
		}
		p.MediaResolution = &res
	}

	if err := unmarshalNullable(row.ToolCall, &p.ToolCall); err != nil {
		return nil, fmt.Errorf("toolCall: %w", err)
	}
	if err := unmarshalNullable(row.ToolResponse, &p.ToolResponse); err != nil {
		return nil, fmt.Errorf("toolResponse: %w", err)
	}
	if err := unmarshalNullable(row.PartMetadata, &p.PartMetadata); err != nil {
		return nil, fmt.Errorf("partMetadata: %w", err)
	}

	return p, nil
}

func decodeActions(row ActionsRow, deltas []StateDeltaRow, artifacts []ArtifactDeltaRow) (adksession.EventActions, error) {
	a := adksession.EventActions{
		SkipSummarization: row.SkipSummarization,
		TransferToAgent:   row.TransferToAgent,
		Escalate:          row.Escalate,
	}

	if err := unmarshalNullable(row.RequestedAuthConfigs, &a.RequestedToolConfirmations); err != nil {
		return adksession.EventActions{}, fmt.Errorf("session: decode requestedToolConfirmations: %w", err)
	}

	// A nil map and an empty map are different documents, so the map is
	// created only when there are rows — the same reason the columns are
	// nullable (§7.2 rule 3). An event whose StateDelta was an empty non-nil
	// map is therefore indistinguishable from one whose StateDelta was nil,
	// which is a real limitation of a row-per-key encoding and is called out
	// in TestAnEmptyStateDeltaMapIsIndistinguishableFromNil.
	if len(deltas) > 0 {
		a.StateDelta = make(map[string]any, len(deltas))
		for _, d := range deltas {
			var v any
			if err := json.Unmarshal(d.Value, &v); err != nil {
				return adksession.EventActions{}, fmt.Errorf("session: decode stateDelta %q: %w", d.Key, err)
			}
			a.StateDelta[Key(d.Scope, d.Key)] = v
		}
	}

	if len(artifacts) > 0 {
		a.ArtifactDelta = make(map[string]int64, len(artifacts))
		for _, ad := range artifacts {
			a.ArtifactDelta[ad.Filename] = int64(ad.Version)
		}
	}

	return a, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// marshalNullable renders v as the bytes of a `json` column, mapping a nil
// pointer, map or slice to SQL NULL rather than to the four bytes "null".
//
// The difference matters on the way back: a NULL column decodes to a nil Go
// value without touching the field, while a literal "null" would too — but
// only a NULL column says "this was absent" to anything reading the table
// directly, including the property graph.
func marshalNullable(v any) (json.RawMessage, error) {
	if isNil(v) {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// unmarshalNullable decodes a `json` column into dst, leaving dst untouched
// when the column was NULL.
func unmarshalNullable(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, dst)
}

// isNil reports whether v is a nil pointer, map or slice held in an interface.
//
// A plain `v == nil` is false for a nil pointer stored in a non-nil interface,
// which is the case every caller here hits: `marshalNullable(e.NodeInfo)` boxes
// a typed nil. Reflection is the only way to see through that, and getting it
// wrong writes `"null"` into a column that should have been NULL.
func isNil(v any) bool {
	if v == nil {
		return true
	}
	switch t := v.(type) {
	case *adksession.RequestInput:
		return t == nil
	case *adksession.NodeInfo:
		return t == nil
	case *genai.GroundingMetadata:
		return t == nil
	case *genai.GenerateContentResponseUsageMetadata:
		return t == nil
	case *genai.CitationMetadata:
		return t == nil
	case *genai.ToolCall:
		return t == nil
	case *genai.ToolResponse:
		return t == nil
	case map[string]any:
		return t == nil
	case []string:
		return t == nil
	}
	return false
}

// parseOffset reads back a `genai.VideoMetadata` offset.
//
// The offsets are `time.Duration` on the genai side and are stored as the text
// their String method produced, because the column is a string in SPEC.md §7.1
// and a duration written as a number would have to pick a unit the schema does
// not name.
func parseOffset(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	return time.ParseDuration(s)
}

// _ pins the assumption the whole mapping rests on: `session.Event` embeds
// `model.LLMResponse`, so the fields this file reads off `e` directly
// (TurnComplete, ErrorCode, …) are the promoted ones and not a parallel set.
var _ = adksession.Event{LLMResponse: adkmodel.LLMResponse{}}
