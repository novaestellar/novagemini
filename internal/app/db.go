package app

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var (
	db     *sql.DB
	dbw    *dbx
	dbOnce sync.Once
)

const schema = `
CREATE TABLE IF NOT EXISTS proxies (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT NOT NULL,
    url          TEXT NOT NULL,
    enabled      INTEGER NOT NULL DEFAULT 1,
    weight       INTEGER NOT NULL DEFAULT 1,
    fail_count   INTEGER NOT NULL DEFAULT 0,
    last_used    INTEGER,
    last_error   TEXT,
    created_at   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS requests (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    ts              INTEGER NOT NULL,
    model           TEXT NOT NULL,
    upstream_model  TEXT,
    proxy_id        INTEGER,
    proxy_name      TEXT,
    account_id      INTEGER,
    account_label   TEXT,
    status          INTEGER NOT NULL,
    error           TEXT,
    ttfb_ms         INTEGER,
    total_ms        INTEGER NOT NULL,
    prompt_chars    INTEGER NOT NULL,
    response_chars  INTEGER NOT NULL,
    prompt_tokens   INTEGER NOT NULL,
    output_tokens   INTEGER NOT NULL,
    endpoint        TEXT,
    stream          INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_requests_ts ON requests(ts);
CREATE INDEX IF NOT EXISTS idx_requests_proxy ON requests(proxy_id);
CREATE INDEX IF NOT EXISTS idx_requests_model ON requests(model);

-- Hourly aggregate (æ°¸ä¹…ä¿ç•™ï¼Œæ˜Žç»†åªç•™ 30 å¤©)
CREATE TABLE IF NOT EXISTS stats_hourly (
    bucket          INTEGER NOT NULL,    -- unix ts of hour start
    model           TEXT NOT NULL,
    proxy_id        INTEGER NOT NULL,    -- 0 = no proxy
    requests        INTEGER NOT NULL DEFAULT 0,
    successes       INTEGER NOT NULL DEFAULT 0,
    failures        INTEGER NOT NULL DEFAULT 0,
    total_ms        INTEGER NOT NULL DEFAULT 0,
    p50_ms          INTEGER NOT NULL DEFAULT 0,
    p95_ms          INTEGER NOT NULL DEFAULT 0,
    prompt_tokens   INTEGER NOT NULL DEFAULT 0,
    output_tokens   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (bucket, model, proxy_id)
);
CREATE INDEX IF NOT EXISTS idx_hourly_bucket ON stats_hourly(bucket);

-- Daily aggregate (æ°¸ä¹…ä¿ç•™)
CREATE TABLE IF NOT EXISTS stats_daily (
    bucket          INTEGER NOT NULL,
    model           TEXT NOT NULL,
    proxy_id        INTEGER NOT NULL,
    requests        INTEGER NOT NULL DEFAULT 0,
    successes       INTEGER NOT NULL DEFAULT 0,
    failures        INTEGER NOT NULL DEFAULT 0,
    total_ms        INTEGER NOT NULL DEFAULT 0,
    p50_ms          INTEGER NOT NULL DEFAULT 0,
    p95_ms          INTEGER NOT NULL DEFAULT 0,
    prompt_tokens   INTEGER NOT NULL DEFAULT 0,
    output_tokens   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (bucket, model, proxy_id)
);
CREATE INDEX IF NOT EXISTS idx_daily_bucket ON stats_daily(bucket);

CREATE TABLE IF NOT EXISTS sessions (
    token        TEXT PRIMARY KEY,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS kv (
    k TEXT PRIMARY KEY,
    v TEXT
);

-- Cookie æ± ï¼šæ¯è¡Œä¸€ä¸ª Google ç™»å½•æ€è´¦å·ï¼ˆä¸€æ•´ä¸² gemini.google.com cookieï¼‰ã€‚
-- è¯·æ±‚æ—¶æŒ‰ last_used_at æœ€ä¹…ä¼˜å…ˆæŒ‘ä¸€ä¸ª enabled çš„ï¼Œå¤©ç„¶è½®è½¬ + åˆ†æ•£å• IP ä¸Šé™ã€‚
CREATE TABLE IF NOT EXISTS accounts (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    label         TEXT NOT NULL DEFAULT '',      -- ç”¨æˆ·å¯å‘½åï¼ˆä¸€èˆ¬å¡«é‚®ç®±ï¼‰
    cookie        TEXT NOT NULL,                 -- å®Œæ•´ cookie ä¸² "k=v; k=v"
    status        TEXT NOT NULL DEFAULT 'enabled', -- enabled | disabled
    note          TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    last_used_at  INTEGER NOT NULL DEFAULT 0,     -- ä¸Šæ¬¡è¢«æŒ‘ä¸­å‘è¯·æ±‚çš„æ—¶åˆ»
    last_ok_at    INTEGER NOT NULL DEFAULT 0,     -- ä¸Šæ¬¡è¯·æ±‚æˆåŠŸçš„æ—¶åˆ»
    last_error    TEXT NOT NULL DEFAULT '',
    fail_count    INTEGER NOT NULL DEFAULT 0,     -- è¿žç»­å¤±è´¥æ¬¡æ•°ï¼ˆæˆåŠŸå½’é›¶ï¼‰
    proxy_id      INTEGER NOT NULL DEFAULT 0,     -- ç»‘å®šçš„å‡ºå£ï¼Œ0 = è¿˜æ²¡ç»‘
    auth_user     INTEGER NOT NULL DEFAULT 0      -- åŒä¸€æµè§ˆå™¨ profile å†…çš„è´¦å·åºå·ï¼ˆauthuserï¼‰
);
CREATE INDEX IF NOT EXISTS idx_accounts_pick ON accounts(status, last_used_at);

-- Hasil probe kapabilitas akun (ESY5D). Disimpan supaya routing model sudah
-- tahu akun mana yang benar-benar punya akses ke sebuah model sejak boot,
-- tanpa menunggu probe pertama selesai.
CREATE TABLE IF NOT EXISTS account_caps (
    account_id   INTEGER PRIMARY KEY,
    tier         TEXT NOT NULL DEFAULT 'unknown',
    caps         TEXT NOT NULL DEFAULT '',
    rollouts     TEXT NOT NULL DEFAULT '',
    quota_image  INTEGER NOT NULL DEFAULT -1,
    quota_music  INTEGER NOT NULL DEFAULT -1,
    quota_think  INTEGER NOT NULL DEFAULT -1,
    quota_video  INTEGER NOT NULL DEFAULT -1,
    alive        INTEGER NOT NULL DEFAULT 0,
    err          TEXT NOT NULL DEFAULT '',
    checked_at   INTEGER NOT NULL DEFAULT 0
);
`

