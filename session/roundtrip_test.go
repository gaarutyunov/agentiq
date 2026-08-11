package session

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	adkmodel "google.golang.org/adk/v2/model"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// roundTrip is store-then-load with the database taken out.
//
// That is not a shortcut. SPEC.md §7.2's property is a property of the mapping:
// the columns are `json` precisely so that Postgres hands back the bytes it was
// given (rule 1), so a database in the middle can only turn a passing case into
// a failing one — never the reverse. Running it here means the corpus runs on
// every `go test ./...` instead of only when Docker is up, and the identical
// corpus runs through the real tables from the execution half.
func roundTrip(t *testing.T, e *adksession.Event) *adksession.Event {
	t.Helper()
	rows, err := encodeEvent("00000000-0000-0000-0000-000000000001", e)
	require.NoError(t, err, "encode")
	back, err := decodeEvent(rows)
	require.NoError(t, err, "decode")
	return back
}

// TestEventRoundTripIsByteIdentical is the SPEC.md §7.2 conformance property
// and the §15 scenario "Event round-trip is byte-identical":
//
//	for any session.Event E, json.Marshal(load(store(E))) == json.Marshal(E)
//
// The corpus entry carrying a `temp:` key is excluded, because §7.2 rule 4
// makes that the one sanctioned deviation — it has its own test below rather
// than a quiet exemption here.
func TestEventRoundTripIsByteIdentical(t *testing.T) {
	for _, entry := range corpus() {
		if hasTempKey(entry.event) {
			continue
		}
		t.Run(entry.name, func(t *testing.T) {
			want, err := json.Marshal(entry.event)
			require.NoError(t, err)

			got, err := json.Marshal(roundTrip(t, entry.event))
			require.NoError(t, err)

			assert.JSONEq(t, string(want), string(got), entry.why)
			assert.Equal(t, string(want), string(got),
				"byte equality, not just JSON equivalence — %s", entry.why)
		})
	}
}

// TestTheCorpusCarriesWhatSection15Names pins the corpus itself.
//
// A corpus is only worth as much as its contents, and "the round-trip property
// holds over the corpus" says nothing if the corpus quietly lost the event
// §15 requires. This asserts the three features by inspecting the events, not
// by trusting their names.
func TestTheCorpusCarriesWhatSection15Names(t *testing.T) {
	var text, thoughtSignature, functionCall bool
	for _, entry := range corpus() {
		if entry.event.Content == nil {
			continue
		}
		for _, p := range entry.event.Content.Parts {
			text = text || p.Text != ""
			thoughtSignature = thoughtSignature || len(p.ThoughtSignature) > 0
			functionCall = functionCall || p.FunctionCall != nil
		}
	}
	assert.True(t, text, "SPEC.md §15: the corpus must include an event with text")
	assert.True(t, thoughtSignature, "SPEC.md §15: ... a thought signature")
	assert.True(t, functionCall, "SPEC.md §15: ... and a function call")
}

// TestTempStateIsDroppedOnWriteAndAbsentOnRead is SPEC.md §7.2 rule 4 and the
// M2 acceptance item that asks for it explicitly.
//
// It is its own test and not a corpus entry because it asserts the opposite of
// the property above: this is the one place the reloaded event is *not* the
// event that was stored, and an assertion that says so is the difference
// between a sanctioned deviation and a bug nobody noticed.
func TestTempStateIsDroppedOnWriteAndAbsentOnRead(t *testing.T) {
	e := &adksession.Event{
		ID:           "event-temp",
		Timestamp:    fixedTime,
		InvocationID: "inv-temp",
		Author:       "root",
		Actions: adksession.EventActions{
			StateDelta: map[string]any{
				"app:theme":    "dark",
				"user:name":    "ada",
				"turns":        float64(3),
				"temp:scratch": "discard me",
			},
		},
	}

	rows, err := encodeEvent("00000000-0000-0000-0000-000000000001", e)
	require.NoError(t, err)

	// Dropped on write: no row is produced at all, so there is nothing for a
	// later reader — including the property graph — to filter out.
	for _, d := range rows.StateDeltas {
		assert.NotEqual(t, "temp", d.Scope, "no state_delta row may carry scope 'temp'")
		assert.NotContains(t, d.Key, "temp:", "the temp: prefix must not survive into a key")
	}
	require.Len(t, rows.StateDeltas, 3, "app:, user: and the unprefixed key are stored; temp: is not")

	// Absent on read.
	back, err := decodeEvent(rows)
	require.NoError(t, err)
	require.NotNil(t, back.Actions.StateDelta)
	assert.NotContains(t, back.Actions.StateDelta, "temp:scratch")

	// And every other key came back untouched, prefix and all. A deviation
	// that also dropped `app:` would pass the assertion above.
	assert.Equal(t, map[string]any{
		"app:theme": "dark",
		"user:name": "ada",
		"turns":     float64(3),
	}, back.Actions.StateDelta)
}

