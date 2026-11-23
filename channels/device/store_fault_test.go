package device

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.bison9.com/agency/internal/tokenhash"
	"go.bison9.com/agency/utils"
)

// failOn installs a trigger that aborts every op (INSERT, UPDATE or DELETE)
// on table with a recognisable message, so a test can fail one statement of a
// store transaction.
func failOn(t *testing.T, s *Store, op, table string) {
	t.Helper()
	name := "fail_" + strings.ToLower(op) + "_" + table
	if _, err := s.db.ExecContext(context.Background(),
		`CREATE TRIGGER `+name+` BEFORE `+op+` ON `+table+` BEGIN SELECT RAISE(ABORT, 'injected `+name+`'); END`); err != nil {
		t.Fatalf("create trigger %s: %v", name, err)
	}
}

// wantInjected fails the test unless err is the failure failOn injected.
func wantInjected(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("%s: err = %v, want the injected failure", what, err)
	}
}

// closedStore is an opened then closed store: every query fails.
func closedStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(context.Background(), filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return s
}

// Every store operation reports a database that cannot be reached instead of
// answering as if the device or request did not exist.
func TestStore_ClosedDatabaseErrors(t *testing.T) {
	ctx := context.Background()
	s := closedStore(t)
	isClosed := func(what string, err error) {
		t.Helper()
		if err == nil || errors.Is(err, ErrPairedNotFound) || errors.Is(err, ErrPendingNotFound) {
			t.Errorf("%s on a closed database: err = %v, want the database error", what, err)
		}
	}
	_, err := s.CreatePending(ctx, PendingPairing{DeviceID: "d", PublicKey: "pk"})
	isClosed("CreatePending", err)
	_, err = s.ListPending(ctx)
	isClosed("ListPending", err)
	_, _, err = s.Approve(ctx, "req", nil, nil)
	isClosed("Approve", err)
	isClosed("Reject", s.Reject(ctx, "req"))
	_, err = s.ListPaired(ctx)
	isClosed("ListPaired", err)
	isClosed("SetDeviceAgent", s.SetDeviceAgent(ctx, "d", "alice"))
	isClosed("SetDeviceAllowedAgents", s.SetDeviceAllowedAgents(ctx, "d", []string{"alice"}))
	isClosed("SetDeviceUser", s.SetDeviceUser(ctx, "d", "Alice"))
	_, _, err = s.TokenByValue(ctx, "tok")
	isClosed("TokenByValue", err)
	_, err = s.IssueTokens(ctx, "d", nil, nil)
	isClosed("IssueTokens", err)
	isClosed("RemovePaired", s.RemovePaired(ctx, "d"))
}

// A failing statement inside CreatePending rolls it back: the device's
// earlier request is kept as it was.
func TestCreatePending_StatementFailures(t *testing.T) {
	for _, op := range []string{"DELETE", "INSERT"} {
		t.Run(op, func(t *testing.T) {
			s := openTestStore(t)
			ctx := context.Background()
			reqID, err := s.CreatePending(ctx, PendingPairing{DeviceID: "d", PublicKey: "pk", DisplayName: "first"})
			if err != nil {
				t.Fatal(err)
			}
			failOn(t, s, op, "pending_pairings")
			_, err = s.CreatePending(ctx, PendingPairing{DeviceID: "d", PublicKey: "pk", DisplayName: "second"})
			wantInjected(t, "CreatePending", err)
			pend, err := s.ListPending(ctx)
			if err != nil || len(pend) != 1 || pend[0].RequestID != reqID || pend[0].DisplayName != "first" {
				t.Fatalf("ListPending after a failed create = %+v, %v; want the first request kept", pend, err)
			}
		})
	}
}

