package loader

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"

	"her/tui"
	"github.com/xwb1989/sqlparser"
)

// DBProxy is a localhost HTTP server that gives skills controlled access to
// SQLite databases. It's the database equivalent of SkillProxy (the network
// proxy) — skills don't know they're talking to a proxy, they just use
// skillkit.DB() which reads DB_PROXY_URL from the environment.
//
// The proxy enforces trust-tier access control: 2nd-party skills get
// read/write to declared tables, 3rd-party get read-only, and 4th-party
// only access their own sidecar DB (plus optional read-only snapshots).
//
// In Python terms, think of this as a Flask/FastAPI app that sits in front
// of SQLite, validating every request before it touches the database. Skills
// call it like any REST API — they don't know it's running on localhost.
type DBProxy struct {
	listener net.Listener
	port     int
	dbPath   string // path to her.db
	bus      eventBus // event bus for DDL audit events (nil = no events)

	// onDDL is called when a skill executes DDL on its sidecar database.
	// The bot sets this to emit DDLDetected events into the agent event
	// channel, triggering an agent run that decides how to respond.
	// Nil means no agent notification (DDL is still logged via the bus).
	onDDL func(skillName, statement string)

	// readDB is a read-only connection to her.db with an authorizer callback.
	// Every query goes through the authorizer, which checks the current
	// skill's permissions before allowing table access.
	//
	// This is a SEPARATE connection from the main memory.Store — the Store
	// has no authorizer (built-in tools need unrestricted access). The proxy
	// opens her.db through a custom driver name ("sqlite3_dbproxy_<id>")
	// that has the authorizer hook registered.
	readDB *sql.DB

	// writeDB is a read-write connection to her.db, also with the authorizer.
	// Separate from readDB because the read connection is opened with mode=ro.
	// Only used for 2nd-party skills with write permissions.
	writeDB *sql.DB

	// activeTx holds the currently active transaction, if any.
	// Only one transaction at a time (skills execute sequentially).
	// Protected by txMu (separate from the permissions mu to avoid
	// holding the permission lock during long-running transactions).
	txMu     sync.Mutex
	activeTx *activeTransaction

	// mu protects permissions — same pattern as SkillProxy.allowedDomains.
	// The runner writes permissions before each skill run (Lock), and HTTP
	// handlers read them on every request (RLock).
	mu          sync.RWMutex
	permissions *dbPermissions // nil = deny all (no skill running)
}

// activeTransaction tracks an in-progress database transaction.
type activeTransaction struct {
	tx        *sql.Tx
	id        string
	createdAt time.Time
	skillName string
}

// dbPermissions describes what the currently running skill can access.
// Built from the skill's Permissions.DB and Permissions.DBSnapshot fields,
// combined with its trust level.
type dbPermissions struct {
	readTables  map[string]bool // tables the skill can SELECT from
	writeTables map[string]bool // tables the skill can INSERT/UPDATE/DELETE
	trustLevel  TrustLevel
	skillName   string
	skillDir    string // for locating the skill's sidecar DB
}

// eventBus is an interface for emitting events. This lets us use the real
// tui.Bus in production and nil/mock in tests. Same pattern as the embedder
// interface in runner.go.
//
// The parameter type matches tui.Event (which has EventTime + EventSource).
type eventBus interface {
	Emit(e tui.Event)
}

// dbProxyDriverCounter is used to generate unique driver names. Each DBProxy
// instance needs its own registered driver because sql.Register is global
// and driver names must be unique. In tests, multiple proxies may exist.
//
// sync/atomic would be more correct, but a simple counter with a mutex is
// fine for something that runs once at startup.
var dbProxyDriverCounter int
var dbProxyDriverMu sync.Mutex

// NewDBProxy creates and starts the DB proxy on 127.0.0.1:0 (random port).
// The proxy is ready to accept connections when this function returns.
//
// dbPath is the path to her.db. The proxy opens its own read-only connection
// to this file with an authorizer callback that enforces table-level access
// control based on the current skill's permissions.
//
// The caller must call Close() during shutdown.
func NewDBProxy(dbPath string, bus eventBus) (*DBProxy, error) {
	p := &DBProxy{
		dbPath: dbPath,
		bus:    bus,
	}

	// --- Register a custom SQLite driver with an authorizer ---
	//
	// sql.Register() registers a driver globally by name. We can't reuse
	// the default "sqlite3" driver because we need to install an authorizer
	// callback via ConnectHook. Each DBProxy gets a unique driver name to
	// avoid conflicts in tests where multiple proxies may exist.
	//
	// The authorizer fires at query prepare time (sqlite3_prepare), NOT
	// execution time. This means it catches all table access — including
	// through views, CTEs, and triggers — before any data is touched.
	dbProxyDriverMu.Lock()
	dbProxyDriverCounter++
	driverName := fmt.Sprintf("sqlite3_dbproxy_%d", dbProxyDriverCounter)
	dbProxyDriverMu.Unlock()

	sql.Register(driverName, &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			conn.RegisterAuthorizer(p.authorize)
			return nil
		},
	})

	// Open a read-only connection to her.db through the authorizer-equipped
	// driver. WAL mode allows concurrent reads with the main Store connection.
	// mode=ro prevents writes at the SQLite level (belt-and-suspenders with
	// the authorizer).
	db, err := sql.Open(driverName, dbPath+"?_journal_mode=WAL&mode=ro")
	if err != nil {
		return nil, fmt.Errorf("db proxy open: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("db proxy ping: %w", err)
	}
	p.readDB = db

	// Open a read-write connection with the same authorizer. This is used
	// for INSERT/UPDATE/DELETE operations by 2nd-party skills.
	// We reuse the same driver (same authorizer) — the only difference
	// is no mode=ro, so writes are allowed at the SQLite level.
	writeDB, err := sql.Open(driverName, dbPath+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("db proxy write open: %w", err)
	}
	if err := writeDB.Ping(); err != nil {
		db.Close()
		writeDB.Close()
		return nil, fmt.Errorf("db proxy write ping: %w", err)
	}
	p.writeDB = writeDB

	// Set up the HTTP router.
	mux := http.NewServeMux()
	mux.HandleFunc("/db/", p.handleDB)

	// Listen on localhost, random port — same pattern as SkillProxy.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("db proxy listen: %w", err)
	}
	p.listener = listener
	p.port = listener.Addr().(*net.TCPAddr).Port

	// Serve in a background goroutine. http.Serve blocks until the
	// listener is closed — the error on shutdown is expected.
	go func() {
		if err := http.Serve(listener, mux); err != nil {
			log.Debug("db proxy server stopped", "reason", err)
		}
	}()

	log.Info("db proxy started", "port", p.port, "addr", "127.0.0.1")
	return p, nil
}

