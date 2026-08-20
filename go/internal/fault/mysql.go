package fault

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func init() {
	Register("mysql", func() Fault { return &MySQLFault{} })
}

// The mysql CLI client is an external dependency (like tc / stress-ng /
// kubectl), so the atom keeps the zero-dependency promise. The hooks below
// are swappable in tests to avoid requiring a real database.
var (
	mysqlLookPath = exec.LookPath
	mysqlRun      = func(args []string, password string, timeout time.Duration) (string, error) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Env = mysqlEnv(password)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	mysqlSpawn = func(args []string, password string) (*exec.Cmd, error) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Env = mysqlEnv(password)
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		go cmd.Wait() // reap the child so it never becomes a zombie
		return cmd, nil
	}
)

// mysqlEnv carries the password via MYSQL_PWD so it never appears on the
// command line, in Describe(), or in the timeline.
func mysqlEnv(password string) []string {
	env := append([]string(nil), os.Environ()...)
	return append(env, "MYSQL_PWD="+password)
}

// MySQLFault injects database-instance faults through the mysql CLI client
// (chaosblade "blade create mysql" equivalents):
//
//   - connection: occupy N slots of the connection pool with SLEEP sessions;
//   - lock: hold "LOCK TABLES t WRITE", blocking writes to the table;
//   - session: KILL sessions matching user / db / command / SQL pattern.
//
// Recover is idempotent: holder processes are killed (their sessions die and
// release connections/locks); session kills are irreversible by nature.
type MySQLFault struct {
	Host     string
	Port     int
	User     string
	Password string
	Database string
	Mode     string // connection | lock | session

	// connection mode
	Connections int
	// lock mode
	Table string
	// session mode criteria (at least one required)
	SessionUser    string
	SessionDB      string
	SessionCommand string
	SessionPattern string

	// Duration is used to size the SLEEP() of holder sessions; the
	// experiment timer also kills them at the same deadline.
	Duration time.Duration

	clients []*exec.Cmd
	killed  int
}

var identRE = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

func (f *MySQLFault) Name() string        { return "mysql" }
func (f *MySQLFault) Description() string { return "database instance faults via mysql client (connection/lock/session)" }

func (f *MySQLFault) Describe() string {
	return fmt.Sprintf(
		"mysql(mode=%s host=%s port=%d user=%s database=%s connections=%d table=%s)",
		f.Mode, f.Host, f.Port, f.User, f.Database, f.Connections, f.Table,
	)
}

func (f *MySQLFault) baseArgs() []string {
	args := []string{"mysql", "-h", f.Host, "-P", strconv.Itoa(f.Port), "-u", f.User, "--connect-timeout=5"}
	if f.Database != "" {
		args = append(args, "-D", f.Database)
	}
	return args
}

func (f *MySQLFault) run(sql string, timeout time.Duration) (string, error) {
	return mysqlRun(append(f.baseArgs(), "-e", sql), f.Password, timeout)
}

func (f *MySQLFault) Check() error {
	switch f.Mode {
	case "connection", "lock", "session":
	default:
		return Errf("unsupported mysql mode %q (connection|lock|session)", f.Mode)
	}
	if f.Host == "" {
		f.Host = "127.0.0.1"
	}
	if f.Port == 0 {
		f.Port = 3306 // zero value means "unset", not an invalid port
	}
	if f.Port < 1 || f.Port > 65535 {
		return Errf("port must be in [1, 65535]")
	}
	if f.User == "" || f.Password == "" {
		return Errf("user and password are required")
	}
	if _, err := mysqlLookPath("mysql"); err != nil {
		return Errf("mysql CLI not found: install the mysql client")
	}
	switch f.Mode {
	case "connection":
		if f.Connections < 1 {
			return Errf("connections must be >= 1")
		}
		if f.Duration <= 0 {
			return Errf("duration must be > 0 for connection mode")
		}
		// Never occupy more slots than the server allows.
		out, err := f.run("SELECT @@max_connections", 10*time.Second)
		if err != nil {
			return Errf("cannot connect to mysql at %s:%d: %s", f.Host, f.Port, strings.TrimSpace(out))
		}
		maxConn, err := strconv.Atoi(lastLine(out))
		if err != nil {
			return Errf("cannot parse max_connections from %q", out)
		}
		if f.Connections >= maxConn {
			return Errf("connections=%d must be < max_connections=%d", f.Connections, maxConn)
		}
	case "lock":
		if f.Database == "" || f.Table == "" {
			return Errf("lock mode requires database and table")
		}
		if !identRE.MatchString(f.Database) || !identRE.MatchString(f.Table) {
			return Errf("database/table must match [A-Za-z0-9_]+")
		}
		if f.Duration <= 0 {
			return Errf("duration must be > 0 for lock mode")
		}
		out, err := f.run(
			fmt.Sprintf("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='%s' AND table_name='%s'", f.Database, f.Table),
			10*time.Second,
		)
		if err != nil {
			return Errf("cannot connect to mysql at %s:%d: %s", f.Host, f.Port, strings.TrimSpace(out))
		}
		if strings.TrimSpace(lastLine(out)) == "0" {
			return Errf("table %s.%s not found", f.Database, f.Table)
		}
	case "session":
		if f.SessionUser == "" && f.SessionDB == "" && f.SessionCommand == "" && f.SessionPattern == "" {
			return Errf("session mode requires at least one of session-user/session-db/session-command/session-pattern")
		}
	}
	return nil
}

