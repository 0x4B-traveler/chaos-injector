package fault

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// stubMySQL swaps the mysql client hooks so tests never need a real
// database; every stub still receives the args/env exactly as production.
func stubMySQL(
	t *testing.T,
	run func(args []string, password string, timeout time.Duration) (string, error),
	spawn func(args []string, password string) (*exec.Cmd, error),
	lookPath func(string) (string, error),
) {
	t.Helper()
	oldRun, oldSpawn, oldLook := mysqlRun, mysqlSpawn, mysqlLookPath
	mysqlRun, mysqlSpawn, mysqlLookPath = run, spawn, lookPath
	t.Cleanup(func() { mysqlRun, mysqlSpawn, mysqlLookPath = oldRun, oldSpawn, oldLook })
}

func okLookPath(string) (string, error) { return "/usr/bin/mysql", nil }
func failLookPath(string) (string, error) {
	return "", &exec.Error{Name: "mysql", Err: exec.ErrNotFound}
}

func TestMySQLParameterValidation(t *testing.T) {
	stubMySQL(t, func(args []string, _ string, _ time.Duration) (string, error) {
		return "@@max_connections\n151\n", nil
	}, func(args []string, _ string) (*exec.Cmd, error) {
		return nil, nil
	}, okLookPath)

	cases := []struct {
		name string
		f    *MySQLFault
		want string
	}{
		{"bad mode", &MySQLFault{Mode: "drop", User: "u", Password: "p"}, "unsupported mysql mode"},
		{"bad port", &MySQLFault{Mode: "connection", Port: 70000, User: "u", Password: "p"}, "port must be in"},
		{"no password", &MySQLFault{Mode: "connection", User: "u"}, "user and password are required"},
		{"zero connections", &MySQLFault{Mode: "connection", User: "u", Password: "p", Duration: time.Second}, "connections must be >= 1"},
		{"zero duration", &MySQLFault{Mode: "connection", User: "u", Password: "p", Connections: 5}, "duration must be > 0"},
		{"lock without table", &MySQLFault{Mode: "lock", User: "u", Password: "p", Database: "app", Duration: time.Second}, "lock mode requires"},
		{"lock bad table", &MySQLFault{Mode: "lock", User: "u", Password: "p", Database: "app", Table: "x; DROP TABLE t", Duration: time.Second}, "must match"},
		{"session no criteria", &MySQLFault{Mode: "session", User: "u", Password: "p"}, "at least one of"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.f.Check()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Check() = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestMySQLCheckRequiresCLI(t *testing.T) {
	stubMySQL(t, func([]string, string, time.Duration) (string, error) { return "", nil },
		func([]string, string) (*exec.Cmd, error) { return nil, nil }, failLookPath)
	err := (&MySQLFault{Mode: "connection", User: "u", Password: "p", Connections: 5, Duration: time.Second}).Check()
	if err == nil || !strings.Contains(err.Error(), "mysql CLI not found") {
		t.Fatalf("Check() = %v, want mysql CLI not found", err)
	}
}

func TestMySQLConnectionCheckCapsMax(t *testing.T) {
	var gotSQL string
	stubMySQL(t, func(args []string, _ string, _ time.Duration) (string, error) {
		gotSQL = strings.Join(args, " ")
		return "@@max_connections\n8\n", nil
	}, func([]string, string) (*exec.Cmd, error) { return nil, nil }, okLookPath)

	tooMany := &MySQLFault{Mode: "connection", User: "u", Password: "p", Connections: 20, Duration: time.Second}
	if err := tooMany.Check(); err == nil || !strings.Contains(err.Error(), "max_connections=8") {
		t.Fatalf("Check() = %v, want max_connections rejection", err)
	}
	if !strings.Contains(gotSQL, "max_connections") {
		t.Fatalf("check must query max_connections, got %q", gotSQL)
	}

	ok := &MySQLFault{Mode: "connection", User: "u", Password: "p", Connections: 5, Duration: time.Second}
	if err := ok.Check(); err != nil {
		t.Fatalf("Check() = %v, want nil", err)
	}
}

func TestMySQLLockCheckRejectsMissingTable(t *testing.T) {
	stubMySQL(t, func(args []string, _ string, _ time.Duration) (string, error) {
		return "COUNT(*)\n0\n", nil
	}, func([]string, string) (*exec.Cmd, error) { return nil, nil }, okLookPath)
	f := &MySQLFault{Mode: "lock", User: "u", Password: "p", Database: "app", Table: "orders", Duration: time.Second}
	if err := f.Check(); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("Check() = %v, want table-not-found", err)
	}
}

func TestMySQLConnectionInjectAndRollback(t *testing.T) {
	var spawned []string
	stubMySQL(t, func([]string, string, time.Duration) (string, error) { return "@@max_connections\n151\n", nil },
		func(args []string, password string) (*exec.Cmd, error) {
			spawned = append(spawned, strings.Join(args, " "))
			if strings.Contains(strings.Join(args, " "), password) {
				t.Errorf("password leaked into mysql args: %v", args)
			}
			return &exec.Cmd{}, nil
		}, okLookPath)

	f := &MySQLFault{
		Mode: "connection", User: "u", Password: "secret", Connections: 3, Duration: 5 * time.Second,
	}
	if err := f.Inject(); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if len(spawned) != 3 {
		t.Fatalf("spawned %d holders, want 3", len(spawned))
	}
	for _, args := range spawned {
		if !strings.Contains(args, "SELECT SLEEP(5)") {
			t.Fatalf("holder sql missing SLEEP(5): %q", args)
		}
		if strings.Contains(args, "secret") {
			t.Fatalf("password leaked into mysql args: %q", args)
		}
	}
	if err := f.Recover(); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(f.clients) != 0 {
		t.Fatalf("clients not cleared after Recover: %d", len(f.clients))
	}
}

// TestMySQLRecoverKillsLiveHolders proves Recover really terminates running
// holder sessions, releasing connections and table locks.
func TestMySQLRecoverKillsLiveHolders(t *testing.T) {
	cmd := longRunningCmd()
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start holder process: %v", err)
	}
	go cmd.Wait()
	f := &MySQLFault{clients: []*exec.Cmd{cmd}}
	if err := f.Recover(); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	// ProcessState is filled by the reaping Wait(); give it a moment.
	deadline := time.Now().Add(3 * time.Second)
	for cmd.ProcessState == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if cmd.ProcessState == nil {
		t.Fatal("holder process still running after Recover")
	}
}

func TestMySQLLockInjectHoldsLockSession(t *testing.T) {
	var sql string
	stubMySQL(t, func(args []string, _ string, _ time.Duration) (string, error) {
		return "COUNT(*)\n1\n", nil
	}, func(args []string, _ string) (*exec.Cmd, error) {
		sql = strings.Join(args, " ")
		return &exec.Cmd{}, nil
	}, okLookPath)

	f := &MySQLFault{Mode: "lock", User: "u", Password: "p", Database: "app", Table: "orders", Duration: 5 * time.Second}
	if err := f.Inject(); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if !strings.Contains(sql, "LOCK TABLES `orders` WRITE") || !strings.Contains(sql, "SLEEP(5)") {
		t.Fatalf("lock holder sql = %q", sql)
	}
}

func TestMySQLSessionKill(t *testing.T) {
	const rows = "ID\tUSER\tHOST\tDB\tCOMMAND\tINFO\n" +
		"1\troot\tlocalhost\tapp\tQuery\tSELECT SLEEP(5)\n" +
		"2\troot\tlocalhost\tapp\tSleep\tNULL\n" +
		"3\tapp\t10.0.0.1\tapp\tQuery\tSELECT * FROM users\n"
	var gotSQL []string
	stubMySQL(t, func(args []string, _ string, _ time.Duration) (string, error) {
		sql := args[len(args)-1]
		gotSQL = append(gotSQL, sql)
		if strings.Contains(sql, "PROCESSLIST") {
			if !strings.Contains(sql, "CONNECTION_ID()") {
				t.Errorf("processlist query must exclude own connection: %q", sql)
			}
			return rows, nil
		}
		if strings.Contains(sql, "KILL") {
			return "", nil
		}
		return "@@max_connections\n151\n", nil
	}, func([]string, string) (*exec.Cmd, error) { return nil, nil }, okLookPath)

	f := &MySQLFault{Mode: "session", User: "u", Password: "p", SessionPattern: "SLEEP"}
	if err := f.Inject(); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if f.Killed() != 1 {
		t.Fatalf("Killed() = %d, want 1", f.Killed())
	}
	last := gotSQL[len(gotSQL)-1]
	if !strings.Contains(last, "KILL 1") || strings.Contains(last, "KILL 3") {
		t.Fatalf("kill sql = %q, want only matching session 1", last)
	}
}

func TestMySQLSessionNoMatchRejected(t *testing.T) {
	const rows = "ID\tUSER\tHOST\tDB\tCOMMAND\tINFO\n1\troot\tlocalhost\tapp\tQuery\tSELECT 1\n"
	stubMySQL(t, func(args []string, _ string, _ time.Duration) (string, error) {
		return rows, nil
	}, func([]string, string) (*exec.Cmd, error) { return nil, nil }, okLookPath)

	f := &MySQLFault{Mode: "session", User: "u", Password: "p", SessionPattern: "zzz"}
	err := f.Inject()
	if err == nil || !strings.Contains(err.Error(), "no session matches") {
		t.Fatalf("Inject() = %v, want no-session-match", err)
	}
}

func TestMySQLDescribeOmitsPassword(t *testing.T) {
	f := &MySQLFault{Mode: "connection", User: "root", Password: "s3cret", Database: "app", Connections: 10}
	if strings.Contains(f.Describe(), "s3cret") {
		t.Fatalf("Describe() leaked password: %s", f.Describe())
	}
}

func longRunningCmd() *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.Command("ping", "-n", "60", "127.0.0.1")
	}
	return exec.Command("sleep", "1000")
}
