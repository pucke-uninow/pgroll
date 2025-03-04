// SPDX-License-Identifier: Apache-2.0

package migrations_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
	"github.com/xataio/pgroll/pkg/state"
	"golang.org/x/exp/maps"
	"golang.org/x/exp/slices"
)

// The version of postgres against which the tests are run
// if the POSTGRES_VERSION environment variable is not set.
const defaultPostgresVersion = "15.3"

type TestCase struct {
	name          string
	migrations    []migrations.Migration
	wantStartErr  error
	afterStart    func(t *testing.T, db *sql.DB)
	afterComplete func(t *testing.T, db *sql.DB)
	afterRollback func(t *testing.T, db *sql.DB)
}

type TestCases []TestCase

func ExecuteTests(t *testing.T, tests TestCases) {
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				ctx := context.Background()

				// run all migrations except the last one
				for i := 0; i < len(tt.migrations)-1; i++ {
					if err := mig.Start(ctx, &tt.migrations[i]); err != nil {
						t.Fatalf("Failed to start migration: %v", err)
					}

					if err := mig.Complete(ctx); err != nil {
						t.Fatalf("Failed to complete migration: %v", err)
					}
				}

				// start the last migration
				err := mig.Start(ctx, &tt.migrations[len(tt.migrations)-1])
				if tt.wantStartErr != nil {
					if !errors.Is(err, tt.wantStartErr) {
						t.Fatalf("Expected error %q, got %q", tt.wantStartErr, err)
					}
					return
				}
				if err != nil {
					t.Fatalf("Failed to start migration: %v", err)
				}

				// run the afterStart hook
				if tt.afterStart != nil {
					tt.afterStart(t, db)
				}

				// roll back the migration
				if err := mig.Rollback(ctx); err != nil {
					t.Fatalf("Failed to roll back migration: %v", err)
				}

				// run the afterRollback hook
				if tt.afterRollback != nil {
					tt.afterRollback(t, db)
				}

				// re-start the last migration
				if err := mig.Start(ctx, &tt.migrations[len(tt.migrations)-1]); err != nil {
					t.Fatalf("Failed to start migration: %v", err)
				}

				// complete the last migration
				if err := mig.Complete(ctx); err != nil {
					t.Fatalf("Failed to complete migration: %v", err)
				}

				// run the afterComplete hook
				if tt.afterComplete != nil {
					tt.afterComplete(t, db)
				}
			})
		})
	}
}

func withMigratorAndConnectionToContainer(t *testing.T, fn func(mig *roll.Roll, db *sql.DB)) {
	t.Helper()
	ctx := context.Background()

	waitForLogs := wait.
		ForLog("database system is ready to accept connections").
		WithOccurrence(2).
		WithStartupTimeout(5 * time.Second)

	pgVersion := os.Getenv("POSTGRES_VERSION")
	if pgVersion == "" {
		pgVersion = defaultPostgresVersion
	}

	ctr, err := postgres.RunContainer(ctx,
		testcontainers.WithImage("postgres:"+pgVersion),
		testcontainers.WithWaitStrategy(waitForLogs),
	)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := ctr.Terminate(ctx); err != nil {
			t.Fatalf("Failed to terminate container: %v", err)
		}
	})

	cStr, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}

	st, err := state.New(ctx, cStr, "pgroll")
	if err != nil {
		t.Fatal(err)
	}
	err = st.Init(ctx)
	if err != nil {
		t.Fatal(err)
	}

	const lockTimeoutMs = 500
	mig, err := roll.New(ctx, cStr, "public", lockTimeoutMs, st)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := mig.Close(); err != nil {
			t.Fatalf("Failed to close migrator connection: %v", err)
		}
	})

	db, err := sql.Open("postgres", cStr)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("Failed to close database connection: %v", err)
		}
	})

	fn(mig, db)
}

// Common assertions

func ViewMustExist(t *testing.T, db *sql.DB, schema, version, view string) {
	t.Helper()
	if !viewExists(t, db, schema, version, view) {
		t.Fatalf("Expected view %q to exist", view)
	}
}

func ViewMustNotExist(t *testing.T, db *sql.DB, schema, version, view string) {
	t.Helper()
	if viewExists(t, db, schema, version, view) {
		t.Fatalf("Expected view %q to not exist", view)
	}
}

func TableMustExist(t *testing.T, db *sql.DB, schema, table string) {
	t.Helper()
	if !tableExists(t, db, schema, table) {
		t.Fatalf("Expected table %q to exist", table)
	}
}

func TableMustNotExist(t *testing.T, db *sql.DB, schema, table string) {
	t.Helper()
	if tableExists(t, db, schema, table) {
		t.Fatalf("Expected table %q to not exist", table)
	}
}

func ColumnMustExist(t *testing.T, db *sql.DB, schema, table, column string) {
	t.Helper()
	if !columnExists(t, db, schema, table, column) {
		t.Fatalf("Expected column %q to exist", column)
	}
}

