package app

import (
	"encoding/json"
	"os"
	"sync"
)

type Config struct {
	Port int `json:"port"`
	// AltPorts adalah port tambahan opsional yang dilayani proses yang sama
	// (handler identik). Default KOSONG, dan itu disengaja.
	//
	// Riwayatnya: fitur ini dibuat untuk mengantisipasi 9router yang menyimpan
	// alamat provider di konfigurasinya sendiri (gw2a → http://127.0.0.1:8083/v1),
	// sehingga pindah port membuat semua model gw2a/* terlihat mati. Tapi
	// mendengarkan port kedua secara diam-diam justru lebih berbahaya: di mesin
	// ini 8090 sudah dipakai dashboard lain (D:\Labs\novacf\admin.py), dan
	// pendengar kita menyerobotnya sampai dashboard itu tampak hilang.
	//
	// Jadi: jangan pernah menebak port. Isi hanya kalau memang tahu port itu
	// kosong. Kalau kosong, hanya Port yang dilayani.
	AltPorts       []int  `json:"alt_ports"`
	Host           string `json:"host"`
	RetryAttempts  int    `json:"retry_attempts"`
	RetryDelaySec  int    `json:"retry_delay_sec"`
	RequestTimeout int    `json:"request_timeout_sec"`
	GeminiBL       string `json:"gemini_bl"`
	DefaultModel   string `json:"default_model"`
	LogRequests    bool   `json:"log_requests"`
	CookieFile     string `json:"cookie_file"`
	Proxy          string `json:"proxy"` // 只用于启动时播种代理池，见 seedProxiesFromConfig
	Impersonate    string `json:"impersonate"`
	DBPath         string `json:"db_path"`
	AdminToken     string `json:"admin_token"`
	AdminEnabled   bool   `json:"admin_enabled"`
	RetentionDays  int    `json:"retention_days"`

	// Per-IP rate limit (一个 slot = 直连/或一个代理),0 表示不限。
	// 默认值取实测区间 80-180 的下沿，详见 ratelimit.go 的说明。
	PerIPConcurrent int `json:"per_ip_concurrent"` // 瞬时并发上限
	PerIPRPM        int `json:"per_ip_rpm"`        // 每分钟请求上限
	PerIPRPH        int `json:"per_ip_rph"`        // 每小时请求上限

	// 代理连续失败熔断后隔多久放回池子（分钟），0 = 不恢复。
	ProxyCooldownMin int `json:"proxy_cooldown_min"`
	// 代理池无可用出口时是否退回直连。默认 false。
	FallbackDirect bool `json:"fallback_direct"`
	// cookie 失效时是否降级成匿名继续跑。默认 false。
	FallbackAnon bool `json:"fallback_anon"`
	// 是否自动从 /app 页面抓最新 bl 版本号。默认 true。
	GeminiBLAuto bool `json:"gemini_bl_auto"`
	// 单次请求 prompt 的 UTF-8 字节上限，0 = 不限。
	MaxPromptBytes int `json:"max_prompt_bytes"`
	// DenyWaitBudgetSec: kalau semua kandidat akun untuk sebuah model sedang
	// cooldown, tunggu sisa cooldown terdekat sampai batas ini sebelum menyerah.
	// 0 = tidak pernah menunggu (perilaku lama: tolak instan). Default 10.
	DenyWaitBudgetSec int `json:"deny_wait_budget_sec"`
	// ModelDenyTTLSec: cooldown per (akun, model) setelah Google menyajikan model
	// lain. Default 120: cukup melewati lonjakan, cukup pendek supaya model yang
	// hanya dimiliki sedikit akun (3.8 Flash, Argon) tidak mati 5 menit penuh.
	ModelDenyTTLSec int `json:"model_deny_ttl_sec"`
	// 是否走 Gemini 原生 conversation_id 服务端多轮。默认 false，见 conversation.go。
	MultiTurn bool `json:"multi_turn"`
	// 出完结果是否自动删掉 gemini.google.com 上留下的这条会话（#19，rpc GzXR5e）。
	// 只登录态生效（删除要 XSRF）；异步 best-effort，删失败只记日志不影响响应。默认 false。
	AutoDeleteConversation bool `json:"auto_delete_conversation"`
	// 匿名优先（#20）：这次请求不需要登录态能力（纯文本、非思考、无工具、无图）时
	// 不占用 cookie 账号，走匿名省额度；需要登录才挑号。默认 false。见 modelNeedsLogin。
	AnonFirst bool `json:"anon_first"`
	// 临时对话（#34）：发请求时塞 inner[45]=1，走 Gemini 网页端原生「临时对话」——
	// 会话不进账号的会话列表、不用于训练，天然取代 auto_delete_conversation（无需再发删除
	// RPC）。只登录态有意义（匿名本就不落账号）。与 multi_turn 冲突：multi_turn 开着时不发，
	// 见 temporaryChatEligible。默认 false。
	TemporaryChat bool `json:"temporary_chat"`
	// SaveMediaToDisk: simpan artefak besar (music/video) ke data/media, kirim URL.
	SaveMediaToDisk bool `json:"save_media_to_disk"`
	// MediaInlineKB: ambang base64 vs file (KB). 0 = default 1024.
	MediaInlineKB int `json:"media_inline_kb"`
}

