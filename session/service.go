package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/gaarutyunov/gopgql/exec"
	adksession "google.golang.org/adk/v2/session"

	"github.com/gaarutyunov/agentiq/dbosadk"
	"github.com/gaarutyunov/agentiq/generated/client"
)

// The append policy of SPEC.md §9.2's step taxonomy: three retries, base one
// second, exponential. They are spec'd numbers rather than DBOS's defaults,
// which are zero retries at 100ms.
//
// Three and not the model call's five, because the two failures are not alike.
// A model call fails because a provider is overloaded and will very likely
// succeed later; an append fails because the database rejected it, and the
// third attempt at a violated constraint fails the same way as the first.
const (
	AppendMaxRetries   = 3
	AppendBaseInterval = time.Second
)

// UnknownAgentDigest is the `agent_digest` a session gets when the caller did
// not name one.
//
// `agentiq.session.agent_digest` is NOT NULL and ADK's `CreateRequest` has no
// field for it — the digest is AgentIQ's own (SPEC.md §6.3), pinned by the
// workflow input. A placeholder is what lets `sessiontestsuite`, which knows
// nothing about digests, create sessions at all; it is deliberately not a valid
// digest, so a row carrying it is legible as "created outside a run".
const UnknownAgentDigest = "sha256:unknown"

// ErrNoHandle is returned when the service is asked to do something outside a
// DBOS workflow and was constructed without [WithHandle].
//
// DBOS hands out transactions, not connections: a `*dbos.DataSource` has no
// exported way to reach the pool it wraps, so the only statements a service
// built from one alone can issue are the ones inside `dbos.RunAsTransaction`.
// That covers every call from workflow code and none from anywhere else.
var ErrNoHandle = errors.New("session: no database handle outside a DBOS workflow; pass session.WithHandle")

// ErrSessionNotFound is returned by Get and AppendEvent for a session that is
// not stored.
//
// `sessiontestsuite` requires both: Get after Delete must fail, and
// AppendEvent against a session that was never created must fail. The latter is
// why the surrogate-id lookup an append needs cannot be folded into
// `createSession`'s upsert — an upsert would make it succeed.
var ErrSessionNotFound = errors.New("session: not found")

// Option configures the service.
//
// SPEC.md §8.3 writes the constructor as `New(ds *dbos.DataSource)` and that
// call still compiles: the options are additive, and every write from workflow
// code works without any of them.
type Option func(*service)

// WithHandle supplies the handle used when the caller is not inside a DBOS
// workflow — the conformance suite, the demo's read path, and any read at all,
// since a read is not a transaction to join.
//
// It is an `exec.Handle` rather than a pool because SPEC.md §5 keeps `pgx` out
// of this package: the caller builds `exec.Pgx(pool)` over the same pool the
// DataSource was made from, and `session` never learns which driver that was.
func WithHandle(h exec.Handle) Option {
	return func(s *service) { s.handle = h }
}

// WithAgentDigest sets the `agent_digest` written on sessions this service
// creates (SPEC.md §6.3). It defaults to [UnknownAgentDigest].
func WithAgentDigest(digest string) Option {
	return func(s *service) {
		if digest != "" {
			s.digest = digest
		}
	}
}

// WithWorkflowUUID records which DBOS workflow owns the sessions this service
// creates, which is what makes the `Workflow ──HAS_SESSION──▶ Session` edge of
// SPEC.md §7.3 resolve.
func WithWorkflowUUID(uuid string) Option {
	return func(s *service) {
		if uuid != "" {
			s.workflowUUID = &uuid
		}
	}
}