// Port returns the TCP port the proxy is listening on.
func (p *DBProxy) Port() int {
	return p.port
}

// URL returns the full proxy URL (e.g., "http://127.0.0.1:54321").
// This gets set as DB_PROXY_URL in the skill's environment.
func (p *DBProxy) URL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", p.port)
}

// SetDDLCallback stores the callback for DDL audit events.
// Called from cmd/run.go after creating the bot and agent event channel.
func (p *DBProxy) SetDDLCallback(fn func(skillName, statement string)) {
	p.onDDL = fn
}

// SetPermissions configures access control for the currently running skill.
// Called by the runner before executing a skill that declared db permissions.
//
// Parses the skill's Permissions.DB entries like "expenses:rw" or
// "scheduled_tasks:r" into read/write table maps. The trust level further
// restricts access — 3rd-party skills are downgraded to read-only even
// if they declared :rw.
func (p *DBProxy) SetPermissions(skill *Skill) {
	read := make(map[string]bool)
	write := make(map[string]bool)

	for _, entry := range skill.Permissions.DB {
		// Parse "table:mode" format. Default to read-only if no mode specified.
		table, mode := parseDBPermission(entry)
		read[table] = true
		if mode == "rw" && skill.TrustLevel.canWriteDB() {
			write[table] = true
		}
	}

	// 4th-party snapshot tables are read-only copies placed in the sidecar.
	// They don't go in the read/write maps — handled separately by the
	// snapshot mechanism in Phase 6.

	p.mu.Lock()
	defer p.mu.Unlock()

	p.permissions = &dbPermissions{
		readTables:  read,
		writeTables: write,
		trustLevel:  skill.TrustLevel,
		skillName:   skill.Name,
		skillDir:    skill.Dir,
	}
	log.Debug("db proxy permissions set", "skill", skill.Name, "read", read, "write", write)
}

// ClearPermissions removes the access control, denying all requests.
// Called after a skill finishes executing (via defer in the runner).
func (p *DBProxy) ClearPermissions() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.permissions = nil
	log.Debug("db proxy permissions cleared")
}

// Close shuts down the proxy listener and database connections.
// Called during application shutdown.
func (p *DBProxy) Close() error {
	log.Info("db proxy stopping", "port", p.port)
	// Roll back any active transaction.
	p.txMu.Lock()
	if p.activeTx != nil {
		p.activeTx.tx.Rollback()
		p.activeTx = nil
	}
	p.txMu.Unlock()

	if p.readDB != nil {
		p.readDB.Close()
	}
	if p.writeDB != nil {
		p.writeDB.Close()
	}
	return p.listener.Close()
}