// TestStateKeysAreProjectedOntoTheirScope is SPEC.md §6.4.
func TestStateKeysAreProjectedOntoTheirScope(t *testing.T) {
	for _, tc := range []struct {
		key     string
		scope   string
		stored  string
		persist bool
	}{
		{"app:theme", ScopeApp, "theme", true},
		{"user:name", ScopeUser, "name", true},
		{"turns", ScopeSession, "turns", true},
		{"temp:scratch", "", "", false},
		// A key that merely contains a prefix is not prefixed by it. This is
		// the case a `strings.Contains` implementation gets wrong.
		{"my:app:theme", ScopeSession, "my:app:theme", true},
		{"", ScopeSession, "", true},
	} {
		t.Run(tc.key, func(t *testing.T) {
			scope, stored, persist := ScopeOf(tc.key)
			assert.Equal(t, tc.scope, scope)
			assert.Equal(t, tc.stored, stored)
			assert.Equal(t, tc.persist, persist)

			if persist {
				assert.Equal(t, tc.key, Key(scope, stored),
					"Key must invert ScopeOf, or the key does not come back as it went in")
			}
		})
	}
}

// TestPartIndexIsSlicePositionAndOrdersTheReadBack is SPEC.md §7.2 rule 2.
func TestPartIndexIsSlicePositionAndOrdersTheReadBack(t *testing.T) {
	e := &adksession.Event{
		ID:        "event-order",
		Timestamp: fixedTime,
		Author:    "root",
		LLMResponse: llmResponse(&genai.Content{
			Role:  "model",
			Parts: []*genai.Part{{Text: "zebra"}, {Text: "apple"}, {Text: "mango"}},
		}),
	}

	rows, err := encodeEvent("00000000-0000-0000-0000-000000000001", e)
	require.NoError(t, err)
	require.Len(t, rows.Parts, 3)
	for i, p := range rows.Parts {
		assert.Equal(t, i, p.PartIndex, "part_index is the slice position, nothing else")
	}

	back, err := decodeEvent(rows)
	require.NoError(t, err)
	require.NotNil(t, back.Content)
	var texts []string
	for _, p := range back.Content.Parts {
		texts = append(texts, p.Text)
	}
	assert.Equal(t, []string{"zebra", "apple", "mango"}, texts,
		"parts come back in part_index order, which is the order they were written in")
}

// TestAbsentIsNotEmpty is SPEC.md §7.2 rule 3, at the level of the columns
// rather than the marshalled document.
//
// The round-trip property above would catch a violation, but it would report it
// as "these two JSON documents differ" — this says which column is wrong.
func TestAbsentIsNotEmpty(t *testing.T) {
	absent := &adksession.Event{ID: "a", Timestamp: fixedTime}
	absentRows, err := encodeEvent("s", absent)
	require.NoError(t, err)
	assert.Nil(t, absentRows.Event.ContentRole, "a nil Content writes content_role NULL")
	assert.Nil(t, absentRows.Event.CustomMetadata, "a nil map writes custom_metadata NULL")
	assert.Nil(t, absentRows.Event.LongRunningToolIDs, "a nil slice writes NULL")

	empty := &adksession.Event{
		ID:                 "b",
		Timestamp:          fixedTime,
		LongRunningToolIDs: []string{},
		LLMResponse:        emptyNotAbsent(),
	}
	emptyRows, err := encodeEvent("s", empty)
	require.NoError(t, err)
	require.NotNil(t, emptyRows.Event.ContentRole, "a present Content writes a non-null content_role")
	assert.Equal(t, "", *emptyRows.Event.ContentRole, "... and it is the empty role it had")
	assert.JSONEq(t, `{}`, string(emptyRows.Event.CustomMetadata), "an empty map is a non-null empty object")
	require.NotNil(t, emptyRows.Event.LongRunningToolIDs)
	assert.Empty(t, *emptyRows.Event.LongRunningToolIDs)

	// The same distinction one level down, in a flattened part group. This is
	// the case the pointer columns exist for: without them a `&FunctionCall{}`
	// and a nil FunctionCall both write three NULLs.
	part, err := encodePart(0, &genai.Part{FunctionCall: &genai.FunctionCall{}})
	require.NoError(t, err)
	require.NotNil(t, part.FunctionCallID, "a present but empty FunctionCall is not NULL")
	assert.Equal(t, "", *part.FunctionCallID)

	nilCall, err := encodePart(0, &genai.Part{})
	require.NoError(t, err)
	assert.Nil(t, nilCall.FunctionCallID, "an absent FunctionCall is NULL")
}