// The expiry sweep runs inside the same transaction: when it fails, the new
// request is rolled back too.
func TestCreatePending_SweepFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	// Fails only the sweep's deletes (the insert's own DELETE by device id
	// matches no row, so the trigger never fires for it).
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_sweep BEFORE DELETE ON pending_pairings
		WHEN OLD.device_id = 'old' BEGIN SELECT RAISE(ABORT, 'injected sweep'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO pending_pairings (request_id, device_id, public_key, created_at_ms)
		VALUES ('r-old', 'old', 'pk', 1)`); err != nil {
		t.Fatal(err)
	}
	_, err := s.CreatePending(ctx, PendingPairing{DeviceID: "new", PublicKey: "pk"})
	wantInjected(t, "CreatePending", err)
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM pending_pairings WHERE device_id='new'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("new request rows = %d (%v), want 0 after rollback", n, err)
	}
}

// A failing statement inside Approve leaves the request pending and the
// device unpaired.
func TestApprove_StatementFailures(t *testing.T) {
	for _, tc := range []struct{ op, table string }{
		{"INSERT", "paired_devices"},
		{"INSERT", "device_tokens"},
		{"DELETE", "pending_pairings"},
	} {
		t.Run(tc.op+"_"+tc.table, func(t *testing.T) {
			s := openTestStore(t)
			ctx := context.Background()
			reqID, err := s.CreatePending(ctx, PendingPairing{DeviceID: "d", PublicKey: "pk", Role: "node"})
			if err != nil {
				t.Fatal(err)
			}
			failOn(t, s, tc.op, tc.table)
			_, _, err = s.Approve(ctx, reqID, nil, nil)
			wantInjected(t, "Approve", err)
			if _, ok, err := s.GetPaired(ctx, "d"); err != nil || ok {
				t.Fatalf("device paired after a failed approve (ok=%v err=%v)", ok, err)
			}
			if pend, err := s.ListPending(ctx); err != nil || len(pend) != 1 {
				t.Fatalf("pending after a failed approve = %+v, %v; want the request kept", pend, err)
			}
		})
	}
}

// A pending request stored without a role is approved with the node role.
func TestApprove_EmptyRoleDefaultsToNode(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	reqID, err := s.CreatePending(ctx, PendingPairing{DeviceID: "d", PublicKey: "pk"})
	if err != nil {
		t.Fatal(err)
	}
	dev, toks, err := s.Approve(ctx, reqID, nil, nil)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if len(dev.Roles) != 1 || dev.Roles[0] != "node" || len(toks) != 1 || toks[0].Role != "node" {
		t.Fatalf("roles = %v tokens = %+v, want one node role and token", dev.Roles, toks)
	}
}

// IssueTokens and RemovePaired roll back on a failing statement: the device
// keeps its tokens and its record.
func TestTokenAndRemoveStatementFailures(t *testing.T) {
	cases := []struct {
		name, op, table string
		run             func(context.Context, *Store) error
	}{
		{"IssueTokens delete", "DELETE", "device_tokens", func(ctx context.Context, s *Store) error {
			_, err := s.IssueTokens(ctx, "dev1", []string{"node"}, nil)
			return err
		}},
		{"IssueTokens insert", "INSERT", "device_tokens", func(ctx context.Context, s *Store) error {
			_, err := s.IssueTokens(ctx, "dev1", []string{"node"}, nil)
			return err
		}},
		{"RemovePaired tokens", "DELETE", "device_tokens", func(ctx context.Context, s *Store) error { return s.RemovePaired(ctx, "dev1") }},
		{"RemovePaired device", "DELETE", "paired_devices", func(ctx context.Context, s *Store) error { return s.RemovePaired(ctx, "dev1") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStore(t)
			pairStoreDevice(t, s, "dev1")
			before := rawTokens(t, s)
			failOn(t, s, tc.op, tc.table)
			ctx := context.Background()
			wantInjected(t, tc.name, tc.run(ctx, s))
			if _, ok, err := s.GetPaired(ctx, "dev1"); err != nil || !ok {
				t.Fatalf("device gone after a failed %s (ok=%v err=%v)", tc.name, ok, err)
			}
			if after := rawTokens(t, s); len(after) != len(before) || after[0] != before[0] {
				t.Fatalf("tokens changed after a failed %s: %v -> %v", tc.name, before, after)
			}
		})
	}
}

// Updating a device that is not paired reports ErrPairedNotFound.
func TestSetDeviceAgent_UnknownDevice(t *testing.T) {
	s := openTestStore(t)
	if err := s.SetDeviceAgent(context.Background(), "nope", "alice"); !errors.Is(err, ErrPairedNotFound) {
		t.Fatalf("SetDeviceAgent(unknown) = %v, want ErrPairedNotFound", err)
	}
}

// A revoked token no longer authenticates.
func TestTokenByValue_Revoked(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	toks, err := s.IssueTokens(ctx, "dev1", []string{"node"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE device_tokens SET revoked_at_ms=1`); err != nil {
		t.Fatal(err)
	}
	if dt, ok, err := s.TokenByValue(ctx, toks[0].Token); err != nil || ok || dt != nil {
		t.Fatalf("TokenByValue(revoked) = %+v ok=%v err=%v, want not found", dt, ok, err)
	}
}