// authorize is the SQLite authorizer callback. It fires at prepare time
// (when SQLite compiles a query) — before any data is read or written.
//
// The callback receives the operation type and relevant arguments:
//   - SQLITE_READ:   arg1=table, arg2=column
//   - SQLITE_SELECT: arg1="", arg2="" (the SELECT statement itself)
//   - SQLITE_INSERT: arg1=table
//   - SQLITE_UPDATE: arg1=table, arg2=column
//   - SQLITE_DELETE: arg1=table
//
// Returns SQLITE_OK (allow), SQLITE_DENY (hard error), or SQLITE_IGNORE
// (silently return NULL for the column).
//
// In Python terms, this is like a database middleware that intercepts every
// query before execution. The difference is it's built into SQLite itself,
// so it can't be bypassed — even views, CTEs, and triggers go through it.
func (p *DBProxy) authorize(op int, arg1, arg2, dbName string) int {
	p.mu.RLock()
	perms := p.permissions
	p.mu.RUnlock()

	// No permissions set = deny everything.
	if perms == nil {
		return sqlite3.SQLITE_DENY
	}

	switch op {
	case sqlite3.SQLITE_READ:
		// arg1 = table name, arg2 = column name.
		// Allow if the table is in readTables or writeTables.
		if perms.readTables[arg1] || perms.writeTables[arg1] {
			return sqlite3.SQLITE_OK
		}
		return sqlite3.SQLITE_DENY

	case sqlite3.SQLITE_SELECT:
		// The SELECT statement itself (not a table read). Always allow —
		// individual table reads are checked via SQLITE_READ above.
		return sqlite3.SQLITE_OK

	case sqlite3.SQLITE_INSERT, sqlite3.SQLITE_UPDATE, sqlite3.SQLITE_DELETE:
		// Write operations. Only allowed if the table is in writeTables.
		if perms.writeTables[arg1] {
			return sqlite3.SQLITE_OK
		}
		return sqlite3.SQLITE_DENY

	case sqlite3.SQLITE_PRAGMA:
		// PRAGMA can expose schema info, change journal mode, etc.
		// Block all PRAGMAs from skills.
		return sqlite3.SQLITE_DENY

	case sqlite3.SQLITE_ATTACH, sqlite3.SQLITE_DETACH:
		// ATTACH DATABASE could give access to arbitrary files.
		// Block unconditionally.
		return sqlite3.SQLITE_DENY

	case sqlite3.SQLITE_FUNCTION:
		// Function calls. Most are safe (COUNT, SUM, etc.) but some
		// are dangerous (load_extension). For now, allow all — the SQL
		// parser in Phase 3 will add function-level filtering.
		return sqlite3.SQLITE_OK

	case sqlite3.SQLITE_TRANSACTION:
		// BEGIN, COMMIT, ROLLBACK — needed for transaction support.
		// The proxy controls when transactions happen (only 2nd-party),
		// so allowing the SQLite-level operation is safe.
		return sqlite3.SQLITE_OK

	case sqlite3.SQLITE_SAVEPOINT:
		// SAVEPOINT, RELEASE, ROLLBACK TO — used internally by
		// database/sql for nested transactions.
		return sqlite3.SQLITE_OK

	default:
		// Unknown or internal operations (CREATE, DROP, etc.).
		// Deny by default — skills shouldn't be doing DDL on her.db.
		return sqlite3.SQLITE_DENY
	}
}