func getDB() *dbx {
	dbOnce.Do(func() {
		var driver, connStr, sqliteDir string
		if dsnEnv := strings.TrimSpace(os.Getenv("SQL_DSN")); dsnEnv != "" {
			curDialect, driver, connStr = detectDSN(dsnEnv)
		} else {
			curDialect = dialectSQLite
			driver = "sqlite"
			path := cfg.DBPath
			if path == "" {
				path = "./data/gemini.db"
			}
			sqliteDir = filepath.Dir(path)
			if err := os.MkdirAll(sqliteDir, 0o755); err != nil {
				fmt.Fprintf(os.Stderr, "[db] å»ºç›®å½• %s å¤±è´¥: %v\n%s", sqliteDir, err, dbPermHint(sqliteDir))
				os.Exit(1)
			}
			connStr = fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
		}
		conn, err := sql.Open(driver, connStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[db] open failed: %v\n", err)
			os.Exit(1)
		}
		conn.SetMaxOpenConns(8)
		conn.SetMaxIdleConns(4)
		conn.SetConnMaxLifetime(0)
		// mysql/pg é©±åŠ¨é»˜è®¤ä¸æŽ¥å—å¤šè¯­å¥ Execï¼ŒæŒ‰æ–¹è¨€é€æ¡å»ºè¡¨ã€‚
		for _, stmt := range schemaStatements(curDialect) {
			if _, err := conn.Exec(stmt); err != nil {
				// mysql çš„ CREATE INDEX æ²¡æœ‰ IF NOT EXISTSï¼Œé‡å¯æ—¶é‡å¤æŠ¥é”™ï¼Œå®¹å¿ã€‚
				if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(stmt)), "CREATE INDEX") {
					continue
				}
				// sqlite æœ€å¸¸è§ SQLITE_CANTOPEN(14)ï¼šç›®å½•èƒ½è¿›ä½†å†™ä¸äº†ï¼Œåˆ¤æ®+è§£æ³•ä¸€èµ·æ‰“ã€‚
				hint := ""
				if curDialect == dialectSQLite {
					hint = dbPermHint(sqliteDir)
				}
				fmt.Fprintf(os.Stderr, "[db] åˆå§‹åŒ–å¤±è´¥: %v\n  è¯­å¥: %s\n%s", err, stmt, hint)
				os.Exit(1)
			}
		}
		// è€ sqlite åº“çš„åˆ—è¿ç§»ã€‚æ–°åº“/mysql/pg ä¸éœ€è¦ï¼ˆè·‘äº†ä¹Ÿåªæ˜¯æŠ¥é”™å¿½ç•¥ï¼‰ï¼Œåªåœ¨ sqlite ä¸Šåšã€‚
		if curDialect == dialectSQLite {
			_, _ = conn.Exec(`ALTER TABLE requests ADD COLUMN upstream_model TEXT`)
			_, _ = conn.Exec(`ALTER TABLE requests ADD COLUMN account_id INTEGER`)
			_, _ = conn.Exec(`ALTER TABLE requests ADD COLUMN account_label TEXT`)
			_, _ = conn.Exec(`ALTER TABLE accounts ADD COLUMN proxy_id INTEGER NOT NULL DEFAULT 0`)
			// rotate_disabled=1 berarti pemilik sesi ini adalah browser, bukan
			// NovaGemini. Rotasi ganda membatalkan token satu sama lain, jadi
			// akun seperti ini harus dibiarkan tanpa rotate dan cookienya
			// diperbarui lewat /admin/api/cookies (alat sync).
			_, _ = conn.Exec(`ALTER TABLE accounts ADD COLUMN rotate_disabled INTEGER NOT NULL DEFAULT 0`)
			// auth_user: satu profil browser bisa memegang beberapa akun Google
			// sekaligus (multi-login). Semua akun itu berbagi SATU set cookie;
			// yang membedakan hanya indeks `authuser` di URL/header. Tanpa kolom
			// ini akun kedua dan seterusnya akan selalu dilayani sebagai akun
			// indeks 0 â€” cookie benar, akun salah.
			_, _ = conn.Exec(`ALTER TABLE accounts ADD COLUMN auth_user INTEGER NOT NULL DEFAULT 0`)
			// tier_override: probe hanya membaca apa yang dikirim Google. Kalau
			// payload tidak mencantumkan cap berbayar sementara langganannya
			// aktif, operator bisa menetapkan tier sendiri dan probe tidak akan
			// menimpanya.
			_, _ = conn.Exec(`ALTER TABLE accounts ADD COLUMN tier_override TEXT NOT NULL DEFAULT ''`)
			_, _ = conn.Exec(`ALTER TABLE requests DROP COLUMN prompt_preview`)
			_, _ = conn.Exec(`ALTER TABLE requests DROP COLUMN response_preview`)
		}
		db = conn
		dbw = &dbx{conn}
		logf("[db] opened dialect=%d driver=%s", curDialect, driver)
	})
	return dbw
}