func (f *MySQLFault) Inject() error {
	if err := f.Check(); err != nil {
		return err
	}
	switch f.Mode {
	case "connection":
		return f.injectConnection()
	case "lock":
		return f.injectLock()
	default:
		return f.injectSession()
	}
}

func (f *MySQLFault) injectConnection() error {
	sleepSQL := fmt.Sprintf("SELECT SLEEP(%d)", int(f.Duration.Seconds()))
	for i := 0; i < f.Connections; i++ {
		cmd, err := mysqlSpawn(append(f.baseArgs(), "-e", sleepSQL), f.Password)
		if err != nil {
			f.Recover()
			return Errf("failed to open mysql connection %d/%d: %v", i+1, f.Connections, err)
		}
		f.clients = append(f.clients, cmd)
	}
	return nil
}

func (f *MySQLFault) injectLock() error {
	lockSQL := fmt.Sprintf("LOCK TABLES `%s` WRITE; SELECT SLEEP(%d)", f.Table, int(f.Duration.Seconds()))
	cmd, err := mysqlSpawn(append(f.baseArgs(), "-e", lockSQL), f.Password)
	if err != nil {
		f.Recover()
		return Errf("failed to acquire table lock: %v", err)
	}
	f.clients = append(f.clients, cmd)
	return nil
}

func (f *MySQLFault) injectSession() error {
	out, err := f.run(
		"SELECT ID, USER, HOST, DB, COMMAND, INFO FROM information_schema.PROCESSLIST WHERE ID <> CONNECTION_ID()",
		10*time.Second,
	)
	if err != nil {
		return Errf("cannot query processlist: %s", strings.TrimSpace(out))
	}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n")[1:] { // skip header
		fields := strings.Split(line, "\t")
		if len(fields) < 6 {
			continue
		}
		id, suser, sdb, scmd := fields[0], fields[1], fields[3], fields[4]
		sinfo := strings.Join(fields[5:], "\t")
		if f.SessionUser != "" && suser != f.SessionUser {
			continue
		}
		if f.SessionDB != "" && sdb != f.SessionDB {
			continue
		}
		if f.SessionCommand != "" && scmd != f.SessionCommand {
			continue
		}
		if f.SessionPattern != "" && !strings.Contains(sinfo, f.SessionPattern) {
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return Errf("no session matches the given criteria")
	}
	var kills []string
	for _, id := range ids {
		kills = append(kills, "KILL "+id)
	}
	if out, err := f.run(strings.Join(kills, "; "), 10*time.Second); err != nil {
		return Errf("failed to kill sessions: %s", strings.TrimSpace(out))
	}
	f.killed = len(ids)
	return nil
}

// Killed reports how many sessions were terminated (session mode only).
func (f *MySQLFault) Killed() int { return f.killed }

func (f *MySQLFault) Recover() error {
	// Killing the holder processes ends their sessions, which releases both
	// the occupied connections and the table lock. Session-mode kills are
	// irreversible (like the process atom), so there is nothing to roll back.
	for _, cmd := range f.clients {
		if cmd.Process != nil && cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
		}
	}
	f.clients = nil
	return nil
}

// lastLine returns the final non-empty line of a mysql -e result (the value
// row, after the column-name header).
func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}
