package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"loreviva.local/loreviva/migrations"
)

var integrationUnique = regexp.MustCompile(`^UNIQUE KEY ([a-z_]+) \(([a-z_,]+)\)$`)
var integrationAtomicCheck = regexp.MustCompile(`\(([a-z_]+(?:='[^']*'|isnull|isnotnull))\)`)

func verifyIntegrationOperatorHashes(ctx context.Context, c *sql.Conn) error {
	var missing int
	e := c.QueryRowContext(ctx, "SELECT COUNT(*) FROM operator_credentials o LEFT JOIN integration_credential_hashes h ON h.token_hash=o.token_sha256 AND h.kind='OPERATOR' AND h.principal_id=o.credential_id LEFT JOIN memory_installation i ON i.singleton=1 AND i.installation_id=h.installation_id WHERE h.token_hash IS NULL OR i.installation_id IS NULL").Scan(&missing)
	if e != nil {
		return e
	}
	if missing != 0 {
		return errors.New("schema8 completed operator hash binding missing or inconsistent")
	}
	return nil
}

// Normalize only redundant atom/outer parentheses. Boolean grouping remains
// significant: a drifted OR/AND constraint must never match by punctuation loss.
func integrationCheck(v string) string {
	v = studioNormalizeCheck(v)
	for {
		next := integrationAtomicCheck.ReplaceAllString(v, "$1")
		if next == v {
			break
		}
		v = next
	}
	for strings.HasPrefix(v, "(") && strings.HasSuffix(v, ")") {
		depth := 0
		whole := true
		for i, c := range v {
			if c == '(' {
				depth++
			}
			if c == ')' {
				depth--
			}
			if depth == 0 && i < len(v)-1 {
				whole = false
				break
			}
		}
		if !whole {
			break
		}
		v = v[1 : len(v)-1]
	}
	return v
}

func migrateIntegrationStatement(ctx context.Context, c *sql.Conn, statement string) error {
	statement = strings.TrimSpace(statement)
	m := studioCreate.FindStringSubmatch(statement)
	if m == nil {
		return errors.New("unrecognized schema8 statement")
	}
	// Existing partial DDL must be verified before any later object is created.
	var exists int
	if e := c.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=?", m[1]).Scan(&exists); e != nil {
		return e
	}
	if exists == 0 {
		if _, e := c.ExecContext(ctx, statement); e != nil {
			return e
		}
	}
	return verifyIntegrationTable(ctx, c, m[1], m[2])
}

