package wasmpg

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestObserveListen(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want []listenOp
	}{{
		name: "plain listen",
		sql:  "LISTEN dbos_notifications_channel",
		want: []listenOp{{listenRegister, "dbos_notifications_channel"}},
	}, {
		name: "lower case and trailing semicolon",
		sql:  "listen jobs;",
		want: []listenOp{{listenRegister, "jobs"}},
	}, {
		name: "unquoted names fold to lower case, as the backend folds them",
		sql:  "LISTEN MixedCase",
		want: []listenOp{{listenRegister, "mixedcase"}},
	}, {
		name: "quoted names keep their case",
		sql:  `LISTEN "MixedCase"`,
		want: []listenOp{{listenRegister, "MixedCase"}},
	}, {
		name: "doubled quotes inside a quoted name",
		sql:  `LISTEN "we""ird"`,
		want: []listenOp{{listenRegister, `we"ird`}},
	}, {
		name: "unlisten",
		sql:  "UNLISTEN jobs",
		want: []listenOp{{listenUnregister, "jobs"}},
	}, {
		name: "unlisten all",
		sql:  "UNLISTEN *",
		want: []listenOp{{kind: listenUnregisterAll}},
	}, {
		name: "several statements in one simple query",
		sql:  "LISTEN a; UNLISTEN b; LISTEN c",
		want: []listenOp{{listenRegister, "a"}, {listenUnregister, "b"}, {listenRegister, "c"}},
	}, {
		name: "leading line comment",
		sql:  "-- set up the queue\nLISTEN jobs",
		want: []listenOp{{listenRegister, "jobs"}},
	}, {
		name: "leading block comment",
		sql:  "/* dbos */ LISTEN jobs",
		want: []listenOp{{listenRegister, "jobs"}},
	}, {
		name: "identifier that merely starts with the keyword",
		sql:  "SELECT listener_id FROM t",
	}, {
		name: "the word inside a string literal is not a statement",
		sql:  `SELECT 'x; LISTEN spoofed'`,
	}, {
		name: "the word inside a dollar-quoted body is not a statement",
		sql:  `SELECT $tag$; LISTEN spoofed$tag$`,
	}, {
		name: "the word inside a quoted identifier is not a statement",
		sql:  `SELECT "col; LISTEN spoofed" FROM t`,
	}, {
		name: "a placeholder is not a dollar quote",
		sql:  "SELECT $1; LISTEN real",
		want: []listenOp{{listenRegister, "real"}},
	}, {
		name: "ordinary statement",
		sql:  "SELECT * FROM dbos.workflow_status",
	}, {
		name: "empty",
		sql:  "",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, observeListen(tt.sql))
		})
	}
}

func TestSplitStatements(t *testing.T) {
	assert.Equal(t, []string{"a", " b", " c"}, splitStatements("a; b; c"))
	assert.Equal(t, []string{`SELECT 'a;b'`, ` c`}, splitStatements(`SELECT 'a;b'; c`))
	assert.Equal(t, []string{`SELECT $$a;b$$`, ` c`}, splitStatements(`SELECT $$a;b$$; c`))
	assert.Equal(t, []string{"a -- ;b\n", " c"}, splitStatements("a -- ;b\n; c"))
	assert.Equal(t, []string{"a /* ;b */", " c"}, splitStatements("a /* ;b */; c"))
	// Unterminated quoting swallows the rest rather than splitting inside it.
	assert.Equal(t, []string{"SELECT 'unterminated; LISTEN x"}, splitStatements("SELECT 'unterminated; LISTEN x"))
}