func ColumnMustNotExist(t *testing.T, db *sql.DB, schema, table, column string) {
	t.Helper()
	if columnExists(t, db, schema, table, column) {
		t.Fatalf("Expected column %q to not exist", column)
	}
}

func ColumnMustHaveType(t *testing.T, db *sql.DB, schema, table, column, expectedType string) {
	t.Helper()
	if !columnHasType(t, db, schema, table, column, expectedType) {
		t.Fatalf("Expected column %q to have type %q", column, expectedType)
	}
}

func TableMustHaveColumnCount(t *testing.T, db *sql.DB, schema, table string, n int) {
	t.Helper()
	if !tableMustHaveColumnCount(t, db, schema, table, n) {
		t.Fatalf("Expected table to have %d columns", n)
	}
}

func FunctionMustExist(t *testing.T, db *sql.DB, schema, function string) {
	t.Helper()
	if !functionExists(t, db, schema, function) {
		t.Fatalf("Expected function %q to exist", function)
	}
}

func FunctionMustNotExist(t *testing.T, db *sql.DB, schema, function string) {
	t.Helper()
	if functionExists(t, db, schema, function) {
		t.Fatalf("Expected function %q to not exist", function)
	}
}

func TriggerMustNotExist(t *testing.T, db *sql.DB, schema, table, trigger string) {
	t.Helper()
	if triggerExists(t, db, schema, table, trigger) {
		t.Fatalf("Expected trigger %q to not exist", trigger)
	}
}

func TriggerMustExist(t *testing.T, db *sql.DB, schema, table, trigger string) {
	t.Helper()
	if !triggerExists(t, db, schema, table, trigger) {
		t.Fatalf("Expected trigger %q to exist", trigger)
	}
}

func ConstraintMustNotExist(t *testing.T, db *sql.DB, schema, table, constraint string) {
	t.Helper()
	if constraintExists(t, db, schema, table, constraint) {
		t.Fatalf("Expected constraint %q to not exist", constraint)
	}
}

func ConstraintMustExist(t *testing.T, db *sql.DB, schema, table, constraint string) {
	t.Helper()
	if !constraintExists(t, db, schema, table, constraint) {
		t.Fatalf("Expected constraint %q to exist", constraint)
	}
}

func IndexMustExist(t *testing.T, db *sql.DB, schema, table, index string) {
	t.Helper()
	if !indexExists(t, db, schema, table, index) {
		t.Fatalf("Expected index %q to exist", index)
	}
}

func IndexMustNotExist(t *testing.T, db *sql.DB, schema, table, index string) {
	t.Helper()
	if indexExists(t, db, schema, table, index) {
		t.Fatalf("Expected index %q to not exist", index)
	}
}

func indexExists(t *testing.T, db *sql.DB, schema, table, index string) bool {
	t.Helper()

	var exists bool
	err := db.QueryRow(`
    SELECT EXISTS (
      SELECT 1
      FROM pg_indexes
      WHERE schemaname = $1
      AND tablename = $2
      AND indexname = $3
    )`,
		schema, table, index).Scan(&exists)
	if err != nil {
		t.Fatal(err)
	}

	return exists
}

func constraintExists(t *testing.T, db *sql.DB, schema, table, constraint string) bool {
	t.Helper()

	var exists bool
	err := db.QueryRow(`
    SELECT EXISTS (
      SELECT 1
      FROM pg_catalog.pg_constraint
      WHERE conrelid = $1::regclass
      AND conname = $2
    )`,
		fmt.Sprintf("%s.%s", schema, table), constraint).Scan(&exists)
	if err != nil {
		t.Fatal(err)
	}

	return exists
}

func triggerExists(t *testing.T, db *sql.DB, schema, table, trigger string) bool {
	t.Helper()

	var exists bool
	err := db.QueryRow(`
    SELECT EXISTS (
      SELECT 1
      FROM pg_catalog.pg_trigger
      WHERE tgrelid = $1::regclass
      AND tgname = $2
    )`,
		fmt.Sprintf("%s.%s", schema, table), trigger).Scan(&exists)
	if err != nil {
		t.Fatal(err)
	}

	return exists
}

func functionExists(t *testing.T, db *sql.DB, schema, functionName string) bool {
	t.Helper()

	var exists bool
	err := db.QueryRow(`
    SELECT EXISTS (
      SELECT 1
      FROM pg_catalog.pg_proc
      WHERE proname = $1
      AND pronamespace = $2::regnamespace
    )`,
		functionName, schema).Scan(&exists)
	if err != nil {
		t.Fatal(err)
	}

	return exists
}

