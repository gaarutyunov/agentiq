package wasmpg

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The encoders are asserted byte for byte rather than round-tripped through
// themselves. A decoder that agrees with a wrong encoder proves nothing; pgx
// is the real reader, and these are the bytes the PostgreSQL wire protocol
// specifies it will get.

func TestEncodeAuthenticationOk(t *testing.T) {
	assert.Equal(t, []byte{'R', 0, 0, 0, 8, 0, 0, 0, 0}, encodeAuthenticationOk())
}

func TestEncodeParameterStatus(t *testing.T) {
	got := encodeParameterStatus("client_encoding", "UTF8")
	want := []byte{'S', 0, 0, 0, 25}
	want = append(want, "client_encoding\x00UTF8\x00"...)
	assert.Equal(t, want, got)

	// Length is self-inclusive: it counts itself but not the type byte.
	assert.Equal(t, len(got)-1, int(binary.BigEndian.Uint32(got[1:5])))
}

func TestEncodeBackendKeyData(t *testing.T) {
	got := encodeBackendKeyData(40001, 0x5eed)
	assert.Equal(t, []byte{'K', 0, 0, 0, 12, 0, 0, 0x9c, 0x41, 0, 0, 0x5e, 0xed}, got)
}

func TestEncodeReadyForQuery(t *testing.T) {
	assert.Equal(t, []byte{'Z', 0, 0, 0, 5, 'I'}, encodeReadyForQuery(readyIdle))
}

func TestEncodeNotificationResponse(t *testing.T) {
	got := encodeNotificationResponse(40000, "dbos_notifications_channel", `{"id":1}`)

	want := []byte{'A'}
	body := []byte{0, 0, 0x9c, 0x40}
	body = append(body, "dbos_notifications_channel\x00"...)
	body = append(body, `{"id":1}`...)
	body = append(body, 0)
	want = binary.BigEndian.AppendUint32(want, uint32(len(body)+4))
	want = append(want, body...)

	assert.Equal(t, want, got)

	// And it decodes back to what went in, which is what pgx's
	// OnNotification callback will report.
	n, ok := decodeNotification(got[5:])
	require.True(t, ok)
	assert.Equal(t, notification{pid: 40000, channel: "dbos_notifications_channel", payload: `{"id":1}`}, n)
}

func TestEncodeErrorResponse(t *testing.T) {
	got := encodeErrorResponse("FATAL", "08P01", "boom")
	want := []byte{'E', 0, 0, 0, 32}
	want = append(want, "SFATAL\x00VFATAL\x00C08P01\x00Mboom\x00\x00"...)
	assert.Equal(t, want, got)
}

func TestHandshakeByteSequence(t *testing.T) {
	m, err := New(Config{Exec: nullExec})
	require.NoError(t, err)

	got := m.handshake(40001)

	// SPEC.md §12.3 fixes both the set and the order.
	var want []byte
	want = append(want, encodeAuthenticationOk()...)
	want = append(want, encodeParameterStatus("server_version", "19.0")...)
	want = append(want, encodeParameterStatus("client_encoding", "UTF8")...)
	want = append(want, encodeParameterStatus("DateStyle", "ISO, MDY")...)
	want = append(want, encodeParameterStatus("TimeZone", "UTC")...)
	want = append(want, encodeParameterStatus("integer_datetimes", "on")...)
	want = append(want, encodeBackendKeyData(40001, 40001^0x5eed)...)
	want = append(want, encodeReadyForQuery('I')...)

	assert.Equal(t, want, got)
	assert.Equal(t, []byte{'R', 'S', 'S', 'S', 'S', 'S', 'K', 'Z'}, messageTypes(t, got))
}

func TestNextFrontendMessageStartupFraming(t *testing.T) {
	ssl := startupMessage(sslRequestCode, nil)

	// A short buffer is not an error: it is "wait for more bytes".
	msg, n, err := nextFrontendMessage(ssl[:4], true)
	require.NoError(t, err)
	assert.Zero(t, n)

	msg, n, err = nextFrontendMessage(ssl, true)
	require.NoError(t, err)
	assert.Equal(t, 8, n)
	assert.Equal(t, sslRequestCode, msg.code)
	assert.Zero(t, msg.typ)
}

func TestNextFrontendMessageTypedFraming(t *testing.T) {
	buf := append(query("SELECT 1"), query("SELECT 2")...)

	msg, n, err := nextFrontendMessage(buf, false)
	require.NoError(t, err)
	assert.Equal(t, byte('Q'), msg.typ)
	assert.Equal(t, "SELECT 1", msg.sql())

	msg, _, err = nextFrontendMessage(buf[n:], false)
	require.NoError(t, err)
	assert.Equal(t, "SELECT 2", msg.sql())
}

func TestNextFrontendMessageRejectsBadLength(t *testing.T) {
	_, _, err := nextFrontendMessage([]byte{'Q', 0, 0, 0, 1}, false)
	assert.ErrorIs(t, err, errMessageTooSmall)

	_, _, err = nextFrontendMessage([]byte{'Q', 0x7f, 0xff, 0xff, 0xff}, false)
	assert.ErrorIs(t, err, errMessageTooLarge)
}

func TestParseMessageSQL(t *testing.T) {
	// Parse is: statement name, query, then parameter metadata this does not
	// need to read.
	payload := append([]byte("stmt_1\x00"), "LISTEN foo\x00"...)
	payload = append(payload, 0, 0)
	msg := frontendMessage{typ: feParse, payload: payload}
	assert.Equal(t, "LISTEN foo", msg.sql())

	assert.Empty(t, frontendMessage{typ: 'B'}.sql())
}

func TestSplitNotificationsRemovesInlineFrames(t *testing.T) {
	inline := encodeNotificationResponse(7, "chan", "hello")
	stream := append(encodeReadyForQuery('I'), inline...)
	stream = append(stream, encodeParameterStatus("TimeZone", "UTC")...)

	clean, found := splitNotifications(stream)

	// The connection that happened to run the statement must not see the
	// notification: routing it is the multiplexer's job.
	assert.Equal(t, []byte{'Z', 'S'}, messageTypes(t, clean))
	require.Len(t, found, 1)
	assert.Equal(t, notification{pid: 7, channel: "chan", payload: "hello"}, found[0])
}

func TestSplitNotificationsPassesThroughUnframedTail(t *testing.T) {
	stream := append(encodeReadyForQuery('I'), 'X', 0, 0)

	clean, found := splitNotifications(stream)

	assert.Equal(t, stream, clean)
	assert.Empty(t, found)
}

// messageTypes reduces a backend byte stream to its message type bytes, which
// is the level most assertions care about.
func messageTypes(t *testing.T, buf []byte) []byte {
	t.Helper()
	var types []byte
	for i := 0; i < len(buf); {
		typ, total, ok := nextBackendMessage(buf[i:])
		require.True(t, ok, "unframed bytes at offset %d", i)
		types = append(types, typ)
		i += total
	}
	return types
}
