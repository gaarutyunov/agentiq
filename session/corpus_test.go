package session

import (
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// The crafted-event corpus SPEC.md §15 and the M2 acceptance require.
//
// §15 names three things it must contain — an event with text, a thought
// signature and a function call — and that event is [corpusTextThoughtCall].
// The rest exist because each one is a way the mapping could be wrong while
// still passing that one: absent versus empty, key order, part order, a value
// that is not an object, and the state scopes.
//
// Every entry is run through the §7.2 property in TestEventRoundTripIsByteIdentical.

// corpusEntry is one crafted event and the reason it is in the corpus.
type corpusEntry struct {
	name  string
	why   string
	event *adksession.Event
}

// fixedTime keeps the corpus deterministic. `time.Now()` here would make a
// failure depend on when it ran, and the round-trip property is about the
// mapping, not about the clock.
var fixedTime = time.Date(2026, 8, 7, 12, 0, 0, 123456000, time.UTC)

func corpus() []corpusEntry {
	return []corpusEntry{
		{
			name: "text, thought signature and function call",
			why: "SPEC.md §15 names this event explicitly. It is also the one that " +
				"proves genai.Part is not a discriminated union (D4): text, thought, " +
				"thoughtSignature and functionCall are set on parts of one event, and a " +
				"schema that normalised Part into per-kind tables could not hold it.",
			event: &adksession.Event{
				ID:           "event-text-thought-call",
				Timestamp:    fixedTime,
				InvocationID: "inv-1",
				Author:       "root",
				LLMResponse: llmResponse(&genai.Content{
					Role: "model",
					Parts: []*genai.Part{
						{Text: "Let me look that up."},
						{
							Text:             "I should call the search tool.",
							Thought:          true,
							ThoughtSignature: []byte{0x00, 0x01, 0xfe, 0xff},
						},
						{
							FunctionCall: &genai.FunctionCall{
								ID:   "call-1",
								Name: "search",
								Args: map[string]any{"query": "postgres 19", "limit": float64(10)},
							},
						},
					},
				}),
			},
		},
		{
			name: "a multi-key, deeply nested JSON argument",
			why: "SPEC.md §7.2 rule 1 is about the bytes in the column, and this is " +
				"the event that puts several keys and a nested object into one. It " +
				"round-trips through a Go `map[string]any`, so the *order* it comes " +
				"back in is Go's, not the column's — see " +
				"TestGoMapDecodingNormalisesKeyOrderBeforeTheColumnCan. What this " +
				"entry does prove is that nothing is lost: every key, the nesting, " +
				"the array and the number types all survive.",
			event: &adksession.Event{
				ID:           "event-nested-args",
				Timestamp:    fixedTime,
				InvocationID: "inv-2",
				Author:       "root",
				LLMResponse: llmResponse(&genai.Content{
					Role: "model",
					Parts: []*genai.Part{{
						FunctionCall: &genai.FunctionCall{
							Name: "f",
							Args: map[string]any{
								"query":  "postgres 19",
								"limit":  float64(10),
								"nested": map[string]any{"a": float64(2), "b": float64(1)},
								"list":   []any{"x", float64(1), true, nil},
							},
						},
					}},
				}),
			},
		},
		{
			name: "part order is slice order",
			why: "SPEC.md §7.2 rule 2. part_index comes from slice position and is " +
				"read back with ORDER BY part_index, so an event whose parts are not " +
				"in a naturally sorted order is the one that catches a store reading " +
				"them back in insertion or physical order.",
			event: &adksession.Event{
				ID:           "event-part-order",
				Timestamp:    fixedTime,
				InvocationID: "inv-3",
				Author:       "root",
				LLMResponse: llmResponse(&genai.Content{
					Role: "model",
					Parts: []*genai.Part{
						{Text: "zebra"},
						{Text: "apple"},
						{Text: "mango"},
					},
				}),
			},
		},
		{
			name: "absent is not empty",
			why: "SPEC.md §7.2 rule 3. Content is present but has no parts and an " +
				"empty role; CustomMetadata is an empty non-nil map; Routes is an " +
				"empty non-nil slice. All three marshal differently from their nil " +
				"counterparts, so a mapping that collapsed either direction fails here.",
			event: &adksession.Event{
				ID:           "event-absent-vs-empty",
				Timestamp:    fixedTime,
				InvocationID: "inv-4",
				Author:       "root",
				Routes:       []string{},
				LLMResponse:  emptyNotAbsent(),
			},
		},
		{
			name: "nothing is set",
			why: "The all-nil counterpart of the entry above. Together they are what " +
				"makes rule 3 a two-sided assertion rather than a one-sided one.",
			event: &adksession.Event{
				ID:           "event-all-nil",
				Timestamp:    fixedTime,
				InvocationID: "inv-5",
				Author:       "root",
			},
		},
		{
			name: "every state scope, and a temp key",
			why: "SPEC.md §6.4 and §7.2 rule 4. app:, user: and an unprefixed key " +
				"must survive with their prefixes intact; the temp: key is the one " +
				"sanctioned loss and is asserted separately in " +
				"TestTempStateIsDroppedOnWriteAndAbsentOnRead — so this entry is " +
				"excluded from the byte-identical property and included in the scope one.",
			event: &adksession.Event{
				ID:           "event-state-scopes",
				Timestamp:    fixedTime,
				InvocationID: "inv-6",
				Author:       "root",
				Actions: adksession.EventActions{
					StateDelta: map[string]any{
						"app:theme":    "dark",
						"user:name":    "ada",
						"turns":        float64(3),
						"temp:scratch": "discard me",
					},
				},
			},
		},
		{
			name: "an error event",
			why: "Failure-matrix rows F6 and F7 end in an event carrying errorCode " +
				"and errorMessage, so the columns those land in are on the round-trip " +
				"path too.",
			event: &adksession.Event{
				ID:           "event-error",
				Timestamp:    fixedTime,
				InvocationID: "inv-7",
				Author:       "root",
				LLMResponse:  errorResponse("529", "provider overloaded"),
			},
		},
		{
			name: "a function response and inline data",
			why: "The two remaining flattened groups with a bytea column between " +
				"them. inlineData is where `Bytes` would have been, and the mapping " +
				"has to keep an empty-but-present blob distinct from an absent one.",
			event: &adksession.Event{
				ID:           "event-response-inline",
				Timestamp:    fixedTime,
				InvocationID: "inv-8",
				Author:       "tool",
				LLMResponse: llmResponse(&genai.Content{
					Role: "user",
					Parts: []*genai.Part{
						{FunctionResponse: &genai.FunctionResponse{
							ID:       "call-1",
							Name:     "search",
							Response: map[string]any{"hits": float64(2)},
						}},
						{InlineData: &genai.Blob{
							MIMEType:    "image/png",
							Data:        []byte{0x89, 0x50, 0x4e, 0x47},
							DisplayName: "shot.png",
						}},
					},
				}),
			},
		},
		{
			name: "artifact deltas and the actions flags",
			why: "The `agentiq.actions` row and its PRODUCES edge. Artifacts arrive " +
				"in M4, but the delta rows an event carries are M2's to store.",
			event: &adksession.Event{
				ID:           "event-actions",
				Timestamp:    fixedTime,
				InvocationID: "inv-9",
				Author:       "root",
				Actions: adksession.EventActions{
					SkipSummarization: true,
					TransferToAgent:   "researcher",
					Escalate:          true,
					ArtifactDelta:     map[string]int64{"report.md": 2, "chart.png": 1},
				},
			},
		},
	}
}

// llmResponse builds the embedded response with just a Content set. It exists
// so the corpus entries read as documents rather than as struct literals with
// one field buried in them.
func llmResponse(c *genai.Content) adkmodel.LLMResponse {
	return adkmodel.LLMResponse{Content: c}
}

func errorResponse(code, message string) adkmodel.LLMResponse {
	return adkmodel.LLMResponse{ErrorCode: code, ErrorMessage: message}
}

// emptyNotAbsent sets Content and CustomMetadata to non-nil empty values at
// once, for the entry where rule 3's "set-but-empty" side is under test.
func emptyNotAbsent() adkmodel.LLMResponse {
	return adkmodel.LLMResponse{Content: &genai.Content{}, CustomMetadata: map[string]any{}}
}