var (
	cfg     Config
	cfgOnce sync.Once
)

func defaultConfig() Config {
	return Config{
		SaveMediaToDisk: false,
		MediaInlineKB:   1024,
		Port:            8083,
		AltPorts:        nil,
		Host:            "0.0.0.0",
		RetryAttempts:   3,
		RetryDelaySec:   2,
		RequestTimeout:  180,
		GeminiBL:        "boq_gemini-web-uiserver_20261007.01_p0",
		DefaultModel:    "gemini-3.6-flash",
		LogRequests:     true,
		CookieFile:      "",
		Proxy:           "",
		Impersonate:     "chrome_146",
		DBPath:          "./data/gemini.db",
		AdminToken:      "",
		AdminEnabled:    true,
		RetentionDays:   30,
		// 单出口实测 80-180 次不等（连接策略和出口质量决定），静态 IP 上 188。
		// RPH 取下沿 80 保守留量；按低速率跑的部署可以调高很多 ——
		// 10 次/分钟连打 800 次、跨 110 分钟一次没被拦。
		PerIPConcurrent: 5,
		PerIPRPM:        30,
		PerIPRPH:        80,
		// 实测被拦的出口 106-121 分钟自动恢复，冷却取 120 分钟。
		ProxyCooldownMin: 120,
		FallbackDirect:   false,
		FallbackAnon:     false,
		GeminiBLAuto:     true,
		// 实测上游的墙在约 13 万 UTF-8 字节：129,950 字节中英文各 3/3 过，
		// 135,990 字节各 1/3，141,920 字节各 1/3。取 128000 留一点余量。
		MaxPromptBytes:         128000,
		DenyWaitBudgetSec:      10,
		ModelDenyTTLSec:        120,
		MultiTurn:              false,
		AutoDeleteConversation: false,
		AnonFirst:              false,
		TemporaryChat:          false,
	}
}

func loadConfig(path string) error {
	cfgPath = path
	cfg = defaultConfig()
	if path == "" {
		for _, p := range []string{"./config.json", os.ExpandEnv("$HOME/.config/novagemini/config.json")} {
			if _, err := os.Stat(p); err == nil {
				path = p
				break
			}
		}
	}
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &cfg)
}

// cfgPath diisi oleh loadConfig supaya perubahan dari panel bisa ditulis balik
// ke file yang sama (mis. bind address).
var cfgPath string

func saveConfigFile() error {
	// loadConfig dipanggil dengan path kosong saat memakai default.
	path := cfgPath
	if path == "" {
		path = "config.json"
	}
	// Baca file yang ada dulu, lalu timpa HANYA field yang memang diubah di
	// memori. Menulis seluruh struct akan mengembalikan nilai yang datang dari
	// kv (mis. save_media_to_disk) ke default, dan itu pernah terjadi.
	merged := map[string]interface{}{}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &merged)
	}
	current, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	cur := map[string]interface{}{}
	if err := json.Unmarshal(current, &cur); err != nil {
		return err
	}
	for k, v := range cur {
		merged[k] = v
	}
	b, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0644)
}