// handleDB is the main HTTP handler that routes requests by method and path.
// All endpoints are under /db/{table} or /db/{table}/{id}.
func (p *DBProxy) handleDB(w http.ResponseWriter, r *http.Request) {
	// Check that a skill is currently running (permissions are set).
	p.mu.RLock()
	perms := p.permissions
	p.mu.RUnlock()

	if perms == nil {
		jsonError(w, "no skill is currently executing", http.StatusForbidden)
		return
	}

	// Extract the table name from the path: /db/{table} or /db/{table}/{id}
	path := strings.TrimPrefix(r.URL.Path, "/db/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		jsonError(w, "missing table name", http.StatusBadRequest)
		return
	}
	table := parts[0]

	// Optional row ID from the path (for PUT/DELETE: /db/{table}/{id}).
	rowID := ""
	if len(parts) == 2 {
		rowID = parts[1]
	}

	switch r.Method {
	case http.MethodGet:
		// GET /db/_schema lists tables in the skill's sidecar DB.
		if table == "_schema" {
			p.handleSchema(w, r, perms)
			return
		}
		p.handleRead(w, r, perms, table)
	case http.MethodPost:
		// POST to /db/_tx/* handles transactions.
		if strings.HasPrefix(table, "_tx") {
			p.handleTransaction(w, r, perms, path)
			return
		}
		// POST to /db/_ddl executes DDL on the skill's sidecar DB.
		if table == "_ddl" {
			p.handleDDL(w, r, perms)
			return
		}
		p.handleInsert(w, r, perms, table)
	case http.MethodPut:
		if rowID == "" {
			jsonError(w, "PUT requires /db/{table}/{id}", http.StatusBadRequest)
			return
		}
		p.handleUpdate(w, r, perms, table, rowID)
	case http.MethodDelete:
		if rowID == "" {
			jsonError(w, "DELETE requires /db/{table}/{id}", http.StatusBadRequest)
			return
		}
		p.handleDelete(w, r, perms, table, rowID)
	default:
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleRead executes a SELECT query against her.db and returns JSON rows.
//
// Query parameters:
//   - where: SQL WHERE clause (e.g., "amount > 50 AND category = 'food'")
//   - limit: max rows to return (default 100, max 1000)
//   - offset: skip this many rows (default 0)
//
// The authorizer enforces table-level access — if the skill doesn't have
// read access to the table, SQLite returns an error at prepare time.
//
// Response format:
//
//	{"rows": [...], "count": 15, "limit": 100, "offset": 0}
func (p *DBProxy) handleRead(w http.ResponseWriter, r *http.Request, perms *dbPermissions, table string) {
	// Quick permission check before even building the query.
	// The authorizer would catch this too, but a clear 403 is better UX
	// than a cryptic SQLite error.
	if !perms.readTables[table] && !perms.writeTables[table] {
		jsonError(w, fmt.Sprintf("read access denied for table %q", table), http.StatusForbidden)
		return
	}

	// Parse query parameters.
	where := r.URL.Query().Get("where")
	limit := clampInt(parseIntDefault(r.URL.Query().Get("limit"), 100), 1, 1000)
	offset := parseIntDefault(r.URL.Query().Get("offset"), 0)

	// Validate the WHERE clause if present. This is defense-in-depth —
	// the authorizer catches everything, but the parser gives better
	// error messages and rejects obvious attacks (subqueries, dangerous
	// functions) before they even reach SQLite.
	if where != "" {
		if err := validateWhere(where); err != nil {
			jsonError(w, fmt.Sprintf("invalid WHERE clause: %s", err), http.StatusBadRequest)
			return
		}
	}

	// Build the SELECT query.
	// Table name is validated against the permission allowlist above, so it's
	// safe to interpolate.
	query := fmt.Sprintf("SELECT * FROM %s", table)
	if where != "" {
		query += " WHERE " + where
	}
	query += fmt.Sprintf(" LIMIT %d OFFSET %d", limit, offset)

	// Execute the query. Use writeDB so reads can see recent writes within
	// the same skill execution (readDB is opened with mode=ro, which may
	// not see WAL writes from writeDB immediately). The authorizer on
	// writeDB enforces the same access control.
	rows, err := p.writeDB.QueryContext(r.Context(), query)
	if err != nil {
		// The authorizer returns "not authorized" for blocked tables.
		if strings.Contains(err.Error(), "not authorized") {
			jsonError(w, fmt.Sprintf("access denied: %s", err), http.StatusForbidden)
			return
		}
		jsonError(w, fmt.Sprintf("query error: %s", err), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	// Scan rows dynamically into []map[string]any.
	//
	// In Python you'd do dict(zip(cursor.description, row)) — in Go we
	// need to manually build scan targets because rows.Scan() requires
	// typed pointers. We use []any pointers and let the driver figure
	// out the types.
	columns, err := rows.Columns()
	if err != nil {
		jsonError(w, fmt.Sprintf("reading columns: %s", err), http.StatusInternalServerError)
		return
	}

	var results []map[string]any
	for rows.Next() {
		// Create a slice of pointers for Scan to write into.
		// Each element is a pointer to an any — the driver will fill in
		// the concrete type (int64, float64, string, []byte, nil).
		values := make([]any, len(columns))
		ptrs := make([]any, len(columns))
		for i := range values {
			ptrs[i] = &values[i]
		}

		if err := rows.Scan(ptrs...); err != nil {
			jsonError(w, fmt.Sprintf("scanning row: %s", err), http.StatusInternalServerError)
			return
		}

		// Build the row map. Convert []byte values to strings for clean
		// JSON output — SQLite TEXT columns come through as []byte via
		// the Go driver.
		row := make(map[string]any, len(columns))
		for i, col := range columns {
			switch v := values[i].(type) {
			case []byte:
				row[col] = string(v)
			default:
				row[col] = v
			}
		}
		results = append(results, row)
	}

	if err := rows.Err(); err != nil {
		jsonError(w, fmt.Sprintf("iterating rows: %s", err), http.StatusInternalServerError)
		return
	}

	// Return empty array instead of null when no rows match.
	if results == nil {
		results = []map[string]any{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"rows":   results,
		"count":  len(results),
		"limit":  limit,
		"offset": offset,
	})
}

// handleInsert handles POST /db/{table} — inserts a row.
//
// Request body: JSON object of column:value pairs.
// Response: {"id": <new_row_id>, "rows_affected": 1}
func (p *DBProxy) handleInsert(w http.ResponseWriter, r *http.Request, perms *dbPermissions, table string) {
	if !perms.writeTables[table] {
		jsonError(w, fmt.Sprintf("write access denied for table %q", table), http.StatusForbidden)
		return
	}

	// Parse the request body as a JSON object.
	var row map[string]any
	body, err := io.ReadAll(r.Body)
	if err != nil {
		jsonError(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(body, &row); err != nil {
		jsonError(w, fmt.Sprintf("invalid JSON: %s", err), http.StatusBadRequest)
		return
	}
	if len(row) == 0 {
		jsonError(w, "empty row", http.StatusBadRequest)
		return
	}

	// Build the INSERT statement.
	columns := make([]string, 0, len(row))
	placeholders := make([]string, 0, len(row))
	values := make([]any, 0, len(row))
	for col, val := range row {
		columns = append(columns, col)
		placeholders = append(placeholders, "?")
		values = append(values, val)
	}

	query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		table,
		strings.Join(columns, ", "),
		strings.Join(placeholders, ", "),
	)

	// Use the transaction if one is active, otherwise use writeDB directly.
	var result sql.Result
	if tx := p.getActiveTx(r); tx != nil {
		result, err = tx.ExecContext(r.Context(), query, values...)
	} else {
		result, err = p.writeDB.ExecContext(r.Context(), query, values...)
	}
	if err != nil {
		if strings.Contains(err.Error(), "not authorized") {
			jsonError(w, fmt.Sprintf("access denied: %s", err), http.StatusForbidden)
			return
		}
		jsonError(w, fmt.Sprintf("insert failed: %s", err), http.StatusInternalServerError)
		return
	}

	id, _ := result.LastInsertId()
	affected, _ := result.RowsAffected()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{
		"id":            id,
		"rows_affected": affected,
	})
}

// handleUpdate handles PUT /db/{table}/{id} — updates a row by ID.
//
// Request body: JSON object of column:value pairs to update.
// Response: {"rows_affected": N}
func (p *DBProxy) handleUpdate(w http.ResponseWriter, r *http.Request, perms *dbPermissions, table, rowID string) {
	if !perms.writeTables[table] {
		jsonError(w, fmt.Sprintf("write access denied for table %q", table), http.StatusForbidden)
		return
	}

	var fields map[string]any
	body, err := io.ReadAll(r.Body)
	if err != nil {
		jsonError(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(body, &fields); err != nil {
		jsonError(w, fmt.Sprintf("invalid JSON: %s", err), http.StatusBadRequest)
		return
	}
	if len(fields) == 0 {
		jsonError(w, "no fields to update", http.StatusBadRequest)
		return
	}

	// Build SET clause: "col1 = ?, col2 = ?"
	setClauses := make([]string, 0, len(fields))
	values := make([]any, 0, len(fields)+1)
	for col, val := range fields {
		setClauses = append(setClauses, col+" = ?")
		values = append(values, val)
	}
	values = append(values, rowID) // WHERE id = ?

	query := fmt.Sprintf("UPDATE %s SET %s WHERE id = ?",
		table,
		strings.Join(setClauses, ", "),
	)

	var result sql.Result
	if tx := p.getActiveTx(r); tx != nil {
		result, err = tx.ExecContext(r.Context(), query, values...)
	} else {
		result, err = p.writeDB.ExecContext(r.Context(), query, values...)
	}
	if err != nil {
		if strings.Contains(err.Error(), "not authorized") {
			jsonError(w, fmt.Sprintf("access denied: %s", err), http.StatusForbidden)
			return
		}
		jsonError(w, fmt.Sprintf("update failed: %s", err), http.StatusInternalServerError)
		return
	}

	affected, _ := result.RowsAffected()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"rows_affected": affected})
}

// handleDelete handles DELETE /db/{table}/{id} — deletes a row by ID.
//
// Response: {"rows_affected": N}
func (p *DBProxy) handleDelete(w http.ResponseWriter, r *http.Request, perms *dbPermissions, table, rowID string) {
	if !perms.writeTables[table] {
		jsonError(w, fmt.Sprintf("write access denied for table %q", table), http.StatusForbidden)
		return
	}

	query := fmt.Sprintf("DELETE FROM %s WHERE id = ?", table)

	var (
		result sql.Result
		err    error
	)
	if tx := p.getActiveTx(r); tx != nil {
		result, err = tx.ExecContext(r.Context(), query, rowID)
	} else {
		result, err = p.writeDB.ExecContext(r.Context(), query, rowID)
	}
	if err != nil {
		if strings.Contains(err.Error(), "not authorized") {
			jsonError(w, fmt.Sprintf("access denied: %s", err), http.StatusForbidden)
			return
		}
		jsonError(w, fmt.Sprintf("delete failed: %s", err), http.StatusInternalServerError)
		return
	}

	affected, _ := result.RowsAffected()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"rows_affected": affected})
}

// handleTransaction handles POST /db/_tx/{action} — begin, commit, rollback.
//
// Transactions are only available to 2nd-party skills. They provide
// atomic multi-step writes (e.g., insert expense + insert line items).
//
//	POST /db/_tx/begin    → {"tx_id": "abc123"}
//	POST /db/_tx/commit   → {"ok": true}
//	POST /db/_tx/rollback → {"ok": true}
func (p *DBProxy) handleTransaction(w http.ResponseWriter, r *http.Request, perms *dbPermissions, path string) {
	// Only 2nd-party skills can use transactions.
	if perms.trustLevel != TrustSecondParty {
		jsonError(w, "transactions only available to 2nd-party skills", http.StatusForbidden)
		return
	}

	// Extract action from path: "_tx/begin", "_tx/commit", "_tx/rollback"
	action := strings.TrimPrefix(path, "_tx/")
	if action == "_tx" || action == "" {
		action = strings.TrimPrefix(path, "_tx")
		action = strings.TrimPrefix(action, "/")
	}

	switch action {
	case "begin":
		p.txMu.Lock()
		defer p.txMu.Unlock()

		// Only one transaction at a time.
		if p.activeTx != nil {
			jsonError(w, "a transaction is already active", http.StatusConflict)
			return
		}

		// Use context.Background(), NOT r.Context(). The transaction must
		// outlive this HTTP request — it spans multiple requests (begin,
		// then inserts, then commit). If we used r.Context(), the tx
		// would be rolled back as soon as this handler returns.
		tx, err := p.writeDB.BeginTx(context.Background(), nil)
		if err != nil {
			jsonError(w, fmt.Sprintf("begin failed: %s", err), http.StatusInternalServerError)
			return
		}

		// Generate a random transaction ID.
		idBytes := make([]byte, 8)
		rand.Read(idBytes)
		txID := hex.EncodeToString(idBytes)

		p.activeTx = &activeTransaction{
			tx:        tx,
			id:        txID,
			createdAt: time.Now(),
			skillName: perms.skillName,
		}

		// Auto-expire after 30 seconds to prevent dangling locks.
		go p.expireTransaction(txID, 30*time.Second)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"tx_id": txID})

	case "commit":
		p.txMu.Lock()
		defer p.txMu.Unlock()

		if p.activeTx == nil {
			jsonError(w, "no active transaction", http.StatusBadRequest)
			return
		}

		err := p.activeTx.tx.Commit()
		p.activeTx = nil
		if err != nil {
			jsonError(w, fmt.Sprintf("commit failed: %s", err), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})

	case "rollback":
		p.txMu.Lock()
		defer p.txMu.Unlock()

		if p.activeTx == nil {
			jsonError(w, "no active transaction", http.StatusBadRequest)
			return
		}

		err := p.activeTx.tx.Rollback()
		p.activeTx = nil
		if err != nil {
			jsonError(w, fmt.Sprintf("rollback failed: %s", err), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})

	default:
		jsonError(w, fmt.Sprintf("unknown transaction action: %q", action), http.StatusBadRequest)
	}
}