func tableExists(t *testing.T, db *sql.DB, schema, table string) bool {
	t.Helper()

	var exists bool
	err := db.QueryRow(`
		SELECT EXISTS (
			SELECT 1
			FROM pg_catalog.pg_tables
			WHERE schemaname = $1
			AND tablename = $2
		)`,
		schema, table).Scan(&exists)
	if err != nil {
		t.Fatal(err)
	}

	return exists
}

func tableMustHaveColumnCount(t *testing.T, db *sql.DB, schema, table string, n int) bool {
	t.Helper()

	var count int
	err := db.QueryRow(`
    SELECT COUNT(*)
    FROM information_schema.columns
    WHERE table_schema = $1
    AND table_name = $2`,
		schema, table).Scan(&count)
	if err != nil {
		t.Fatal(err)
	}

	return count == n
}

func viewExists(t *testing.T, db *sql.DB, schema, version, view string) bool {
	t.Helper()
	versionSchema := roll.VersionedSchemaName(schema, version)
	var exists bool
	err := db.QueryRow(`
		SELECT EXISTS (
			SELECT 1
			FROM pg_catalog.pg_views
			WHERE schemaname = $1
			AND viewname = $2
		)`,
		versionSchema, view).Scan(&exists)
	if err != nil {
		t.Fatal(err)
	}
	return exists
}

func columnExists(t *testing.T, db *sql.DB, schema, table, column string) bool {
	t.Helper()

	var exists bool
	err := db.QueryRow(`
    SELECT EXISTS (
      SELECT 1
      FROM information_schema.columns
      WHERE table_schema = $1
      AND table_name = $2
      AND column_name = $3
    )`,
		schema, table, column).Scan(&exists)
	if err != nil {
		t.Fatal(err)
	}

	return exists
}

func columnHasType(t *testing.T, db *sql.DB, schema, table, column, expectedType string) bool {
	t.Helper()

	var actualType string
	err := db.QueryRow(`
    SELECT data_type
    FROM information_schema.columns
    WHERE table_schema = $1
    AND table_name = $2
    AND column_name = $3
  `,
		schema, table, column).Scan(&actualType)
	if err != nil {
		t.Fatal(err)
	}

	return expectedType == actualType
}

func MustInsert(t *testing.T, db *sql.DB, schema, version, table string, record map[string]string) {
	t.Helper()

	if err := insert(t, db, schema, version, table, record); err != nil {
		t.Fatal(err)
	}
}

func MustNotInsert(t *testing.T, db *sql.DB, schema, version, table string, record map[string]string) {
	t.Helper()

	if err := insert(t, db, schema, version, table, record); err == nil {
		t.Fatal("Expected INSERT to fail")
	}
}

func insert(t *testing.T, db *sql.DB, schema, version, table string, record map[string]string) error {
	t.Helper()
	versionSchema := roll.VersionedSchemaName(schema, version)

	mustSetSearchPath(t, db, versionSchema)

	cols := maps.Keys(record)
	slices.Sort(cols)

	recordStr := "("
	for i, c := range cols {
		if i > 0 {
			recordStr += ", "
		}
		recordStr += c
	}
	recordStr += ") VALUES ("
	for i, c := range cols {
		if i > 0 {
			recordStr += ", "
		}
		recordStr += fmt.Sprintf("'%s'", record[c])
	}
	recordStr += ")"

	//nolint:gosec // this is a test so we don't care about SQL injection
	stmt := fmt.Sprintf("INSERT INTO %s.%s %s", versionSchema, table, recordStr)

	_, err := db.Exec(stmt)
	return err
}

func MustSelect(t *testing.T, db *sql.DB, schema, version, table string) []map[string]any {
	t.Helper()
	versionSchema := roll.VersionedSchemaName(schema, version)

	//nolint:gosec // this is a test so we don't care about SQL injection
	selectStmt := fmt.Sprintf("SELECT * FROM %s.%s", versionSchema, table)

	q, err := db.Query(selectStmt)
	if err != nil {
		t.Fatal(err)
	}

	res := make([]map[string]any, 0)

	for q.Next() {
		cols, err := q.Columns()
		if err != nil {
			t.Fatal(err)
		}
		values := make([]any, len(cols))
		valuesPtr := make([]any, len(cols))
		for i := range values {
			valuesPtr[i] = &values[i]
		}
		if err := q.Scan(valuesPtr...); err != nil {
			t.Fatal(err)
		}

		row := map[string]any{}
		for i, col := range cols {
			// avoid having to cast int literals to int64 in tests
			if v, ok := values[i].(int64); ok {
				values[i] = int(v)
			}
			row[col] = values[i]
		}

		res = append(res, row)
	}

	return res
}

func mustSetSearchPath(t *testing.T, db *sql.DB, schema string) {
	t.Helper()

	_, err := db.Exec(fmt.Sprintf("SET search_path = %s", pq.QuoteIdentifier(schema)))
	if err != nil {
		t.Fatal(err)
	}
}

func ptr[T any](x T) *T { return &x }
