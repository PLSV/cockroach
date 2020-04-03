// Copyright 2016 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package sql_test

import (
	"context"
	gosql "database/sql"
	"database/sql/driver"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/internal/client"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/tests"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/storage/storagebase"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/jackc/pgx"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestAnonymizeStatementsForReporting(t *testing.T) {
	defer leaktest.AfterTest(t)()

	const stmt = `
INSERT INTO sensitive(super, sensible) VALUES('that', 'nobody', 'must', 'see');

select * from crdb_internal.node_runtime_info;
`

	rUnsafe := "i'm not safe"
	rSafe := log.Safe("something safe")

	safeErr := sql.AnonymizeStatementsForReporting("testing", stmt, rUnsafe)

	const (
		expMessage = "panic while testing 2 statements: INSERT INTO _(_, _) VALUES " +
			"(_, _, __more2__); SELECT * FROM _._; caused by i'm not safe"
		expSafeRedactedMessage = "?:0: panic while testing 2 statements: INSERT INTO _(_, _) VALUES " +
			"(_, _, __more2__); SELECT * FROM _._: caused by <redacted>"
		expSafeSafeMessage = "?:0: panic while testing 2 statements: INSERT INTO _(_, _) VALUES " +
			"(_, _, __more2__); SELECT * FROM _._: caused by something safe"
	)

	actMessage := safeErr.Error()
	if actMessage != expMessage {
		t.Fatalf("wanted: %s\ngot: %s", expMessage, actMessage)
	}

	actSafeRedactedMessage := log.ReportablesToSafeError(0, "", []interface{}{safeErr}).Error()
	if actSafeRedactedMessage != expSafeRedactedMessage {
		t.Fatalf("wanted: %s\ngot: %s", expSafeRedactedMessage, actSafeRedactedMessage)
	}

	safeErr = sql.AnonymizeStatementsForReporting("testing", stmt, rSafe)

	actSafeSafeMessage := log.ReportablesToSafeError(0, "", []interface{}{safeErr}).Error()
	if actSafeSafeMessage != expSafeSafeMessage {
		t.Fatalf("wanted: %s\ngot: %s", expSafeSafeMessage, actSafeSafeMessage)
	}
}

// Test that a connection closed abruptly while a SQL txn is in progress results
// in that txn being rolled back.
//
// TODO(andrei): This test terminates a client connection by calling Close() on
// a driver.Conn(), which sends a MsgTerminate. We should also have a test that
// closes the connection more abruptly than that.
func TestSessionFinishRollsBackTxn(t *testing.T) {
	defer leaktest.AfterTest(t)()
	aborter := NewTxnAborter()
	defer aborter.Close(t)
	params, _ := tests.CreateTestServerParams()
	params.Knobs.SQLExecutor = aborter.executorKnobs()
	s, mainDB, _ := serverutils.StartServer(t, params)
	defer s.Stopper().Stop(context.TODO())
	{
		pgURL, cleanup := sqlutils.PGUrl(
			t, s.ServingSQLAddr(), "TestSessionFinishRollsBackTxn", url.User(security.RootUser))
		defer cleanup()
		if err := aborter.Init(pgURL); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := mainDB.Exec(`
CREATE DATABASE t;
CREATE TABLE t.test (k INT PRIMARY KEY, v TEXT);
`); err != nil {
		t.Fatal(err)
	}

	// We're going to test the rollback of transactions left in various states
	// when the connection closes abruptly.
	// For the state CommitWait, there's no actual rollback we can test for (since
	// the kv-level transaction has already been committed). But we still
	// exercise this state to check that the server doesn't crash (which used to
	// happen - #9879).
	tests := []string{"Open", "RestartWait", "CommitWait"}
	for _, state := range tests {
		t.Run(state, func(t *testing.T) {
			// Create a low-level lib/pq connection so we can close it at will.
			pgURL, cleanupDB := sqlutils.PGUrl(
				t, s.ServingSQLAddr(), state, url.User(security.RootUser))
			defer cleanupDB()
			c, err := pq.Open(pgURL.String())
			if err != nil {
				t.Fatal(err)
			}
			connClosed := false
			defer func() {
				if connClosed {
					return
				}
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
			}()

			ctx := context.TODO()
			conn := c.(driver.ConnBeginTx)
			txn, err := conn.BeginTx(ctx, driver.TxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			tx := txn.(driver.ExecerContext)
			if _, err := tx.ExecContext(ctx, "SET TRANSACTION PRIORITY NORMAL", nil); err != nil {
				t.Fatal(err)
			}

			if state == "RestartWait" || state == "CommitWait" {
				if _, err := tx.ExecContext(ctx, "SAVEPOINT cockroach_restart", nil); err != nil {
					t.Fatal(err)
				}
			}

			insertStmt := "INSERT INTO t.public.test(k, v) VALUES (1, 'a')"
			if state == "RestartWait" {
				// To get a txn in RestartWait, we'll use an aborter.
				if err := aborter.QueueStmtForAbortion(
					insertStmt, 1 /* restartCount */, false /* willBeRetriedIbid */); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := tx.ExecContext(ctx, insertStmt, nil); err != nil {
				t.Fatal(err)
			}

			if err := aborter.VerifyAndClear(); err != nil {
				t.Fatal(err)
			}

			if state == "RestartWait" || state == "CommitWait" {
				_, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT cockroach_restart", nil)
				if state == "CommitWait" {
					if err != nil {
						t.Fatal(err)
					}
				} else if !testutils.IsError(err, "pq: restart transaction:.*") {
					t.Fatal(err)
				}
			}

			// Abruptly close the connection.
			connClosed = true
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}

			// Check that the txn we had above was rolled back. We do this by reading
			// after the preceding txn and checking that we don't get an error and
			// that we haven't been blocked by intents (we can't exactly test that we
			// haven't been blocked but we assert that the query didn't take too
			// long).
			// We do the read in an explicit txn so that automatic retries don't hide
			// any errors.
			// TODO(andrei): Figure out a better way to test for non-blocking.
			// Use a trace when the client-side tracing story gets good enough.
			// There's a bit of difficulty because the cleanup is async.
			txCheck, err := mainDB.Begin()
			if err != nil {
				t.Fatal(err)
			}
			// Run check at low priority so we don't push the previous transaction and
			// fool ourselves into thinking it had been rolled back.
			if _, err := txCheck.Exec("SET TRANSACTION PRIORITY LOW"); err != nil {
				t.Fatal(err)
			}
			ts := timeutil.Now()
			var count int
			if err := txCheck.QueryRow("SELECT count(1) FROM t.test").Scan(&count); err != nil {
				t.Fatal(err)
			}
			// CommitWait actually committed, so we'll need to clean up.
			if state != "CommitWait" {
				if count != 0 {
					t.Fatalf("expected no rows, got: %d", count)
				}
			} else {
				if _, err := txCheck.Exec("DELETE FROM t.test"); err != nil {
					t.Fatal(err)
				}
			}
			if err := txCheck.Commit(); err != nil {
				t.Fatal(err)
			}
			if d := timeutil.Since(ts); d > time.Second {
				t.Fatalf("Looks like the checking tx was unexpectedly blocked. "+
					"It took %s to commit.", d)
			}

		})
	}
}

// Test two things about non-retriable errors happening when the Executor does
// an "autoCommit" (i.e. commits the KV txn after running an implicit
// transaction):
// 1) The error is reported to the client.
// 2) The error doesn't leave the session in the Aborted state. After running
// implicit transactions, the state should always be NoTxn, regardless of any
// errors.
func TestNonRetriableErrorOnAutoCommit(t *testing.T) {
	defer leaktest.AfterTest(t)()

	query := "SELECT 42"

	params := base.TestServerArgs{
		Knobs: base.TestingKnobs{
			SQLExecutor: &sql.ExecutorTestingKnobs{
				BeforeAutoCommit: func(ctx context.Context, stmt string) error {
					if strings.Contains(stmt, query) {
						return fmt.Errorf("injected autocommit error")
					}
					return nil
				},
			},
		},
	}
	s, sqlDB, _ := serverutils.StartServer(t, params)
	defer s.Stopper().Stop(context.TODO())

	sqlDB.SetMaxOpenConns(1)

	if _, err := sqlDB.Exec(query); !testutils.IsError(err, "injected") {
		t.Fatalf("expected injected error, got: %v", err)
	}

	var state string
	if err := sqlDB.QueryRow("SHOW TRANSACTION STATUS").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "NoTxn" {
		t.Fatalf("expected state %s, got: %s", "NoTxn", state)
	}
}

// Test that, if a ROLLBACK statement encounters an error, the error is not
// returned to the client and the session state is transitioned to NoTxn.
func TestErrorOnRollback(t *testing.T) {
	defer leaktest.AfterTest(t)()

	const targetKeyString string = "/Table/53/1/1/0"
	var injectedErr int64

	// We're going to inject an error into our EndTransaction.
	params := base.TestServerArgs{
		Knobs: base.TestingKnobs{
			Store: &storage.StoreTestingKnobs{
				TestingProposalFilter: func(fArgs storagebase.ProposalFilterArgs) *roachpb.Error {
					if !fArgs.Req.IsSingleRequest() {
						return nil
					}
					req := fArgs.Req.Requests[0]
					etReq, ok := req.GetInner().(*roachpb.EndTransactionRequest)
					// We only inject the error once. Turns out that during the life of
					// the test there's two EndTransactions being sent - one is the direct
					// result of the test's call to tx.Rollback(), the second is sent by
					// the TxnCoordSender - indirectly triggered by the fact that, on the
					// server side, the transaction's context gets canceled at the SQL
					// layer.
					if ok &&
						etReq.Header().Key.String() == targetKeyString &&
						atomic.LoadInt64(&injectedErr) == 0 {

						atomic.StoreInt64(&injectedErr, 1)
						return roachpb.NewErrorf("test injected error")
					}
					return nil
				},
			},
		},
	}
	s, sqlDB, _ := serverutils.StartServer(t, params)
	ctx := context.TODO()
	defer s.Stopper().Stop(ctx)

	if _, err := sqlDB.Exec(`
CREATE DATABASE t;
CREATE TABLE t.test (k INT PRIMARY KEY, v TEXT);
`); err != nil {
		t.Fatal(err)
	}

	tx, err := sqlDB.Begin()
	if err != nil {
		t.Fatal(err)
	}

	// Perform a write so that the EndTransaction we're going to send doesn't get
	// elided.
	if _, err := tx.ExecContext(ctx, "INSERT INTO t.test(k, v) VALUES (1, 'abc')"); err != nil {
		t.Fatal(err)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	var state string
	if err := sqlDB.QueryRow("SHOW TRANSACTION STATUS").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "NoTxn" {
		t.Fatalf("expected state %s, got: %s", "NoTxn", state)
	}

	if atomic.LoadInt64(&injectedErr) == 0 {
		t.Fatal("test didn't inject the error; it must have failed to find " +
			"the EndTransaction with the expected key")
	}
}

func TestAppNameStatisticsInitialization(t *testing.T) {
	defer leaktest.AfterTest(t)()

	params, _ := tests.CreateTestServerParams()
	params.Insecure = true
	s, _, _ := serverutils.StartServer(t, params)
	defer s.Stopper().Stop(context.TODO())

	// Prepare a session with a custom application name.
	pgURL := url.URL{
		Scheme:   "postgres",
		User:     url.User(security.RootUser),
		Host:     s.ServingSQLAddr(),
		RawQuery: "sslmode=disable&application_name=mytest",
	}
	rawSQL, err := gosql.Open("postgres", pgURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rawSQL.Close()
	sqlDB := sqlutils.MakeSQLRunner(rawSQL)

	// Issue a query to be registered in stats.
	sqlDB.Exec(t, "SELECT version()")

	// Verify the query shows up in stats.
	rows := sqlDB.Query(t, "SELECT application_name, key FROM crdb_internal.node_statement_statistics")
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var appName, key string
		if err := rows.Scan(&appName, &key); err != nil {
			t.Fatal(err)
		}
		counts[appName+":"+key]++
	}
	if counts["mytest:SELECT version()"] == 0 {
		t.Fatalf("query was not counted properly: %+v", counts)
	}
}

// This test ensures that when in an explicit transaction, statement preparation
// uses the user's transaction and thus properly interacts with deadlock
// detection.
func TestPrepareInExplicitTransactionDoesNotDeadlock(t *testing.T) {
	defer leaktest.AfterTest(t)()

	s, sqlDB, _ := serverutils.StartServer(t, base.TestServerArgs{})
	defer s.Stopper().Stop(context.Background())

	testDB := sqlutils.MakeSQLRunner(sqlDB)
	testDB.Exec(t, "CREATE TABLE foo (i INT PRIMARY KEY)")
	testDB.Exec(t, "CREATE TABLE bar (i INT PRIMARY KEY)")

	tx1, err := sqlDB.Begin()
	require.NoError(t, err)

	tx2, err := sqlDB.Begin()
	require.NoError(t, err)

	// So now I really want to try to have a deadlock.

	_, err = tx1.Exec("ALTER TABLE foo ADD COLUMN j INT NOT NULL")
	require.NoError(t, err)

	_, err = tx2.Exec("ALTER TABLE bar ADD COLUMN j INT NOT NULL")
	require.NoError(t, err)

	// Now we want tx2 to get blocked on tx1 and stay blocked, then we want to
	// push tx1 above tx2 and have it get blocked in planning.
	errCh := make(chan error)
	go func() {
		_, err := tx2.Exec("ALTER TABLE foo ADD COLUMN k INT NOT NULL")
		errCh <- err
	}()
	select {
	case <-time.After(time.Millisecond):
	case err := <-errCh:
		t.Fatalf("expected the transaction to block, got %v", err)
	default:
	}

	// Read from foo so that we can push tx1 above tx2.
	testDB.Exec(t, "SELECT count(*) FROM foo")

	// Write into foo to push tx1
	_, err = tx1.Exec("INSERT INTO foo VALUES (1)")
	require.NoError(t, err)

	// Plan a query which will resolve bar during planning time, this would block
	// and deadlock if it were run on a new transaction.
	_, err = tx1.Prepare("SELECT NULL FROM [SHOW COLUMNS FROM bar] LIMIT 1")
	require.NoError(t, err)

	// Try to commit tx1. Either it should get a RETRY_SERIALIZABLE error or
	// tx2 should. Ensure that either one or both of them does.
	if tx1Err := tx1.Commit(); tx1Err == nil {
		// tx1 committed successfully, ensure tx2 failed.
		tx2ExecErr := <-errCh
		require.Regexp(t, "RETRY_SERIALIZABLE", tx2ExecErr)
		_ = tx2.Rollback()
	} else {
		require.Regexp(t, "RETRY_SERIALIZABLE", tx1Err)
		tx2ExecErr := <-errCh
		require.NoError(t, tx2ExecErr)
		if tx2CommitErr := tx2.Commit(); tx2CommitErr != nil {
			require.Regexp(t, "RETRY_SERIALIZABLE", tx2CommitErr)
		}
	}
}

// TestRetriableErrorDuringPrepare ensures that when preparing and using a new
// transaction, retriable errors are handled properly and do not propagate to
// the user's transaction.
func TestRetriableErrorDuringPrepare(t *testing.T) {
	defer leaktest.AfterTest(t)()
	const uniqueString = "'a very unique string'"
	var failed int64
	const numToFail = 2 // only fail on the first two attempts
	s, sqlDB, _ := serverutils.StartServer(t, base.TestServerArgs{
		Knobs: base.TestingKnobs{
			SQLExecutor: &sql.ExecutorTestingKnobs{
				BeforePrepare: func(ctx context.Context, stmt string, txn *client.Txn) error {
					if strings.Contains(stmt, uniqueString) && atomic.AddInt64(&failed, 1) <= numToFail {
						return roachpb.NewTransactionRetryWithProtoRefreshError("boom",
							txn.ID(), *txn.Serialize())
					}
					return nil
				},
			},
		},
	})
	defer s.Stopper().Stop(context.Background())

	testDB := sqlutils.MakeSQLRunner(sqlDB)
	testDB.Exec(t, "CREATE TABLE foo (i INT PRIMARY KEY)")

	stmt, err := sqlDB.Prepare("SELECT " + uniqueString)
	require.NoError(t, err)
	defer func() { _ = stmt.Close() }()
}

// This test ensures that when in an explicit transaction and statement
// preparation uses the user's transaction, errors during those planning queries
// are handled correctly.
func TestErrorDuringPrepareInExplicitTransactionPropagates(t *testing.T) {
	defer leaktest.AfterTest(t)()

	filter := newDynamicRequestFilter()
	s, sqlDB, _ := serverutils.StartServer(t, base.TestServerArgs{
		Knobs: base.TestingKnobs{
			Store: &storage.StoreTestingKnobs{
				TestingRequestFilter: storagebase.ReplicaRequestFilter(filter.filter),
			},
		},
	})
	defer s.Stopper().Stop(context.Background())

	testDB := sqlutils.MakeSQLRunner(sqlDB)
	testDB.Exec(t, "CREATE TABLE foo (i INT PRIMARY KEY)")
	testDB.Exec(t, "CREATE TABLE bar (i INT PRIMARY KEY)")

	// This test will create an explicit transaction that encounters an error on
	// a latter statement during planning of SHOW COLUMNS. The planning for this
	// SHOW COLUMNS will be run in the user's transaction. The test will inject
	// errors into the execution of that planning query and ensure that the user's
	// transaction state evolves appropriately.

	// Use pgx so that we can introspect error codes returned from cockroach.
	pgURL, cleanup := sqlutils.PGUrl(t, s.ServingSQLAddr(), "", url.User("root"))
	defer cleanup()
	conf, err := pgx.ParseConnectionString(pgURL.String())
	require.NoError(t, err)
	conn, err := pgx.Connect(conf)
	require.NoError(t, err)

	tx, err := conn.Begin()
	require.NoError(t, err)

	_, err = tx.Exec("SAVEPOINT cockroach_restart")
	require.NoError(t, err)

	// Do something with the user's transaction so that we'll use the user
	// transaction in the planning of the below `SHOW COLUMNS`.
	_, err = tx.Exec("INSERT INTO foo VALUES (1)")
	require.NoError(t, err)

	// Inject an error that will happen during planning.
	filter.setFilter(func(ba roachpb.BatchRequest) *roachpb.Error {
		if ba.Txn == nil {
			return nil
		}
		if req, ok := ba.GetArg(roachpb.Get); ok {
			get := req.(*roachpb.GetRequest)
			_, tableID, err := keys.DecodeTablePrefix(get.Key)
			if err != nil || tableID != keys.NamespaceTableID {
				err = nil
				return nil
			}
			return roachpb.NewError(roachpb.NewReadWithinUncertaintyIntervalError(
				ba.Txn.Timestamp, ba.Txn.Timestamp.Next(), ba.Txn))
		}
		return nil
	})

	// Plan a query will get a restart error during planning.
	_, err = tx.Prepare("show_columns", "SELECT NULL FROM [SHOW COLUMNS FROM bar] LIMIT 1")
	require.Regexp(t,
		"restart transaction: TransactionRetryWithProtoRefreshError: ReadWithinUncertaintyInterval", err)
	pgErr, ok := err.(pgx.PgError)
	require.True(t, ok)
	require.Equal(t, pgcode.SerializationFailure, pgErr.Code)

	// Clear the error producing filter, restart the transaction, and run it to
	// completion.
	filter.setFilter(nil)

	_, err = tx.Exec("ROLLBACK TO SAVEPOINT cockroach_restart")
	require.NoError(t, err)

	_, err = tx.Exec("INSERT INTO foo VALUES (1)")
	require.NoError(t, err)
	_, err = tx.Prepare("show_columns", "SELECT NULL FROM [SHOW COLUMNS FROM bar] LIMIT 1")
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
}

// dynamicRequestFilter exposes a filter method which is a
// storagebase.ReplicaRequestFilter but can be set dynamically.
type dynamicRequestFilter struct {
	v atomic.Value
}

func newDynamicRequestFilter() *dynamicRequestFilter {
	f := &dynamicRequestFilter{}
	f.v.Store(storagebase.ReplicaRequestFilter(noopRequestFilter))
	return f
}

func (f *dynamicRequestFilter) setFilter(filter storagebase.ReplicaRequestFilter) {
	if filter == nil {
		f.v.Store(storagebase.ReplicaRequestFilter(noopRequestFilter))
	} else {
		f.v.Store(filter)
	}
}

// noopRequestFilter is a storagebase.ReplicaRequestFilter.
func (f *dynamicRequestFilter) filter(request roachpb.BatchRequest) *roachpb.Error {
	return f.v.Load().(storagebase.ReplicaRequestFilter)(request)
}

// noopRequestFilter is a storagebase.ReplicaRequestFilter that does nothing.
func noopRequestFilter(request roachpb.BatchRequest) *roachpb.Error {
	return nil
}
