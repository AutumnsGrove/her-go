-- Simulation results database schema

CREATE TABLE IF NOT EXISTS sim_runs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
	suite_name TEXT NOT NULL,
	suite_path TEXT NOT NULL,
	chat_model TEXT,
	agent_model TEXT,
	embed_model TEXT,
	memory_model TEXT,
	mood_model TEXT,
	total_messages INTEGER,
	total_cost_usd REAL,
	duration_ms INTEGER
);

CREATE TABLE IF NOT EXISTS sim_messages (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id INTEGER NOT NULL REFERENCES sim_runs(id),
	turn_number INTEGER,
	timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
	role TEXT NOT NULL,
	content TEXT NOT NULL,
	conversation_id TEXT
);

CREATE TABLE IF NOT EXISTS sim_memories (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id INTEGER NOT NULL REFERENCES sim_runs(id),
	timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
	memory TEXT NOT NULL,
	category TEXT,
	subject TEXT DEFAULT 'user',
	importance INTEGER DEFAULT 5,
	active BOOLEAN DEFAULT 1
);

CREATE TABLE IF NOT EXISTS sim_mood_entries (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id INTEGER NOT NULL REFERENCES sim_runs(id),
	ts DATETIME DEFAULT CURRENT_TIMESTAMP,
	kind TEXT NOT NULL,
	valence INTEGER NOT NULL,
	labels TEXT NOT NULL DEFAULT '[]',
	associations TEXT NOT NULL DEFAULT '[]',
	note TEXT,
	source TEXT NOT NULL,
	confidence REAL NOT NULL DEFAULT 0,
	conversation_id TEXT
);

CREATE TABLE IF NOT EXISTS sim_metrics (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id INTEGER NOT NULL REFERENCES sim_runs(id),
	timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
	model TEXT NOT NULL,
	prompt_tokens INTEGER,
	completion_tokens INTEGER,
	total_tokens INTEGER,
	cost_usd REAL,
	latency_ms INTEGER
);

CREATE TABLE IF NOT EXISTS sim_agent_turns (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id INTEGER NOT NULL REFERENCES sim_runs(id),
	turn_number INTEGER,
	timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
	turn_index INTEGER,
	role TEXT NOT NULL,
	tool_name TEXT,
	tool_args TEXT,
	content TEXT
);

CREATE TABLE IF NOT EXISTS sim_summaries (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id INTEGER NOT NULL REFERENCES sim_runs(id),
	timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
	conversation_id TEXT,
	summary TEXT NOT NULL,
	messages_summarized INTEGER
);

CREATE TABLE IF NOT EXISTS sim_run_labels (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id INTEGER NOT NULL REFERENCES sim_runs(id),
	label TEXT NOT NULL UNIQUE,
	note TEXT,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS sim_calendar_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id INTEGER NOT NULL REFERENCES sim_runs(id),
	captured_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	event_id TEXT,
	title TEXT,
	start TEXT,
	end TEXT,
	location TEXT,
	notes TEXT,
	calendar TEXT,
	job TEXT
);

-- Views for common queries

CREATE VIEW IF NOT EXISTS latest_runs AS
SELECT r.*
FROM sim_runs r
INNER JOIN (
	SELECT suite_name, MAX(id) AS max_id
	FROM sim_runs
	GROUP BY suite_name
) latest ON r.id = latest.max_id;

CREATE VIEW IF NOT EXISTS run_summary AS
SELECT
	r.id,
	r.timestamp,
	r.suite_name,
	r.agent_model,
	r.chat_model,
	r.total_cost_usd,
	r.duration_ms,
	COUNT(DISTINCT f.id) AS memories_saved,
	COUNT(DISTINCT m.id) / 2 AS turns
FROM sim_runs r
LEFT JOIN sim_memories f ON f.run_id = r.id
LEFT JOIN sim_messages m ON m.run_id = r.id AND m.role = 'user'
GROUP BY r.id;