// Request rows â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

type RequestRow struct {
	ID            int64  `json:"id"`
	TS            int64  `json:"ts"`
	Model         string `json:"model"`
	UpstreamModel string `json:"upstream_model"`
	ProxyID       *int64 `json:"proxy_id"`
	ProxyName     string `json:"proxy_name"`
	AccountID     *int64 `json:"account_id"`
	AccountLabel  string `json:"account_label"`
	Status        int    `json:"status"`
	Error         string `json:"error"`
	TTFBMs        *int64 `json:"ttfb_ms"`
	TotalMs       int64  `json:"total_ms"`
	PromptChars   int    `json:"prompt_chars"`
	ResponseChars int    `json:"response_chars"`
	PromptTokens  int    `json:"prompt_tokens"`
	OutputTokens  int    `json:"output_tokens"`
	Endpoint      string `json:"endpoint"`
	Stream        int    `json:"stream"`
}

func insertRequest(r *RequestRow) {
	_, err := getDB().Exec(`INSERT INTO requests
        (ts, model, upstream_model, proxy_id, proxy_name, account_id, account_label,
         status, error, ttfb_ms, total_ms,
         prompt_chars, response_chars, prompt_tokens, output_tokens,
         endpoint, stream)
        VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.TS, r.Model, r.UpstreamModel, r.ProxyID, r.ProxyName, r.AccountID, r.AccountLabel,
		r.Status, r.Error, r.TTFBMs, r.TotalMs,
		r.PromptChars, r.ResponseChars, r.PromptTokens, r.OutputTokens,
		r.Endpoint, r.Stream)
	if err != nil {
		logf("[db] insert request failed: %v", err)
	}
}

// Sessions â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

func createSession(token string, ttl time.Duration) {
	now := time.Now().Unix()
	_, err := getDB().Exec(upsert("sessions", []string{"token", "created_at", "expires_at"}, []string{"token"}),
		token, now, now+int64(ttl.Seconds()))
	if err != nil {
		logf("[db] session insert failed: %v", err)
	}
}

func validSession(token string) bool {
	if token == "" {
		return false
	}
	var exp int64
	err := getDB().QueryRow(`SELECT expires_at FROM sessions WHERE token=?`, token).Scan(&exp)
	if err != nil {
		return false
	}
	return time.Now().Unix() < exp
}

// KV helpers â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

func kvGet(k string) string {
	var v string
	if err := getDB().QueryRow(`SELECT v FROM kv WHERE k=?`, k).Scan(&v); err != nil {
		return ""
	}
	return v
}

func kvSet(k, v string) error {
	_, err := getDB().Exec(upsert("kv", []string{"k", "v"}, []string{"k"}), k, v)
	return err
}

// dbPermHint åœ¨æ•°æ®åº“æ‰“ä¸å¼€æ—¶ç»™å‡ºå¯æ“ä½œçš„æç¤ºã€‚
//
// åˆ¤æ®ï¼šå®¹å™¨é•œåƒæ˜¯ distroless + USER nonrootï¼ˆuid 65532ï¼‰ï¼Œè€Œ bind mount ä¼šç”¨å®¿ä¸»
// ç›®å½•çš„å±žä¸»æ•´ä¸ªç›–æŽ‰é•œåƒé‡Œ /data çš„å±žä¸»ã€‚å®¿ä¸»ä¸Šç›®å½•ä¸å­˜åœ¨æ—¶ Docker è‡ªåŠ¨å»ºã€å±žä¸»æ˜¯
// rootï¼ŒäºŽæ˜¯å®¹å™¨é‡Œçš„ 65532 å†™ä¸è¿›åŽ» â€”â€” å®žæµ‹è¿™å°±æ˜¯ `unable to open database file (14)`
// çš„æˆå› ï¼ˆåŒä¸€æ¡å‘½ä»¤åªæŠŠå±žä¸» chown æˆ 65532 å°±èƒ½èµ·æ¥ï¼‰ã€‚
func dbPermHint(dir string) string {
	uid := os.Getuid() // Windows ä¸Šè¿”å›ž -1ï¼Œé‚£è¾¹ä¸æ¶‰åŠè¿™ä¸ªé—®é¢˜
	return fmt.Sprintf(""+
		"      å½“å‰è¿›ç¨‹ uid=%dï¼Œå†™ä¸è¿›ç›®å½• %sã€‚\n"+
		"      Docker éƒ¨ç½²æœ€å¸¸è§çš„åŽŸå› æ˜¯ bind mount çš„å®¿ä¸»ç›®å½•å±žä¸»æ˜¯ rootï¼Œ\n"+
		"      è€Œé•œåƒä»¥ nonroot(65532) è¿è¡Œã€‚ä¸¤ç§è§£æ³•ï¼š\n"+
		"        1) æ”¹ç”¨å…·åå·ï¼ˆæŽ¨èï¼‰ï¼švolumes å†™ gw2a-data:/data\n"+
		"        2) ä¿ç•™ bind mount å°±æŠŠå±žä¸»æ”¹è¿‡æ¥ï¼šsudo chown -R 65532:65532 ./data\n",
		uid, dir)
}