// TestJSONColumnsHoldTheirBytesVerbatim checks the half of SPEC.md §7.2 rule 1
// that the mapping controls: what goes *into* the column.
//
// A value that already carries its own bytes is written through unchanged, so a
// column typed `json` stores exactly this and a column typed `jsonb` would
// reorder it. That is the rule's real subject — the bytes at rest, which the
// property graph and the sibling memory project read directly.
func TestJSONColumnsHoldTheirBytesVerbatim(t *testing.T) {
	raw := json.RawMessage(`{"b":1,"a":2}`)
	rows, err := encodeEvent("s", &adksession.Event{
		ID:          "j",
		Timestamp:   fixedTime,
		LLMResponse: adkmodel.LLMResponse{CustomMetadata: map[string]any{"k": raw}},
	})
	require.NoError(t, err)
	assert.Equal(t, `{"k":{"b":1,"a":2}}`, string(rows.Event.CustomMetadata),
		"the mapping writes the bytes it was given; if this reads {\"a\":2,\"b\":1} "+
			"something normalised the value before it reached the column")
}

// TestGoMapDecodingNormalisesKeyOrderBeforeTheColumnCan records a finding that
// narrows what SPEC.md §7.2 rule 1 actually buys.
//
// The rule forbids `jsonb` because `jsonb` reorders keys and so breaks byte
// equality. That is true of the column. It is not, however, what decides the
// Go round trip: every JSON-bearing field on ADK's types is a `map[string]any`
// or an `any`, and `json.Unmarshal` into either discards key order outright.
// `json.Marshal` then re-emits the keys sorted. So a value that entered as
// `{"b":1,"a":2}` comes back `{"a":2,"b":1}` **whatever the column type is** —
// `json` and `jsonb` are indistinguishable through this path.
//
// Two consequences worth having written down:
//
//   - §7.2's property still holds for realistic events, because a realistic
//     event's args are a Go map to begin with, so `json.Marshal(E)` sorts the
//     keys too and both sides agree. The corpus entry above is exactly that
//     case and it passes.
//   - Rule 1 is still right, but for a different reason than it gives. `json`
//     matters for the bytes at rest — what the property graph, the sibling
//     memory project and anything reading the table directly will see — not for
//     the Go round trip, which cannot observe the difference.
//
// If this test ever fails, Go's map decoding started preserving order and rule
// 1's stated rationale became literally true; delete the test rather than
// working around it.
func TestGoMapDecodingNormalisesKeyOrderBeforeTheColumnCan(t *testing.T) {
	e := &adksession.Event{
		ID:        "key-order",
		Timestamp: fixedTime,
		LLMResponse: adkmodel.LLMResponse{
			CustomMetadata: map[string]any{"k": json.RawMessage(`{"b":1,"a":2}`)},
		},
	}

	rows, err := encodeEvent("s", e)
	require.NoError(t, err)
	require.Equal(t, `{"k":{"b":1,"a":2}}`, string(rows.Event.CustomMetadata),
		"the column holds the original order")

	back, err := decodeEvent(rows)
	require.NoError(t, err)
	out, err := json.Marshal(back.CustomMetadata)
	require.NoError(t, err)

	assert.Equal(t, `{"k":{"a":2,"b":1}}`, string(out),
		"decoding into map[string]any loses the order the column preserved")
}

