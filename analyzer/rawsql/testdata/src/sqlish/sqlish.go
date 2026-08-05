// Package sqlish exercises the raw-SQL literal rule from both directions: the
// statements that must be flagged, and the prose that must not.
package sqlish

const listWorkflows = `SELECT workflow_uuid FROM dbos.workflow_status` // want "raw SQL string literal"

// Written on one source line because analysistest matches a `want` comment
// against the line the diagnostic is reported on, and the diagnostic for a
// multi-line raw string lands on its opening line — where a comment would be
// inside the literal. The escapes still exercise the leading-newline and
// leading-indent branches of the pattern.
const graphQuery = "\n    SELECT w.workflow_uuid\n    FROM GRAPH_TABLE (dbos_graph MATCH (w IS Workflow))\n" // want "raw SQL string literal"

const commented = "-- every M1 type is @readonly\nSELECT 1" // want "raw SQL string literal"

func Statements() []string {
	return []string{
		"INSERT INTO agentiq.session (id) VALUES ($1)",                     // want "raw SQL string literal"
		"UPDATE agentiq.session SET status = $1 WHERE id = $2",             // want "raw SQL string literal"
		"DELETE FROM agentiq.session WHERE id = $1",                        // want "raw SQL string literal"
		"CREATE TABLE agentiq.session (id text primary key)",               // want "raw SQL string literal"
		"CREATE PROPERTY GRAPH dbos_graph VERTEX TABLES (workflow_status)", // want "raw SQL string literal"
		"ALTER TABLE agentiq.session ADD COLUMN digest text",               // want "raw SQL string literal"
		"DROP INDEX agentiq.session_digest_idx",                            // want "raw SQL string literal"
		"WITH recent AS (SELECT 1) SELECT * FROM recent",                   // want "raw SQL string literal"
	}
}

// Prose returns strings that merely mention SQL words. None is a statement, and
// flagging any of them is how a linter gets switched off.
func Prose() []string {
	return []string{
		"failed to select the agent artifact",
		"update available",
		"insert the digest into the run input",
		"selected",
		"",
		"https://example.invalid/create/table",
		"dropped the connection",
	}
}

// Tagged has struct tags containing SQL keywords; a tag is not a statement.
type Tagged struct {
	Select string `json:"select"`
	Update string `json:"update,omitempty"`
}