// handleDDL handles POST /db/_ddl — executes DDL on the skill's sidecar DB.
//
// Request body: {"sql": "CREATE TABLE applications (id INTEGER PRIMARY KEY, ...)"}
// Response: {"ok": true}
//
// DDL is only allowed on the skill's own sidecar database, never on her.db.
// Every DDL statement is logged and emits a DDLEvent on the event bus for
// the audit system.
func (p *DBProxy) handleDDL(w http.ResponseWriter, r *http.Request, perms *dbPermissions) {
	// Parse the request.
	var req struct {
		SQL string `json:"sql"`
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		jsonError(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		jsonError(w, fmt.Sprintf("invalid JSON: %s", err), http.StatusBadRequest)
		return
	}
	if req.SQL == "" {
		jsonError(w, "missing 'sql' field", http.StatusBadRequest)
		return
	}

	// Quick sanity check: reject obviously non-DDL statements.
	// This isn't a security boundary — the sidecar is the skill's own DB.
	// It's a usability guard to prevent confusion.
	upper := strings.ToUpper(strings.TrimSpace(req.SQL))
	isDDL := strings.HasPrefix(upper, "CREATE ") ||
		strings.HasPrefix(upper, "ALTER ") ||
		strings.HasPrefix(upper, "DROP ")
	if !isDDL {
		jsonError(w, "only CREATE, ALTER, and DROP statements are allowed via _ddl", http.StatusBadRequest)
		return
	}

	// Open the skill's sidecar DB.
	sidecarDB, err := p.openSidecarDB(perms)
	if err != nil {
		jsonError(w, fmt.Sprintf("opening sidecar: %s", err), http.StatusInternalServerError)
		return
	}
	defer sidecarDB.Close()

	// Execute the DDL.
	if _, err := sidecarDB.ExecContext(r.Context(), req.SQL); err != nil {
		jsonError(w, fmt.Sprintf("DDL failed: %s", err), http.StatusInternalServerError)
		return
	}

	// Log and emit audit event on the TUI bus (for display/logging).
	log.Info("sidecar DDL executed", "skill", perms.skillName, "sql", req.SQL)
	if p.bus != nil {
		p.bus.Emit(tui.DDLEvent{
			Time:      time.Now(),
			SkillName: perms.skillName,
			Statement: req.SQL,
		})
	}

	// Notify the agent via the DDL callback (for automated monitoring).
	// The agent decides how to respond: log silently, notify Autumn,
	// revert, or quarantine. Only fires for 4th-party skills — 2nd/3rd
	// party DDL is trusted (written or reviewed by Autumn).
	if p.onDDL != nil && perms.trustLevel == TrustFourthParty {
		p.onDDL(perms.skillName, req.SQL)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// handleSchema handles GET /db/_schema — lists tables in the skill's sidecar DB.
//
// Response: {"tables": [{"name": "applications", "columns": ["id", "company", "status"]}]}
func (p *DBProxy) handleSchema(w http.ResponseWriter, r *http.Request, perms *dbPermissions) {
	sidecarDB, err := p.openSidecarDB(perms)
	if err != nil {
		jsonError(w, fmt.Sprintf("opening sidecar: %s", err), http.StatusInternalServerError)
		return
	}
	defer sidecarDB.Close()

	// Query SQLite's internal schema table for user-created tables.
	// sqlite_master contains all tables, indexes, views, and triggers.
	// We filter to type='table' and exclude internal tables (sqlite_*).
	rows, err := sidecarDB.QueryContext(r.Context(),
		`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		jsonError(w, fmt.Sprintf("querying schema: %s", err), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type tableInfo struct {
		Name    string   `json:"name"`
		Columns []string `json:"columns"`
	}

	var tables []tableInfo
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			jsonError(w, fmt.Sprintf("scanning table name: %s", err), http.StatusInternalServerError)
			return
		}

		// Get column names for this table using PRAGMA table_info.
		colRows, err := sidecarDB.QueryContext(r.Context(),
			fmt.Sprintf("PRAGMA table_info(%s)", name))
		if err != nil {
			jsonError(w, fmt.Sprintf("querying columns for %s: %s", name, err), http.StatusInternalServerError)
			return
		}

		var columns []string
		for colRows.Next() {
			// PRAGMA table_info returns: cid, name, type, notnull, dflt_value, pk
			var cid int
			var colName, colType string
			var notnull int
			var dfltValue *string
			var pk int
			if err := colRows.Scan(&cid, &colName, &colType, &notnull, &dfltValue, &pk); err != nil {
				colRows.Close()
				jsonError(w, fmt.Sprintf("scanning column: %s", err), http.StatusInternalServerError)
				return
			}
			columns = append(columns, colName)
		}
		colRows.Close()

		tables = append(tables, tableInfo{Name: name, Columns: columns})
	}

	if tables == nil {
		tables = []tableInfo{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"tables": tables})
}

// openSidecarDB opens (or creates) the skill's sidecar database file.
// The sidecar lives at <skill.Dir>/<skill.Name>.db.
//
// Unlike the harness-managed sidecar (sidecar.go), this gives the skill
// full DDL control — it can create its own tables, alter schemas, etc.
// The harness's runs/vec_runs tables are also present if the harness
// previously recorded execution history.
func (p *DBProxy) openSidecarDB(perms *dbPermissions) (*sql.DB, error) {
	dbPath := filepath.Join(perms.skillDir, perms.skillName+".db")
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("opening sidecar db: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging sidecar db: %w", err)
	}
	return db, nil
}

// PrepareSnapshots copies declared her.db tables into the skill's sidecar
// database. Called by the runner before executing a 4th-party skill that
// declared db_snapshot permissions.
//
// The snapshot is a full copy of rows — the skill reads from its sidecar
// as if the table were its own. After execution, CleanupSnapshots drops
// the copied tables.
//
// Uses ATTACH DATABASE to copy between databases in a single SQLite
// connection. This is efficient — SQLite handles the copy internally,
// no row-by-row marshaling.
func (p *DBProxy) PrepareSnapshots(skill *Skill) error {
	if len(skill.Permissions.DBSnapshot) == 0 {
		return nil
	}

	sidecarPath := filepath.Join(skill.Dir, skill.Name+".db")

	// Open the sidecar and attach her.db to it.
	db, err := sql.Open("sqlite3", sidecarPath+"?_journal_mode=WAL")
	if err != nil {
		return fmt.Errorf("opening sidecar for snapshot: %w", err)
	}
	defer db.Close()

	// ATTACH the main her.db so we can copy tables across.
	if _, err := db.Exec(fmt.Sprintf(`ATTACH DATABASE '%s' AS herdb`, p.dbPath)); err != nil {
		return fmt.Errorf("attaching her.db: %w", err)
	}
	defer db.Exec("DETACH DATABASE herdb")

	// Copy each declared snapshot table.
	for _, entry := range skill.Permissions.DBSnapshot {
		table, _ := parseDBPermission(entry) // ignore mode, snapshots are always read-only
		// CREATE TABLE AS SELECT copies both schema and data.
		// The _snapshot_ prefix avoids conflicts with the skill's own tables.
		snapshotName := "_snapshot_" + table
		_, err := db.Exec(fmt.Sprintf(
			"CREATE TABLE IF NOT EXISTS %s AS SELECT * FROM herdb.%s",
			snapshotName, table))
		if err != nil {
			return fmt.Errorf("copying snapshot of %s: %w", table, err)
		}
		log.Debug("snapshot created", "skill", skill.Name, "table", table, "as", snapshotName)
	}

	return nil
}

// CleanupSnapshots removes snapshot tables from the skill's sidecar.
// Called after the skill finishes executing (via defer in the runner).
func (p *DBProxy) CleanupSnapshots(skill *Skill) {
	if len(skill.Permissions.DBSnapshot) == 0 {
		return
	}

	sidecarPath := filepath.Join(skill.Dir, skill.Name+".db")
	db, err := sql.Open("sqlite3", sidecarPath+"?_journal_mode=WAL")
	if err != nil {
		log.Warn("failed to open sidecar for snapshot cleanup", "skill", skill.Name, "err", err)
		return
	}
	defer db.Close()

	for _, entry := range skill.Permissions.DBSnapshot {
		table, _ := parseDBPermission(entry)
		snapshotName := "_snapshot_" + table
		if _, err := db.Exec(fmt.Sprintf("DROP TABLE IF EXISTS %s", snapshotName)); err != nil {
			log.Warn("failed to drop snapshot table", "skill", skill.Name, "table", snapshotName, "err", err)
		}
	}
}

// getActiveTx returns the active transaction if the request includes a
// matching X-Transaction header. Returns nil if no transaction is active
// or the header doesn't match.
func (p *DBProxy) getActiveTx(r *http.Request) *sql.Tx {
	txID := r.Header.Get("X-Transaction")
	if txID == "" {
		return nil
	}

	p.txMu.Lock()
	defer p.txMu.Unlock()

	if p.activeTx != nil && p.activeTx.id == txID {
		return p.activeTx.tx
	}
	return nil
}

// expireTransaction rolls back a transaction if it's still active after
// the timeout. This prevents dangling locks if a skill crashes mid-transaction.
func (p *DBProxy) expireTransaction(txID string, timeout time.Duration) {
	time.Sleep(timeout)

	p.txMu.Lock()
	defer p.txMu.Unlock()

	if p.activeTx != nil && p.activeTx.id == txID {
		log.Warn("transaction expired, rolling back",
			"tx_id", txID,
			"skill", p.activeTx.skillName,
			"age", time.Since(p.activeTx.createdAt),
		)
		p.activeTx.tx.Rollback()
		p.activeTx = nil
	}
}

// jsonError writes a JSON error response. Centralizes error formatting
// so all proxy responses are consistently structured.
func jsonError(w http.ResponseWriter, msg string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// parseIntDefault parses a string as an integer, returning defaultVal if
// the string is empty or invalid.
func parseIntDefault(s string, defaultVal int) int {
	if s == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return defaultVal
	}
	return n
}

// clampInt restricts n to the range [min, max].
func clampInt(n, min, max int) int {
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

// validateWhere checks a SQL WHERE clause for dangerous patterns.
//
// This is defense-in-depth — the SQLite authorizer is the real security
// boundary. The parser catches obvious abuse early with better error
// messages than "not authorized".
//
// The trick: wrap the WHERE clause in a dummy SELECT so the parser can
// handle it (it needs a complete SQL statement). Then walk the AST
// looking for subqueries, dangerous function calls, etc.
//
// Rejected patterns:
//   - Subqueries: EXISTS (SELECT ...), IN (SELECT ...), etc.
//   - Dangerous functions: load_extension, fts3_tokenizer, etc.
//   - Semicolons: prevents statement stacking (DROP TABLE; etc.)
//
// Allowed patterns:
//   - Simple comparisons: amount > 50, category = 'food'
//   - Logical operators: AND, OR, NOT
//   - BETWEEN, IN (literal list), LIKE, IS NULL
//   - Safe functions: COUNT, SUM, COALESCE, etc.
func validateWhere(where string) error {
	// Quick check: reject semicolons entirely. This prevents statement
	// stacking like "1=1; DROP TABLE expenses".
	if strings.Contains(where, ";") {
		return fmt.Errorf("semicolons not allowed in WHERE clause")
	}

	// Wrap in a dummy SELECT so the parser can handle it.
	stmt, err := sqlparser.Parse("SELECT * FROM _t WHERE " + where)
	if err != nil {
		return fmt.Errorf("invalid WHERE clause: %w", err)
	}

	// The parsed statement should be a SELECT.
	sel, ok := stmt.(*sqlparser.Select)
	if !ok {
		return fmt.Errorf("unexpected statement type after parsing WHERE clause")
	}

	// Walk the WHERE expression AST looking for dangerous patterns.
	return walkExpr(sel.Where.Expr)
}

// walkExpr recursively checks an expression AST node for dangerous patterns.
// Returns an error if anything suspicious is found.
func walkExpr(expr sqlparser.Expr) error {
	if expr == nil {
		return nil
	}

	switch node := expr.(type) {
	case *sqlparser.Subquery:
		// Subqueries can reference other tables, bypassing our permission
		// check. The authorizer would catch it, but we reject it here
		// with a clear error message.
		return fmt.Errorf("subqueries not allowed in WHERE clause")

	case *sqlparser.ExistsExpr:
		// EXISTS (SELECT ...) is a different AST node than a bare Subquery.
		return fmt.Errorf("subqueries not allowed in WHERE clause")

	case *sqlparser.FuncExpr:
		// Check function name against the blocklist.
		fname := strings.ToLower(node.Name.String())
		if blockedFunctions[fname] {
			return fmt.Errorf("function %q not allowed", fname)
		}
		// Check function arguments recursively.
		for _, arg := range node.Exprs {
			if aliased, ok := arg.(*sqlparser.AliasedExpr); ok {
				if err := walkExpr(aliased.Expr); err != nil {
					return err
				}
			}
		}

	case *sqlparser.ComparisonExpr:
		if err := walkExpr(node.Left); err != nil {
			return err
		}
		if err := walkExpr(node.Right); err != nil {
			return err
		}

	case *sqlparser.AndExpr:
		if err := walkExpr(node.Left); err != nil {
			return err
		}
		return walkExpr(node.Right)

	case *sqlparser.OrExpr:
		if err := walkExpr(node.Left); err != nil {
			return err
		}
		return walkExpr(node.Right)

	case *sqlparser.NotExpr:
		return walkExpr(node.Expr)

	case *sqlparser.ParenExpr:
		return walkExpr(node.Expr)

	case *sqlparser.RangeCond:
		// BETWEEN X AND Y
		if err := walkExpr(node.Left); err != nil {
			return err
		}
		if err := walkExpr(node.From); err != nil {
			return err
		}
		return walkExpr(node.To)

	case *sqlparser.IsExpr:
		return walkExpr(node.Expr)

	case sqlparser.ValTuple:
		// IN (val1, val2, ...) — check each value in the tuple for
		// nested subqueries.
		for _, val := range node {
			if err := walkExpr(val); err != nil {
				return err
			}
		}
		return nil

	// Leaf nodes — safe, no recursion needed.
	case *sqlparser.ColName, *sqlparser.SQLVal, *sqlparser.NullVal,
		*sqlparser.BoolVal, sqlparser.BoolVal:
		return nil
	}

	// For any node type we don't explicitly handle, allow it.
	// The authorizer is the real safety net.
	return nil
}

// blockedFunctions is a set of SQL functions that skills are not allowed
// to call. These can be used for privilege escalation or data exfiltration.
var blockedFunctions = map[string]bool{
	"load_extension":   true, // load arbitrary shared libraries
	"fts3_tokenizer":   true, // can crash SQLite
	"readfile":         true, // read arbitrary files (if fileio extension loaded)
	"writefile":        true, // write arbitrary files
	"edit":             true, // interactive editing
	"zipfile":          true, // file system access
	"sqlar_compress":   true, // archive access
	"sqlar_uncompress": true, // archive access
}

// parseDBPermission splits a "table:mode" string into table name and mode.
// Supported modes: "r" (read-only), "rw" (read-write).
// Defaults to "r" if no mode is specified.
//
// Examples:
//
//	"expenses:rw"       → ("expenses", "rw")
//	"scheduled_tasks:r" → ("scheduled_tasks", "r")
//	"mood_entries"      → ("mood_entries", "r")
func parseDBPermission(entry string) (table, mode string) {
	parts := strings.SplitN(entry, ":", 2)
	table = parts[0]
	if len(parts) == 2 {
		mode = parts[1]
	} else {
		mode = "r"
	}
	return table, mode
}

// canWriteDB returns whether the trust level allows write access to her.db.
// Only 2nd-party skills (written by Autumn, hash verified) can write.
// 3rd-party (agent-modified) and below are read-only.
func (t TrustLevel) canWriteDB() bool {
	return t == TrustSecondParty
}
