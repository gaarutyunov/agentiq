package wasmpg

import (
	"encoding/binary"
	"fmt"
)

// Frontend message type bytes the multiplexer cares about. Everything else is
// forwarded to the backend untouched, so the list is deliberately short.
const (
	feQuery     = 'Q' // Query — simple query; carries SQL
	feParse     = 'P' // Parse — extended query; carries SQL
	feTerminate = 'X' // Terminate — closes the logical connection
)

// Backend message type bytes this package encodes or inspects.
const (
	beAuthentication       = 'R'
	beBackendKeyData       = 'K'
	beParameterStatus      = 'S'
	beNotificationResponse = 'A'
	beReadyForQuery        = 'Z'
	beErrorResponse        = 'E'

	// readyIdle is the ReadyForQuery transaction status meaning "idle, not in
	// a transaction block" — what a freshly synthesised handshake reports.
	readyIdle = 'I'

	// sslRefused is the bare byte, with no length and no framing, that
	// answers an SSLRequest on a server built without SSL support
	// (SPEC.md §12.3).
	sslRefused byte = 'N'
)

// Startup-phase request codes. These messages have no type byte: they are a
// length followed by the code, which is how a StartupMessage is told apart
// from an SSLRequest.
const (
	protocolVersion3  int32 = 196608 // 3.0
	sslRequestCode    int32 = 80877103
	gssEncRequestCode int32 = 80877104
	cancelRequestCode int32 = 80877102
)

// maxMessageSize caps how large a single frontend message may claim to be.
// pgx never sends anything near it; a larger length means the byte stream has
// desynchronised, and failing here turns that into an error on the connection
// rather than a multi-gigabyte allocation inside a browser tab.
const maxMessageSize = 64 << 20

// errMessageTooLarge and friends surface as connection failures, which is the
// only honest outcome once framing is lost.
var (
	errMessageTooLarge = fmt.Errorf("wasmpg: frontend message exceeds %d bytes", maxMessageSize)
	errMessageTooSmall = fmt.Errorf("wasmpg: frontend message length is below its own header")
)

// frontendMessage is one complete message read off the write buffer.
//
// Startup-phase messages (SSLRequest, GSSENCRequest, CancelRequest,
// StartupMessage) have no type byte, so typ is zero and code carries the
// request code. Everything after the StartupMessage has a type byte and a zero
// code.
type frontendMessage struct {
	typ     byte
	code    int32
	raw     []byte // the whole message, header included
	payload []byte // the body after the header
}

// nextFrontendMessage decodes the first complete message in buf. n is how many
// bytes it consumed; n == 0 with a nil error means buf holds only part of a
// message and the caller should wait for more.
//
// startup selects the untyped framing: a 4-byte self-inclusive length followed
// by a 4-byte request code. It stays true until a StartupMessage is seen,
// because an SSLRequest is answered with a bare 'N' and pgx then sends its
// StartupMessage in the same untyped framing.
func nextFrontendMessage(buf []byte, startup bool) (frontendMessage, int, error) {
	if startup {
		if len(buf) < 8 {
			return frontendMessage{}, 0, nil
		}
		length := int(int32(binary.BigEndian.Uint32(buf[0:4])))
		if length < 8 {
			return frontendMessage{}, 0, errMessageTooSmall
		}
		if length > maxMessageSize {
			return frontendMessage{}, 0, errMessageTooLarge
		}
		if len(buf) < length {
			return frontendMessage{}, 0, nil
		}
		return frontendMessage{
			code:    int32(binary.BigEndian.Uint32(buf[4:8])),
			raw:     buf[:length],
			payload: buf[8:length],
		}, length, nil
	}

	if len(buf) < 5 {
		return frontendMessage{}, 0, nil
	}
	length := int(int32(binary.BigEndian.Uint32(buf[1:5])))
	if length < 4 {
		return frontendMessage{}, 0, errMessageTooSmall
	}
	if length > maxMessageSize {
		return frontendMessage{}, 0, errMessageTooLarge
	}
	total := length + 1
	if len(buf) < total {
		return frontendMessage{}, 0, nil
	}
	return frontendMessage{
		typ:     buf[0],
		raw:     buf[:total],
		payload: buf[5:total],
	}, total, nil
}

// sql extracts the statement text from a Query or Parse message. Anything else
// yields an empty string, which the LISTEN observer treats as "nothing to see".
func (m frontendMessage) sql() string {
	switch m.typ {
	case feQuery:
		s, _, ok := cstring(m.payload)
		if !ok {
			return ""
		}
		return s
	case feParse:
		// Parse is: statement name, query, parameter count, parameter OIDs.
		_, rest, ok := cstring(m.payload)
		if !ok {
			return ""
		}
		s, _, ok := cstring(rest)
		if !ok {
			return ""
		}
		return s
	default:
		return ""
	}
}

// cstring reads a NUL-terminated string off the front of b.
func cstring(b []byte) (string, []byte, bool) {
	for i, c := range b {
		if c == 0 {
			return string(b[:i]), b[i+1:], true
		}
	}
	return "", nil, false
}

