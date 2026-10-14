package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// DDL is not transactional. Retry verifies each already materialized object
// before continuing; the version marker is written only after complete proof.
// Reject duplicate bearer hashes before schema7 writes; migration must not turn an ambiguous existing registry into a unique current identity.
func studioMigrationPreflight(ctx context.Context, c *sql.Conn) error {
	var n int
	if e := c.QueryRowContext(ctx, "SELECT COUNT(*) FROM (SELECT token_sha256 FROM operator_credentials GROUP BY token_sha256 HAVING COUNT(*)>1) duplicate_bearers").Scan(&n); e != nil {
		return e
	}
	if n != 0 {
		return errors.New("duplicate registry bearer hashes; migration refused before schema7 writes")
	}
	return nil
}

var studioCreate = regexp.MustCompile(`(?s)^CREATE TABLE IF NOT EXISTS ([a-z_]+) \((.*)\) ENGINE=InnoDB$`)
var studioColumn = regexp.MustCompile(`^([a-z_]+) ((?:VARBINARY|BINARY|VARCHAR|DATETIME)\([0-9]+\)|BIGINT UNSIGNED|TINYINT|JSON) (NOT NULL|NULL)$`)
var studioCheck = regexp.MustCompile(`^CONSTRAINT ([a-z_]+) CHECK (.*)$`)

func studioNormalizeCheck(v string) string {
	return strings.NewReplacer("`", "", " ", "", "\n", "", "\t", "", "_utf8mb4", "", "\\'", "'").Replace(strings.ToLower(v))
}

// Accept only the recognized schema7 statements. Reusing materialized objects requires their declared shape to match; CREATE IF NOT EXISTS alone is not compatibility proof.
func migrateStudioStatement(ctx context.Context, c *sql.Conn, statement string) error {
	statement = strings.TrimSpace(statement)
	if m := studioCreate.FindStringSubmatch(statement); m != nil {
		if _, e := c.ExecContext(ctx, statement); e != nil {
			return e
		}
		return verifyStudioTable(ctx, c, m[1], m[2])
	}
	if strings.HasPrefix(statement, "ALTER TABLE operator_credentials ADD UNIQUE") {
		var n int
		if e := c.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='operator_credentials' AND INDEX_NAME='studio_unique_bearer'").Scan(&n); e != nil {
			return e
		}
		if n == 0 {
			if _, e := c.ExecContext(ctx, statement); e != nil {
				return e
			}
		}
		var col string
		var unique, seq int
		if e := c.QueryRowContext(ctx, "SELECT COLUMN_NAME,NON_UNIQUE,SEQ_IN_INDEX FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='operator_credentials' AND INDEX_NAME='studio_unique_bearer'").Scan(&col, &unique, &seq); e != nil {
			return e
		}
		if n > 1 || col != "token_sha256" || unique != 0 || seq != 1 {
			return errors.New("registry bearer index differs")
		}
		return nil
	}
	for _, col := range []struct{ name, typ string }{{"issuer_credential_id", "varbinary(64)"}, {"issuer_provenance", "varchar(24)"}} {
		if strings.HasPrefix(statement, "ALTER TABLE registered_jobs ADD COLUMN "+col.name+" ") {
			var typ, nullable string
			e := c.QueryRowContext(ctx, "SELECT COLUMN_TYPE,IS_NULLABLE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='registered_jobs' AND COLUMN_NAME=?", col.name).Scan(&typ, &nullable)
			if errors.Is(e, sql.ErrNoRows) {
				_, e = c.ExecContext(ctx, statement)
				return e
			}
			if e != nil {
				return e
			}
			if typ != col.typ || nullable != "YES" {
				return errors.New("registered issuer column differs")
			}
			return nil
		}
	}
	return errors.New("unrecognized schema7 statement")
}

func verifyStudioTable(ctx context.Context, c *sql.Conn, table, body string) error {
	type column struct{ name, typ, nullable string }
	var want []column
	checks := map[string]string{}
	var primary string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSuffix(strings.TrimSpace(line), ",")
		if line == "" {
			continue
		}
		if m := studioColumn.FindStringSubmatch(line); m != nil {
			nullable := "YES"
			if m[3] == "NOT NULL" {
				nullable = "NO"
			}
			want = append(want, column{m[1], strings.ToLower(m[2]), nullable})
			continue
		}
		if m := studioCheck.FindStringSubmatch(line); m != nil {
			checks[m[1]] = studioNormalizeCheck(m[2])
			continue
		}
		if strings.HasPrefix(line, "PRIMARY KEY (") {
			primary = strings.TrimSuffix(strings.TrimPrefix(line, "PRIMARY KEY ("), ")")
			continue
		}
		return fmt.Errorf("unknown schema7 declaration in %s", table)
	}
	rows, e := c.QueryContext(ctx, "SELECT COLUMN_NAME,COLUMN_TYPE,IS_NULLABLE,EXTRA,COLUMN_DEFAULT FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? ORDER BY ORDINAL_POSITION", table)
	if e != nil {
		return e
	}
	i := 0
	for rows.Next() {
		var got column
		var extra string
		var def sql.NullString
		if e = rows.Scan(&got.name, &got.typ, &got.nullable, &extra, &def); e != nil {
			rows.Close()
			return e
		}
		if i >= len(want) || got != want[i] || extra != "" || def.Valid {
			rows.Close()
			return fmt.Errorf("schema7 column shape differs: %s.%s", table, got.name)
		}
		i++
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	if i != len(want) {
		return errors.New("schema7 columns incomplete")
	}
	var engine string
	if e = c.QueryRowContext(ctx, "SELECT ENGINE FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=?", table).Scan(&engine); e != nil {
		return e
	}
	if engine != "InnoDB" {
		return errors.New("schema7 engine differs")
	}
	rows, e = c.QueryContext(ctx, "SELECT INDEX_NAME,COLUMN_NAME,NON_UNIQUE FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? ORDER BY INDEX_NAME,SEQ_IN_INDEX", table)
	if e != nil {
		return e
	}
	var keys []string
	for rows.Next() {
		var index, col string
		var nonunique int
		if e = rows.Scan(&index, &col, &nonunique); e != nil {
			rows.Close()
			return e
		}
		if index != "PRIMARY" || nonunique != 0 {
			rows.Close()
			return errors.New("schema7 index differs")
		}
		keys = append(keys, col)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	if strings.Join(keys, ",") != primary {
		return errors.New("schema7 primary key differs")
	}
	rows, e = c.QueryContext(ctx, "SELECT t.CONSTRAINT_NAME,c.CHECK_CLAUSE,t.ENFORCED FROM information_schema.TABLE_CONSTRAINTS t JOIN information_schema.CHECK_CONSTRAINTS c ON c.CONSTRAINT_SCHEMA=t.CONSTRAINT_SCHEMA AND c.CONSTRAINT_NAME=t.CONSTRAINT_NAME WHERE t.TABLE_SCHEMA=DATABASE() AND t.TABLE_NAME=? AND t.CONSTRAINT_TYPE='CHECK'", table)
	if e != nil {
		return e
	}
	count := 0
	for rows.Next() {
		var name, clause, enforced string
		if e = rows.Scan(&name, &clause, &enforced); e != nil {
			rows.Close()
			return e
		}
		if checks[name] != studioNormalizeCheck(clause) || enforced != "YES" {
			rows.Close()
			return fmt.Errorf("schema7 check differs: %s: %s", name, clause)
		}
		count++
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	if count != len(checks) {
		return errors.New("schema7 check set differs")
	}
	return nil
}
