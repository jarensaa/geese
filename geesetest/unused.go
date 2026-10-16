package geesetest

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// DropUnused drops the tables and columns a flight plan declares unused
// (geese.FlightPlan.Unused) from a test database, so the application's tests
// prove its code never touches them: a template that migrates, then drops,
// runs them on the schema a migration that drops them leaves. A name is a
// table if one is called that, and otherwise a table's column.
func DropUnused(ctx context.Context, conn *sql.DB, unused []string) error {
	for _, name := range unused {
		parts := strings.Split(name, ".")
		var table sql.NullString
		if err := conn.QueryRowContext(ctx, `SELECT to_regclass($1)::text`, pgx.Identifier(parts).Sanitize()).Scan(&table); err != nil {
			return err
		}
		statement := `DROP TABLE ` + pgx.Identifier(parts).Sanitize()
		if !table.Valid {
			if len(parts) < 2 {
				return fmt.Errorf("geesetest: unused %s is neither a table nor a column", name)
			}
			statement = `ALTER TABLE ` + pgx.Identifier(parts[:len(parts)-1]).Sanitize() + ` DROP COLUMN ` + pgx.Identifier(parts[len(parts)-1:]).Sanitize()
		}
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("geesetest: drop unused %s: %w", name, err)
		}
	}
	return nil
}