func verifyIntegrationTable(ctx context.Context, c *sql.Conn, table, body string) error {
	type column struct{ name, typ, nullable string }
	cols := []column{}
	indexes := map[string][]string{}
	checks := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSuffix(strings.TrimSpace(line), ",")
		if line == "" {
			continue
		}
		if m := studioColumn.FindStringSubmatch(line); m != nil {
			null := "NO"
			if m[3] == "NULL" {
				null = "YES"
			}
			cols = append(cols, column{m[1], strings.ToLower(m[2]), null})
			continue
		}
		if strings.HasPrefix(line, "PRIMARY KEY (") {
			indexes["PRIMARY"] = strings.Split(strings.TrimSuffix(strings.TrimPrefix(line, "PRIMARY KEY ("), ")"), ",")
			continue
		}
		if m := integrationUnique.FindStringSubmatch(line); m != nil {
			indexes[m[1]] = strings.Split(m[2], ",")
			continue
		}
		if m := studioCheck.FindStringSubmatch(line); m != nil {
			checks[m[1]] = integrationCheck(m[2])
			continue
		}
		return fmt.Errorf("unknown schema8 declaration: %s", table)
	}
	// Schema8 declares no foreign keys. A foreign key can reuse an existing
	// index, so matching columns and indexes alone cannot establish equivalence.
	var foreignKeys int
	if e := c.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.TABLE_CONSTRAINTS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? AND CONSTRAINT_TYPE='FOREIGN KEY'", table).Scan(&foreignKeys); e != nil {
		return e
	}
	if foreignKeys != 0 {
		return fmt.Errorf("schema8 foreign key set differs: %s", table)
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
		if i >= len(cols) || got != cols[i] || extra != "" || def.Valid {
			rows.Close()
			return fmt.Errorf("schema8 column differs: %s.%s", table, got.name)
		}
		i++
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	if i != len(cols) {
		return errors.New("schema8 column set differs")
	}
	var engine string
	if e = c.QueryRowContext(ctx, "SELECT ENGINE FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=?", table).Scan(&engine); e != nil {
		return e
	}
	if engine != "InnoDB" {
		return errors.New("schema8 engine differs")
	}
	rows, e = c.QueryContext(ctx, "SELECT INDEX_NAME,COLUMN_NAME,NON_UNIQUE,SUB_PART,INDEX_TYPE,IS_VISIBLE FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? ORDER BY INDEX_NAME,SEQ_IN_INDEX", table)
	if e != nil {
		return e
	}
	got := map[string][]string{}
	for rows.Next() {
		var name, col, kind, visible string
		var nonunique int
		var sub sql.NullInt64
		if e = rows.Scan(&name, &col, &nonunique, &sub, &kind, &visible); e != nil {
			rows.Close()
			return e
		}
		if nonunique != 0 || sub.Valid || kind != "BTREE" || visible != "YES" {
			rows.Close()
			return errors.New("schema8 index attributes differ")
		}
		got[name] = append(got[name], col)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	if len(got) != len(indexes) {
		return errors.New("schema8 index set differs")
	}
	for name, cols := range indexes {
		if strings.Join(got[name], ",") != strings.Join(cols, ",") {
			return fmt.Errorf("schema8 index differs: %s.%s", table, name)
		}
	}
	rows, e = c.QueryContext(ctx, "SELECT t.CONSTRAINT_NAME,c.CHECK_CLAUSE,t.ENFORCED FROM information_schema.TABLE_CONSTRAINTS t JOIN information_schema.CHECK_CONSTRAINTS c ON c.CONSTRAINT_SCHEMA=t.CONSTRAINT_SCHEMA AND c.CONSTRAINT_NAME=t.CONSTRAINT_NAME WHERE t.TABLE_SCHEMA=DATABASE() AND t.TABLE_NAME=? AND t.CONSTRAINT_TYPE='CHECK'", table)
	if e != nil {
		return e
	}
	n := 0
	for rows.Next() {
		var name, clause, enforced string
		if e = rows.Scan(&name, &clause, &enforced); e != nil {
			rows.Close()
			return e
		}
		if checks[name] != integrationCheck(clause) || enforced != "YES" {
			rows.Close()
			return fmt.Errorf("schema8 check differs: %s.%s", table, name)
		}
		n++
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	if n != len(checks) {
		return errors.New("schema8 check set differs")
	}
	return nil
}

func verifyIntegrationMigration(ctx context.Context, c *sql.Conn) error {
	n := 0
	for _, statement := range strings.Split(migrations.DeliveryIdentity, ";") {
		if strings.TrimSpace(statement) == "" {
			continue
		}
		m := studioCreate.FindStringSubmatch(strings.TrimSpace(statement))
		if m == nil {
			return errors.New("invalid schema8 source")
		}
		if e := verifyIntegrationTable(ctx, c, m[1], m[2]); e != nil {
			return e
		}
		n++
	}
	if n != 17 {
		return errors.New("schema8 requires exactly seventeen tables")
	}
	return nil
}

func finalizeIntegrationMigration(ctx context.Context, c *sql.Conn, sum [32]byte) error {
	if e := verifyIntegrationMigration(ctx, c); e != nil {
		return e
	}
	tx, e := c.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var count int
	if e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM operator_credentials").Scan(&count); e != nil {
		return e
	}
	if count > 0 {
		var installation string
		if e = tx.QueryRowContext(ctx, "SELECT installation_id FROM memory_installation WHERE singleton=1").Scan(&installation); e != nil || installation == "" {
			return errors.New("schema8 legacy credentials lack installation binding")
		}
		rows, e := tx.QueryContext(ctx, "SELECT credential_id,token_sha256 FROM operator_credentials ORDER BY token_sha256")
		if e != nil {
			return e
		}
		type item struct {
			id   string
			hash []byte
		}
		items := []item{}
		for rows.Next() {
			var x item
			if e = rows.Scan(&x.id, &x.hash); e != nil {
				rows.Close()
				return e
			}
			if len(x.hash) != sha256.Size {
				rows.Close()
				return errors.New("schema8 invalid legacy hash")
			}
			items = append(items, x)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		for _, x := range items {
			var kind, id, install string
			e = tx.QueryRowContext(ctx, "SELECT kind,principal_id,installation_id FROM integration_credential_hashes WHERE token_hash=?", x.hash).Scan(&kind, &id, &install)
			if e == nil {
				if kind != "OPERATOR" || id != x.id || install != installation {
					return errors.New("schema8 bearer identity collision")
				}
				continue
			}
			if !errors.Is(e, sql.ErrNoRows) {
				return e
			}
			if _, e = tx.ExecContext(ctx, "INSERT INTO integration_credential_hashes(token_hash,kind,principal_id,installation_id,created_at) VALUES(?,'OPERATOR',?,?,UTC_TIMESTAMP(6))", x.hash, x.id, installation); e != nil {
				return e
			}
		}
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO schema_migrations(version,sha256) VALUES(8,?)", sum[:]); e != nil {
		return e
	}
	return tx.Commit()
}