// A row whose stored values cannot be read is an error, not a silently
// skipped or zeroed row.
func TestStore_UnreadableRows(t *testing.T) {
	t.Run("pending", func(t *testing.T) {
		s := openTestStore(t)
		ctx := context.Background()
		// A created_at_ms that is not a number passes the expiry filter
		// (text sorts after integers) and fails the scan.
		if _, err := s.db.ExecContext(ctx, `INSERT INTO pending_pairings (request_id, device_id, public_key, created_at_ms)
			VALUES ('r1', 'd', 'pk', 'not-a-number')`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ListPending(ctx); err == nil {
			t.Error("ListPending with an unreadable row: want an error")
		}
		if _, _, err := s.Approve(ctx, "r1", nil, nil); err == nil || errors.Is(err, ErrPendingNotFound) {
			t.Errorf("Approve of an unreadable request = %v, want the read error", err)
		}
	})

	t.Run("paired", func(t *testing.T) {
		s := openTestStore(t)
		pairStoreDevice(t, s, "dev1")
		ctx := context.Background()
		if _, err := s.db.ExecContext(ctx, `UPDATE paired_devices SET created_at_ms='not-a-number'`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ListPaired(ctx); err == nil {
			t.Error("ListPaired with an unreadable row: want an error")
		}
	})

	t.Run("token", func(t *testing.T) {
		s := openTestStore(t)
		ctx := context.Background()
		toks, err := s.IssueTokens(ctx, "dev1", []string{"node"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE device_tokens SET created_at_ms='not-a-number'`); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := s.TokenByValue(ctx, toks[0].Token); err == nil || ok {
			t.Errorf("TokenByValue of an unreadable row: ok=%v err=%v, want an error", ok, err)
		}
	})
}

// A stored string list that is not JSON reads as empty instead of failing
// the whole row.
func TestUnmarshalStrings_InvalidJSON(t *testing.T) {
	if got := unmarshalStrings("not json"); len(got) != 0 {
		t.Fatalf("unmarshalStrings(invalid) = %v, want empty", got)
	}
}

// OpenStore refuses a path it cannot create, a file that is not a database,
// and a database whose tables cannot take the schema or its migrations.
func TestOpenStore_Failures(t *testing.T) {
	ctx := context.Background()

	t.Run("parent is a file", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if s, err := OpenStore(ctx, filepath.Join(parent, "gateway.db")); err == nil {
			utilsClose(t, s)
			t.Fatal("OpenStore under a regular file: want an error")
		}
	})

	t.Run("not a database", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "gateway.db")
		if err := os.WriteFile(path, []byte(strings.Repeat("this is not a sqlite database ", 20)), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := OpenStore(ctx, path)
		if err == nil || !strings.Contains(err.Error(), "journal_mode") {
			t.Fatalf("OpenStore(not a database) = %v, want the journal_mode read error", err)
		}
	})

	for _, tc := range []struct {
		name, setup, want string
	}{
		// A view where a table is expected: the schema cannot index it.
		{"schema", `CREATE VIEW device_tokens AS SELECT 1 AS token`, "schema"},
		// A paired_devices view cannot take the agent_id column.
		{"migrate agent_id", `CREATE VIEW paired_devices AS SELECT 1 AS device_id`, "migrate agent_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "gateway.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.ExecContext(ctx, tc.setup); err != nil {
				t.Fatalf("setup: %v", err)
			}
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			s, err := OpenStore(ctx, path)
			if err == nil {
				utilsClose(t, s)
				t.Fatalf("OpenStore: want a %s error", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("OpenStore = %v, want a %s error", err, tc.want)
			}
		})
	}
}

// A plaintext token that cannot be rewritten as its hash fails the open,
// and the token is left as it was.
func TestOpenStore_TokenMigrationFails(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gateway.db")
	s, err := OpenStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `INSERT INTO device_tokens (token, device_id, role, created_at_ms) VALUES ('plain', 'd', 'node', 1)`); err != nil {
		t.Fatal(err)
	}
	failOn(t, s, "UPDATE", "device_tokens")
	utilsClose(t, s)

	if s, err = OpenStore(ctx, path); err == nil || !strings.Contains(err.Error(), "migrate tokens") {
		if err == nil {
			utilsClose(t, s)
		}
		t.Fatalf("OpenStore = %v, want a migrate tokens error", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer utils.CloseQuietly(db)
	var tok string
	if err := db.QueryRowContext(ctx, `SELECT token FROM device_tokens`).Scan(&tok); err != nil || tok != "plain" || strings.HasPrefix(tok, tokenhash.Prefix) {
		t.Fatalf("token after a failed migration = %q (%v), want the plaintext kept", tok, err)
	}
}

func utilsClose(t *testing.T, s *Store) {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