// New returns the ADK `session.Service` backed by the `agentiq.*` tables
// through the generated client (SPEC.md §8.3).
//
// Every AppendEvent runs inside `dbos.RunAsTransaction`, so the event rows and
// the step checkpoint commit atomically (D2). Partial events are dropped (D5).
func New(ds *dbos.DataSource, opts ...Option) adksession.Service {
	s := &service{ds: ds, digest: UnknownAgentDigest, client: client.New()}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

type service struct {
	ds           *dbos.DataSource
	handle       exec.Handle
	client       *client.Client
	digest       string
	workflowUUID *string
}

var _ adksession.Service = (*service)(nil)

// workflowContext reports whether ctx is running inside a DBOS workflow — the
// one place `dbos.RunAsTransaction` and `dbos.RunAsStep` can be called from.
//
// The ADK Runner passes the context it was handed straight through to the
// session service, so inside `workflow.AgentRun` this is the workflow's own
// context and outside it (the conformance suite, the demo's read path) it is
// not. Detecting it rather than requiring the caller to say which they are is
// what lets one service satisfy both.
func workflowContext(ctx context.Context) (dbos.Context, bool) {
	// Through dbosadk, because a type assertion does not survive the Runner:
	// ADK wraps the context in its own `agent.InvocationContext` before calling
	// the session service, and that wrapper is a `context.Context` and nothing
	// more. The workflow marks itself in the context once and the value
	// survives every wrapping. See dbosadk.WithWorkflowContext.
	dctx, ok := dbosadk.WorkflowContext(ctx)
	if !ok {
		return nil, false
	}
	if _, err := dbos.GetWorkflowID(dctx); err != nil {
		return nil, false
	}
	return dctx, true
}

// write runs fn on the transaction DBOS opened, or — outside a workflow — on
// the handle from [WithHandle].
//
// R is the value that crosses the checkpoint, so every caller passes something
// DBOS can serialize and replay: a `string` id or a row count, never a struct
// full of `any`.
func write[R any](ctx context.Context, s *service, name string, fn func(context.Context, exec.Handle) (R, error)) (R, error) {
	if dctx, ok := workflowContext(ctx); ok {
		return dbos.RunAsTransaction(dctx, s.ds, func(c context.Context, tx dbos.Tx) (R, error) {
			// `exec.Portable` is what makes D2 work: `dbos.Tx` is
			// driver-agnostic and so, since gopgql v0.3.0, is `exec.Handle`, so
			// the generated call runs on DBOS's own transaction rather than
			// opening a connection of its own.
			return fn(c, exec.Portable(tx))
		},
			dbos.WithStepName(name),
			dbos.WithStepMaxRetries(AppendMaxRetries),
			dbos.WithStepBaseInterval(AppendBaseInterval),
		)
	}
	var zero R
	if s.handle == nil {
		return zero, ErrNoHandle
	}
	return fn(ctx, s.handle)
}

// read runs fn and, inside a workflow, checkpoints its result.
//
// The checkpoint is the JSON of the rows rather than the rows themselves,
// because the generated result types carry `*any` for every `json` column and
// an interface has nothing for a binary encoder to dispatch on. It is also the
// reason a read has to be checkpointed at all: a replay that re-queried would
// see the events the first attempt appended and take a different path from the
// one being replayed.
//
// The query still runs on the plain handle. A read joins no transaction — there
// is nothing to commit with it — and `dbos.RunAsStep` is the boundary that
// records what it returned.
func read[R any](ctx context.Context, s *service, name string, fn func(context.Context, exec.Handle) (R, error)) (R, error) {
	var zero R
	if s.handle == nil {
		return zero, ErrNoHandle
	}
	if dctx, ok := workflowContext(ctx); ok {
		raw, err := dbos.RunAsStep(dctx, func(c context.Context) ([]byte, error) {
			v, err := fn(c, s.handle)
			if err != nil {
				return nil, err
			}
			return json.Marshal(v)
		}, dbos.WithStepName(name))
		if err != nil {
			return zero, err
		}
		var out R
		if err := json.Unmarshal(raw, &out); err != nil {
			return zero, fmt.Errorf("session: replay %s: %w", name, err)
		}
		return out, nil
	}
	return fn(ctx, s.handle)
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// Create upserts the session row and its initial state.
//
// An empty `SessionID` is allocated by `agentiq.create_session` and comes back
// as the returned id. Allocating it in Go would be a uuid drawn in workflow
// code; allocated in the function it is a value the surrounding transaction
// checkpoints, so a replay is handed the id the first attempt committed.
func (s *service) Create(ctx context.Context, req *adksession.CreateRequest) (*adksession.CreateResponse, error) {
	if req == nil {
		return nil, errors.New("session: Create with a nil request")
	}

	state, err := stateDocument(req.State)
	if err != nil {
		return nil, err
	}

	adkID, err := write(ctx, s, "session.Create", func(c context.Context, h exec.Handle) (string, error) {
		return s.client.CreateSession(c, h, client.CreateSessionInput{
			AdkId:        req.SessionID,
			AppName:      req.AppName,
			UserId:       req.UserID,
			AgentDigest:  s.digest,
			WorkflowUuid: s.workflowUUID,
			State:        state,
		})
	})
	if err != nil {
		return nil, fmt.Errorf("session: create: %w", err)
	}

	// Read back rather than assembling from the request: the row may be one an
	// earlier turn created, with state this call did not set, and the upsert's
	// whole point is that the caller cannot tell which.
	got, err := s.Get(ctx, &adksession.GetRequest{
		AppName: req.AppName, UserID: req.UserID, SessionID: adkID,
	})
	if err != nil {
		return nil, err
	}
	return &adksession.CreateResponse{Session: got.Session}, nil
}

// Get loads one session with its events, their parts and its projected state.
func (s *service) Get(ctx context.Context, req *adksession.GetRequest) (*adksession.GetResponse, error) {
	if req == nil {
		return nil, errors.New("session: Get with a nil request")
	}

	rows, err := read(ctx, s, "session.Get", func(c context.Context, h exec.Handle) ([]client.SessionSession, error) {
		return s.client.Session(c, h, client.SessionInput{
			AppName: req.AppName, UserId: req.UserID, AdkId: req.SessionID,
		})
	})
	if err != nil {
		return nil, fmt.Errorf("session: get: %w", err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%w: %s/%s/%s", ErrSessionNotFound, req.AppName, req.UserID, req.SessionID)
	}

	sess, err := sessionFromRow(rows[0])
	if err != nil {
		return nil, err
	}

	// §6.4's two shared scopes. `app:` is application-wide and `user:` is per
	// user, but the rows hang off a session, so the sharing has to be assembled
	// on read from the sessions the scope covers. Without this a session created
	// after an `app:` key was set reports it as absent.
	shared, err := read(ctx, s, "session.SharedState", func(c context.Context, h exec.Handle) ([]client.SharedStateSession, error) {
		return s.client.SharedState(c, h, client.SharedStateInput{AppName: req.AppName})
	})
	if err != nil {
		return nil, fmt.Errorf("session: get shared state: %w", err)
	}
	mergeSharedState(sess, rows[0].Id, req.UserID, shared)

	// The two filters are applied here rather than in the traversal because
	// they compose: §7.1's ordering guarantee is `sequence`, and "the most
	// recent N of those after T" is not what a SQL LIMIT over a time predicate
	// returns unless the predicate is applied first.
	if !req.After.IsZero() {
		kept := sess.events[:0]
		for _, e := range sess.events {
			if !e.Timestamp.Before(req.After) {
				kept = append(kept, e)
			}
		}
		sess.events = kept
	}
	if n := req.NumRecentEvents; n > 0 && len(sess.events) > n {
		sess.events = sess.events[len(sess.events)-n:]
	}

	return &adksession.GetResponse{Session: sess}, nil
}

// List returns the sessions of one user, without their events.
//
// ADK's `ListResponse` carries `session.Session` values and the in-memory
// implementation returns them event-free; loading every conversation to answer
// "which conversations are there" would make the cost of the list the size of
// the history.
func (s *service) List(ctx context.Context, req *adksession.ListRequest) (*adksession.ListResponse, error) {
	if req == nil {
		return nil, errors.New("session: List with a nil request")
	}

	// An empty UserID means every user of the app, which is a different
	// statement and therefore a different compiled query: gopgql turns each
	// operation into one SQL statement, so an argument that is sometimes a
	// filter and sometimes not would have to be two statements anyway.
	rows, err := read(ctx, s, "session.List", func(c context.Context, h exec.Handle) ([]listRow, error) {
		if req.UserID == "" {
			got, err := s.client.SessionsByApp(c, h, client.SessionsByAppInput{AppName: req.AppName})
			if err != nil {
				return nil, err
			}
			out := make([]listRow, 0, len(got))
			for _, r := range got {
				out = append(out, listRow{ID: r.AdkId, AppName: r.AppName, UserID: r.UserId, LastUpdateAt: r.LastUpdateAt})
			}
			return out, nil
		}
		got, err := s.client.Sessions(c, h, client.SessionsInput{AppName: req.AppName, UserId: req.UserID})
		if err != nil {
			return nil, err
		}
		out := make([]listRow, 0, len(got))
		for _, r := range got {
			out = append(out, listRow{ID: r.AdkId, AppName: r.AppName, UserID: r.UserId, LastUpdateAt: r.LastUpdateAt})
		}
		return out, nil
	})
	if err != nil {
		return nil, fmt.Errorf("session: list: %w", err)
	}

	out := make([]adksession.Session, 0, len(rows))
	for _, r := range rows {
		out = append(out, &storedSession{
			id: r.ID, appName: r.AppName, userID: r.UserID,
			lastUpdate: r.LastUpdateAt, state: map[string]any{},
		})
	}
	return &adksession.ListResponse{Sessions: out}, nil
}

// Delete removes the session and everything hanging off it. Deleting a session
// that is not there is a success: ADK's Delete has no "not found", and
// `sessiontestsuite` calls it on sessions it never created.
func (s *service) Delete(ctx context.Context, req *adksession.DeleteRequest) error {
	if req == nil {
		return errors.New("session: Delete with a nil request")
	}
	_, err := write(ctx, s, "session.Delete", func(c context.Context, h exec.Handle) (int64, error) {
		return s.client.DeleteSession(c, h, client.DeleteSessionInput{
			AdkId: req.SessionID, AppName: req.AppName, UserId: req.UserID,
		})
	})
	if err != nil {
		return fmt.Errorf("session: delete: %w", err)
	}
	return nil
}

// AppendEvent writes one event across five tables inside the transaction DBOS
// opened, so the rows and the step checkpoint commit together (D2).
//
// A partial event is dropped and reported as stored — ADK's Runner appends
// every event it yields, including the partials it streams, and an error there
// would abort a turn that is behaving correctly. D5 makes the drop mandatory;
// `dbosadk.NewModel` is what puts the partials somewhere they can still be
// read (SPEC.md §9.4).
func (s *service) AppendEvent(ctx context.Context, sess adksession.Session, e *adksession.Event) error {
	if sess == nil {
		return errors.New("session: AppendEvent with a nil session")
	}
	if e == nil {
		return errors.New("session: AppendEvent with a nil event")
	}
	if e.Partial {
		// Dropped, and reported as stored. ADK's Runner appends every event it
		// yields, partials included, and an error here would abort a turn that
		// is behaving correctly. The snapshot is left alone too: a partial is
		// not part of the conversation, and `dbosadk.NewModel` has already put
		// it where it can be read (§9.4).
		return nil
	}

	_, err := write(ctx, s, "session.AppendEvent", func(c context.Context, h exec.Handle) (string, error) {
		// The surrogate uuid the append is keyed on, resolved inside the same
		// transaction. It cannot be resolved before it — the session could be
		// deleted between the lookup and the write — and it cannot be an upsert,
		// because appending to a session that does not exist has to fail.
		refs, err := s.client.SessionRef(c, h, client.SessionRefInput{
			AppName: sess.AppName(), UserId: sess.UserID(), AdkId: sess.ID(),
		})
		if err != nil {
			return "", err
		}
		if len(refs) == 0 {
			return "", fmt.Errorf("%w: %s/%s/%s", ErrSessionNotFound, sess.AppName(), sess.UserID(), sess.ID())
		}

		docs, err := EncodeForAppend(refs[0].Id, e)
		if err != nil {
			return "", err
		}
		return s.client.AppendEvent(c, h, client.AppendEventInput{
			SessionId:      refs[0].Id,
			Event:          docs.Event,
			Parts:          docs.Parts,
			Actions:        docs.Actions,
			StateDeltas:    docs.StateDeltas,
			ArtifactDeltas: docs.ArtifactDeltas,
		})
	})
	if err != nil {
		return fmt.Errorf("session: append event %q: %w", e.ID, err)
	}

	applyToSnapshot(sess, e)
	return nil
}

// applyToSnapshot mirrors the append onto the in-memory session the caller
// holds, which is what ADK's own implementations do and what its Runner
// depends on.
//
// It is not bookkeeping. The Runner loads the session once at the top of a
// turn, appends the user's message through this method, and then builds the
// model request from `session.Events()` — off the object it is holding, not off
// a fresh load. A service that wrote only to the database left that object
// empty, and the request went out with no contents at all:
//
//	openai: LLM request has no contents to convert
//
// retried five times, with the endpoint never called, because the failure is
// client-side. Nothing in that message points here.
//
// `temp:` keys are applied to the snapshot even though they are not stored.
// That is the same asymmetry §6.4 describes: `temp:` is per-invocation state,
// visible to the turn that set it and gone afterwards. Dropping it here too
// would make it invisible to the turn as well, which is not what "temporary"
// means.
func applyToSnapshot(sess adksession.Session, e *adksession.Event) {
	snapshot, ok := sess.(*storedSession)
	if !ok {
		// A session this service did not load — the conformance suite's own
		// double, or a caller's stand-in. It has no snapshot to keep current
		// and the durable write has already happened.
		return
	}
	snapshot.events = append(snapshot.events, e)
	for key, value := range e.Actions.StateDelta {
		snapshot.state[key] = value
	}
}

// ---------------------------------------------------------------------------
// Rows to ADK values
// ---------------------------------------------------------------------------

// listRow is what List checkpoints: the two compiled queries return different
// generated types with the same four columns, and the step boundary needs one
// type to serialize.
type listRow struct {
	ID           string
	AppName      string
	UserID       string
	LastUpdateAt time.Time
}

// mergeSharedState overlays SPEC.md §6.4's `app:` and `user:` scopes onto a
// loaded session.
//
// The session's own rows already carry its app- and user-scoped keys, and they
// are re-applied here from the same result, so a key held by two sessions
// resolves the same way whichever session is being loaded: last write wins by
// `updated_at`, which is what a delta means.
//
// `temp:` cannot appear — those keys were dropped on write (§7.2 rule 4) and no
// row can carry that scope — so there is nothing to filter for.
func mergeSharedState(sess *storedSession, sessionID, userID string, shared []client.SharedStateSession) {
	type stamped struct {
		value any
		at    time.Time
	}
	newest := map[string]stamped{}

	for _, s := range shared {
		for _, st := range s.State {
			switch st.Scope {
			case ScopeApp:
				// Application-wide: every session of the app contributes.
			case ScopeUser:
				if s.UserId != userID {
					continue
				}
			default:
				// Session scope belongs to one session and is already loaded
				// from it; taking it from here would let another session's key
				// of the same name overwrite this one's.
				if s.Id != sessionID {
					continue
				}
			}
			key := Key(st.Scope, st.Key)
			if prev, ok := newest[key]; ok && prev.at.After(st.UpdatedAt) {
				continue
			}
			newest[key] = stamped{value: st.Value, at: st.UpdatedAt}
		}
	}

	for key, v := range newest {
		sess.state[key] = v.value
	}
}

func sessionFromRow(row client.SessionSession) (*storedSession, error) {
	sess := &storedSession{
		id:         row.AdkId,
		appName:    row.AppName,
		userID:     row.UserId,
		lastUpdate: row.LastUpdateAt,
		state:      make(map[string]any, len(row.State)),
	}

	for _, st := range row.State {
		// Key restores the `app:` / `user:` prefix the scope column replaced
		// (§6.4), which is what makes the key come back as ADK wrote it. No row
		// can carry `temp` — those keys were dropped on write (§7.2 rule 4) —
		// so nothing here has to filter for them.
		sess.state[Key(st.Scope, st.Key)] = st.Value
	}

	// §7.1's ordering guarantee. The traversal orders each selection by its key
	// columns and Event's key is a surrogate uuid, so the events arrive in no
	// order at all until this puts them in one.
	events := make([]client.SessionSessionEvents, len(row.Events))
	copy(events, row.Events)
	slices.SortStableFunc(events, func(a, b client.SessionSessionEvents) int {
		switch {
		case a.Sequence < b.Sequence:
			return -1
		case a.Sequence > b.Sequence:
			return 1
		default:
			return 0
		}
	})

	for _, er := range events {
		rows, err := eventRowsFromSession(row.Id, er)
		if err != nil {
			return nil, err
		}
		e, err := decodeEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("session: decode event %q: %w", er.AdkId, err)
		}
		sess.events = append(sess.events, e)
	}

	return sess, nil
}

// stateDocument projects an initial state map onto the `agentiq.session_state`
// rows `agentiq.create_session` unpacks, dropping `temp:` keys (§6.4, §7.2
// rule 4).
func stateDocument(state map[string]any) (json.RawMessage, error) {
	rows := make([]StateDeltaRow, 0, len(state))
	// Sorted so the document is a function of the map's contents and not of Go's
	// map iteration order: `go generate` output and a checkpointed step argument
	// both have to be reproducible.
	for _, key := range slices.Sorted(maps.Keys(state)) {
		scope, stored, persist := ScopeOf(key)
		if !persist {
			continue
		}
		value, err := json.Marshal(state[key])
		if err != nil {
			return nil, fmt.Errorf("session: encode initial state %q: %w", key, err)
		}
		rows = append(rows, StateDeltaRow{Scope: scope, Key: stored, Value: value})
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		return nil, fmt.Errorf("session: encode the initial state document: %w", err)
	}
	return raw, nil
}

// ---------------------------------------------------------------------------
// session.Session
// ---------------------------------------------------------------------------

// storedSession is one loaded session. It is a snapshot: ADK reads it for the
// duration of a turn and the store is what the next turn reads.
type storedSession struct {
	id         string
	appName    string
	userID     string
	lastUpdate time.Time
	state      map[string]any
	events     []*adksession.Event
}

func (s *storedSession) ID() string                { return s.id }
func (s *storedSession) AppName() string           { return s.appName }
func (s *storedSession) UserID() string            { return s.userID }
func (s *storedSession) LastUpdateTime() time.Time { return s.lastUpdate }
func (s *storedSession) State() adksession.State   { return sessionState(s.state) }
func (s *storedSession) Events() adksession.Events { return eventList(s.events) }

// sessionState is the loaded state, keyed as ADK wrote it.
//
// Set writes to the snapshot and not to the database, matching ADK's in-memory
// implementation: state reaches the tables as an event's `StateDelta`, through
// AppendEvent, which is the only path that has a transaction to commit it in.
// A Set that wrote directly would be a write outside `RunAsTransaction` and
// would not be replayed with the turn that made it.
type sessionState map[string]any

func (m sessionState) Get(key string) (any, error) {
	v, ok := m[key]
	if !ok {
		return nil, adksession.ErrStateKeyNotExist
	}
	return v, nil
}

func (m sessionState) Set(key string, value any) error {
	m[key] = value
	return nil
}

func (m sessionState) All() iter.Seq2[string, any] {
	return func(yield func(string, any) bool) {
		for k, v := range m {
			if !yield(k, v) {
				return
			}
		}
	}
}

// eventList is the loaded events, in `sequence` order.
type eventList []*adksession.Event

func (l eventList) Len() int                   { return len(l) }
func (l eventList) At(i int) *adksession.Event { return l[i] }
func (l eventList) All() iter.Seq[*adksession.Event] {
	return func(yield func(*adksession.Event) bool) {
		for _, e := range l {
			if !yield(e) {
				return
			}
		}
	}
}