// TestAnEmptyStateDeltaMapIsIndistinguishableFromNil records a real limitation
// rather than asserting a requirement.
//
// A row-per-key encoding has no row to write for an empty map, so `StateDelta:
// map[string]any{}` reloads as nil. It is out of reach of SPEC.md §7.2 as
// written — the spec sanctions exactly one deviation and this is not it — and
// it is recorded here so the next person to find it finds it as a known
// property with a named cause rather than as a fresh mystery.
//
// Fixing it means a nullable marker column on `agentiq.actions`, which is a
// schema change and an owner decision.
func TestAnEmptyStateDeltaMapIsIndistinguishableFromNil(t *testing.T) {
	e := &adksession.Event{
		ID:        "empty-delta",
		Timestamp: fixedTime,
		Actions:   adksession.EventActions{StateDelta: map[string]any{}},
	}
	back := roundTrip(t, e)
	assert.Nil(t, back.Actions.StateDelta,
		"an empty StateDelta map comes back nil: there is no row to write for it")

	want, err := json.Marshal(e)
	require.NoError(t, err)
	got, err := json.Marshal(back)
	require.NoError(t, err)
	assert.NotEqual(t, string(want), string(got),
		"and that is a byte difference — if this ever passes, the limitation is gone "+
			"and this test should be deleted rather than inverted")
}

// TestSection71CannotCarryEverySessionEventField names the fields of ADK's
// `session.Event` that SPEC.md §7.1 declares no column for.
//
// §7.2 states its property universally — "for any `session.Event` E" — but
// §7.1's `Event` type has no column for eight of the fields `json.Marshal`
// emits, and none of those eight carry `omitempty`. So the property holds for
// an event whose values are all zero and fails for any event carrying a
// non-zero one. `ModelVersion` and `FinishReason` are set by essentially every
// real generation, so this is not a corner case: it is every real event.
//
// This test does not assert that the gap is acceptable. It asserts that the
// gap is *this list*, so that adding a column to §7.1 makes the test fail and
// forces the list to be updated — and so that the gap is a checked fact rather
// than a discovery waiting to be made against a live database.
func TestSection71CannotCarryEverySessionEventField(t *testing.T) {
	// The fields §7.1 declares no column for, from reading §7.1's Event type
	// against google.golang.org/adk/v2/session.Event at v2.1.0.
	uncarried := map[string]string{
		"ModelVersion":            "set by every real generation",
		"FinishReason":            "set on every final response",
		"AvgLogprobs":             "set when the provider returns logprobs",
		"LogprobsResult":          "same",
		"SessionResumptionHandle": "bidi streaming",
		"InputTranscription":      "audio input",
		"OutputTranscription":     "audio output",
		"Output":                  "workflow node output",
	}

	// Every name above must really be a field of session.Event, or the list
	// has drifted from ADK and says nothing.
	et := reflect.TypeOf(adksession.Event{})
	for name := range uncarried {
		_, ok := fieldByNameIncludingEmbedded(et, name)
		assert.True(t, ok, "%s is no longer a field of session.Event; update this list", name)
	}

	// And each one must really break the round trip, or it is not uncarried.
	// Only the ones that are cheap to set generically are exercised; the
	// pointer-typed ones are covered by the list check above.
	for name, why := range map[string]string{
		"ModelVersion":            uncarried["ModelVersion"],
		"SessionResumptionHandle": uncarried["SessionResumptionHandle"],
	} {
		t.Run(name, func(t *testing.T) {
			e := &adksession.Event{ID: "gap", Timestamp: fixedTime}
			reflect.ValueOf(e).Elem().
				FieldByIndex(mustFieldIndex(t, et, name)).SetString("non-zero")

			want, err := json.Marshal(e)
			require.NoError(t, err)
			got, err := json.Marshal(roundTrip(t, e))
			require.NoError(t, err)

			assert.NotEqual(t, string(want), string(got),
				"SPEC.md §7.1 has no column for %s (%s), so §7.2's universal claim "+
					"does not hold for an event carrying it. If this now passes, §7.1 "+
					"grew the column and this entry should be removed.", name, why)
			assert.Contains(t, string(want), "non-zero")
			assert.NotContains(t, string(got), "non-zero")
		})
	}
}

func fieldByNameIncludingEmbedded(t reflect.Type, name string) (reflect.StructField, bool) {
	return t.FieldByName(name)
}

func mustFieldIndex(t *testing.T, typ reflect.Type, name string) []int {
	t.Helper()
	f, ok := typ.FieldByName(name)
	require.True(t, ok, "field %s", name)
	return f.Index
}

func hasTempKey(e *adksession.Event) bool {
	for k := range e.Actions.StateDelta {
		if strings.HasPrefix(k, adksession.KeyPrefixTemp) {
			return true
		}
	}
	return false
}