// encodeBackend frames a backend message: type byte, self-inclusive 4-byte
// length, payload.
func encodeBackend(typ byte, payload []byte) []byte {
	out := make([]byte, 0, len(payload)+5)
	out = append(out, typ)
	out = appendInt32(out, int32(len(payload)+4))
	return append(out, payload...)
}

func appendInt32(b []byte, v int32) []byte {
	return binary.BigEndian.AppendUint32(b, uint32(v))
}

func appendCString(b []byte, s string) []byte {
	return append(append(b, s...), 0)
}

// encodeAuthenticationOk is AuthenticationOk: an authentication message whose
// sub-type is 0. PGlite runs in single-user mode and never authenticates, so
// this is synthesised rather than proxied (SPEC.md §12.3).
func encodeAuthenticationOk() []byte {
	return encodeBackend(beAuthentication, appendInt32(nil, 0))
}

func encodeParameterStatus(name, value string) []byte {
	payload := appendCString(nil, name)
	payload = appendCString(payload, value)
	return encodeBackend(beParameterStatus, payload)
}

// encodeBackendKeyData carries the PID and cancellation secret pgx records for
// out-of-band query cancellation. There is no second connection to cancel over
// against a single PGlite backend, so both values are synthetic (SPEC.md §12.3).
func encodeBackendKeyData(pid, secret int32) []byte {
	payload := appendInt32(nil, pid)
	payload = appendInt32(payload, secret)
	return encodeBackend(beBackendKeyData, payload)
}

func encodeReadyForQuery(status byte) []byte {
	return encodeBackend(beReadyForQuery, []byte{status})
}

// encodeNotificationResponse builds the 'A' frame the multiplexer injects into
// every logical connection listening on channel (SPEC.md §12.4). PGlite's
// onNotification callback reports no originating PID, so the multiplexer's own
// synthetic backend PID is used.
func encodeNotificationResponse(pid int32, channel, payload string) []byte {
	body := appendInt32(nil, pid)
	body = appendCString(body, channel)
	body = appendCString(body, payload)
	return encodeBackend(beNotificationResponse, body)
}

// encodeErrorResponse reports a shim-level failure in a shape pgx understands,
// so that a fault in the transport surfaces as a pgconn.PgError rather than as
// a torn connection. Failure-matrix row F20 depends on errors arriving this
// way.
func encodeErrorResponse(severity, code, message string) []byte {
	body := []byte{'S'}
	body = appendCString(body, severity)
	body = append(body, 'V')
	body = appendCString(body, severity)
	body = append(body, 'C')
	body = appendCString(body, code)
	body = append(body, 'M')
	body = appendCString(body, message)
	body = append(body, 0)
	return encodeBackend(beErrorResponse, body)
}

// nextBackendMessage frames one backend message out of buf, the same way
// nextFrontendMessage does for the write side. It is used to walk what
// execProtocol handed back, which is a concatenation of complete messages.
func nextBackendMessage(buf []byte) (typ byte, total int, ok bool) {
	if len(buf) < 5 {
		return 0, 0, false
	}
	length := int(int32(binary.BigEndian.Uint32(buf[1:5])))
	if length < 4 || length > maxMessageSize {
		return 0, 0, false
	}
	if len(buf) < length+1 {
		return 0, 0, false
	}
	return buf[0], length + 1, true
}

// notification is a NotificationResponse decoded back out of a backend
// message, so that one found inline in an execProtocol result can be routed
// through the same table as one delivered by onNotification.
type notification struct {
	pid     int32
	channel string
	payload string
}

// splitNotifications separates the NotificationResponse frames out of a
// backend byte stream.
//
// A notification found here reached exactly one logical connection — whichever
// happened to issue the statement that triggered it — which is precisely the
// mis-routing the channel table exists to correct. So it never stays in the
// stream; it is either dropped, because PGlite's execProtocol also dispatches
// it to onNotification, or re-routed through the table when
// [Config.RouteInlineNotifications] says the onNotification path is not to be
// trusted.
//
// A trailing partial frame is left in clean untouched; execProtocol returns
// whole messages, so that should not happen, and passing the bytes through is
// less destructive than guessing.
func splitNotifications(buf []byte) (clean []byte, found []notification) {
	i := 0
	for i < len(buf) {
		typ, total, ok := nextBackendMessage(buf[i:])
		if !ok {
			clean = append(clean, buf[i:]...)
			return clean, found
		}
		frame := buf[i : i+total]
		if typ == beNotificationResponse {
			if n, ok := decodeNotification(frame[5:]); ok {
				found = append(found, n)
			}
		} else {
			clean = append(clean, frame...)
		}
		i += total
	}
	return clean, found
}

func decodeNotification(payload []byte) (notification, bool) {
	if len(payload) < 4 {
		return notification{}, false
	}
	pid := int32(binary.BigEndian.Uint32(payload[0:4]))
	channel, rest, ok := cstring(payload[4:])
	if !ok {
		return notification{}, false
	}
	text, _, ok := cstring(rest)
	if !ok {
		return notification{}, false
	}
	return notification{pid: pid, channel: channel, payload: text}, true
}
