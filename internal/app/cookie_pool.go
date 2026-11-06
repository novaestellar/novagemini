package app

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CookieAccount 是 cookie 池里的一行：一个 Google 登录态账号。
type CookieAccount struct {
	ID         int64  `json:"id"`
	Label      string `json:"label"`
	Cookie     string `json:"cookie"` // 完整串，API 层按需脱敏后再返回
	Status     string `json:"status"`
	Note       string `json:"note"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt int64  `json:"last_used_at"`
	LastOkAt   int64  `json:"last_ok_at"`
	LastError  string `json:"last_error"`
	FailCount  int64  `json:"fail_count"`
	ProxyID    int64  `json:"proxy_id"` // 绑定的出口，0 = 还没绑

	// RotateDisabled 标记会话归浏览器所有。rotateAllAccounts 会跳过它：
	// 两个客户端同时刷新 __Secure-1PSIDTS 会互相作废旧 token，症状就是
	// "no SNlM0e" —— 健康的账号看起来像死了。这类账号的 cookie 由
	// cookie_grab sync 定期覆盖，不由本服务续期。
	RotateDisabled int64 `json:"rotate_disabled"`
	// Impersonate: sidik jari TLS/HTTP2 yang dipakai untuk akun ini. Kalau
	// kosong, pakai nilai global. Sesi yang lahir dari Firefox harus dikirim
	// dengan sidik jari Firefox; memakai chrome_146 untuk cookie Firefox
	// membuat Google mencabut sesinya dalam hitungan jam.
	Impersonate string `json:"impersonate"`

	// AuthUser 是同一个浏览器 profile 里 Google 多账号登录的序号，0 = 主账号。
	// Satu profil bisa memegang belasan akun dengan SATU set cookie; yang
	// membedakan hanya nilai ini, yang dikirim sebagai `authuser` di URL dan
	// `X-Goog-AuthUser` di header. Tanpa ini semua akun dari profil yang sama
	// akan dilayani sebagai akun indeks 0.
	AuthUser int64 `json:"auth_user"`

	// TierOverride: "pro"/"free" yang ditetapkan operator. Kosong = ikut probe.
	TierOverride string `json:"tier_override"`

	// CookieSource: dari mana cookie akun ini berasal, mis. "firefox:animainventi".
	// Hanya diisi oleh alat sync (cookie_grab.py) karena hanya dia yang tahu
	// profil mana yang dipakai. Kalau terisi DAN akun milik browser, mirror
	// berkala (browser_mirror.go) menyegarkan salinan server dari profil itu -
	// membaca, bukan memutar, jadi tetap satu pemilik sesi.
	CookieSource string `json:"cookie_source"`
	// CookieSID: sidik jari SID dari profil yang cookie_source tunjuk. Cermin
	// profil (browser_mirror.go) menolak menimpa kalau SID yang dibaca dari
	// profil tidak cocok - jadi cookie_source yang salah ketik tidak bisa
	// menukar identitas akun. Kosong = belum diverifikasi, cermin dilewati.
	CookieSID string `json:"cookie_sid"`
}

// splitCookiePairs 把 "k=v; k=v" 拆成键值对。
//
// 按 ";" 切再逐段 TrimSpace，不按 "; " 切：从 DevTools 复制出来的串不一定带空格，
// 而按 "; " 切的话 "SID=a;SAPISID=b" 会整段当成一个键 —— 于是 extractSAPISID
// 取不到值、不发 Authorization 头、请求被上游当匿名处理，用户毫不知情。
//
// 值里可能含 "="（base64 补位），所以只在第一个等号处切。
func splitCookiePairs(cookie string) [][2]string {
	var out [][2]string
	for _, p := range strings.Split(cookie, ";") {
		p = strings.TrimSpace(p)
		if i := strings.Index(p, "="); i > 0 {
			out = append(out, [2]string{p[:i], p[i+1:]})
		}
	}
	return out
}

// cookieValue 从一整串 cookie 里取指定名字的值，取不到返回空串。
func cookieValue(cookie, name string) string {
	for _, kv := range splitCookiePairs(cookie) {
		if kv[0] == name {
			return kv[1]
		}
	}
	return ""
}

// extractSAPISID 从一整串 cookie 里取 SAPISID 的值，取不到返回空串。
func extractSAPISID(cookie string) string {
	return cookieValue(cookie, "SAPISID")
}

// cookieSubset 只留下指定名字的项，顺序跟原串一致。刷新 1PSIDTS 时不能把整串
// 都带上：多带 Chrome DBSC / 其它主机的 cookie 实测会让 RotateCookies 回 401。
func cookieSubset(cookie string, names []string) string {
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	var parts []string
	seen := map[string]bool{}
	for _, kv := range splitCookiePairs(cookie) {
		if !want[kv[0]] || kv[1] == "" || seen[kv[0]] {
			continue
		}
		seen[kv[0]] = true
		parts = append(parts, kv[0]+"="+kv[1])
	}
	return strings.Join(parts, "; ")
}

// cookieNames 返回 cookie 串里出现的所有 cookie 名（顺序保留），供 UI 展示。
func cookieNames(cookie string) []string {
	var names []string
	for _, kv := range splitCookiePairs(cookie) {
		names = append(names, kv[0])
	}
	return names
}

// accountAdd 往池里插一条。cookie 必须能取到非空的 SAPISID。
// 这是面板/API 手工添加走的路，用户当场看得到错误提示，拦下来是对的。
//
// 判据是**取不取得到值**，不是"字符串里出没出现过 SAPISID"：后者会把
// 一份没填的逐项模板（键名在、值全空）放进池子，存成一整段 JSON，
// 之后每次轮到它都注定失败。
func accountAdd(label, cookie, note string) (int64, error) {
	c, ok := normalizeCookie(cookie, "手工添加的 cookie")
	if !ok {
		return 0, fmt.Errorf("cookie 解析失败：既不是 \"k=v; k=v\" 串，也不是可识别的 JSON")
	}
	if extractSAPISID(c) == "" {
		return 0, fmt.Errorf("cookie 里没有 SAPISID，多半没复制全（需要 gemini.google.com 下的完整 cookie，至少含 SID / HSID / SSID / APISID / SAPISID / __Secure-1PSID）")
	}
	return accountInsert(label, c, note)
}

// accountAdopt 是启动时导入既有配置专用的：**不做 SAPISID 检查**。
//
// 判据是「升级不能改变用户已有配置的可用性」。缺 SAPISID 的 cookie 在旧版是被
// 原样当 Cookie 头发出去的（只是算不出 SAPISIDHASH 授权头），能不能用由上游说了算 ——
// 不该在升级时被我们新加的校验拦下来，让用户悄无声息地退回匿名。只警告，
// 健康度会在面板上如实体现。
func accountAdopt(label, cookie, note string) (int64, error) {
	cookie, ok := normalizeCookie(cookie, label)
	if !ok {
		return 0, fmt.Errorf("cookie 解析失败")
	}
	if extractSAPISID(cookie) == "" {
		logf("[cookie] %s 里没有 SAPISID，算不出 SAPISIDHASH 授权头，可能只当匿名处理；"+
			"先按原样导入，请到面板核对", label)
	}
	return accountInsert(label, cookie, note)
}

func accountInsert(label, cookie, note string) (int64, error) {
	cookie = strings.TrimSpace(cookie)
	if cookie == "" {
		return 0, fmt.Errorf("cookie 不能为空")
	}
	return insertID(
		`INSERT INTO accounts(label, cookie, status, note, created_at) VALUES (?,?,?,?,?)`,
		strings.TrimSpace(label), cookie, "enabled", strings.TrimSpace(note), time.Now().Unix())
}

// accountList 返回池里全部账号，按 id 升序。
func accountList() []CookieAccount {
	rows, err := getDB().Query(
		`SELECT id, label, cookie, status, note, created_at, last_used_at, last_ok_at, last_error, fail_count, proxy_id, rotate_disabled, auth_user, tier_override, impersonate, cookie_source, cookie_sid
		 FROM accounts ORDER BY id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []CookieAccount
	for rows.Next() {
		var a CookieAccount
		if err := rows.Scan(&a.ID, &a.Label, &a.Cookie, &a.Status, &a.Note,
			&a.CreatedAt, &a.LastUsedAt, &a.LastOkAt, &a.LastError, &a.FailCount,
			&a.ProxyID, &a.RotateDisabled, &a.AuthUser, &a.TierOverride, &a.Impersonate, &a.CookieSource, &a.CookieSID); err != nil {
			continue
		}
		out = append(out, a)
	}
	return out
}

// accountByID 按 id 取一条，取不到返回 nil。
//
// 轮转会把新 cookie 写回库里，之后要用**库里那份**继续发请求 —— 手上那个
// CookieAccount 是轮转之前的快照，接着用等于把刚刷新的值扔掉。
func accountByID(id int64) *CookieAccount {
	var a CookieAccount
	err := getDB().QueryRow(
		`SELECT id, label, cookie, status, note, created_at, last_used_at, last_ok_at, last_error, fail_count, proxy_id, rotate_disabled, auth_user, tier_override, impersonate, cookie_source, cookie_sid
		 FROM accounts WHERE id=?`, id).
		Scan(&a.ID, &a.Label, &a.Cookie, &a.Status, &a.Note,
			&a.CreatedAt, &a.LastUsedAt, &a.LastOkAt, &a.LastError, &a.FailCount,
			&a.ProxyID, &a.RotateDisabled, &a.AuthUser, &a.TierOverride, &a.Impersonate, &a.CookieSource, &a.CookieSID)
	if err != nil {
		return nil
	}
	return &a
}

// accountDelete 删除一条。
func accountDelete(id int64) error {
	_, err := getDB().Exec(`DELETE FROM accounts WHERE id=?`, id)
	return err
}

// accountSetStatus 改状态（enabled / disabled）。
func accountSetStatus(id int64, status string) error {
	if status != "enabled" && status != "disabled" {
		return fmt.Errorf("非法状态 %q", status)
	}
	_, err := getDB().Exec(`UPDATE accounts SET status=? WHERE id=?`, status, id)
	return err
}

// accountUpdateMeta 改 label / note（不动 cookie 本身）。
func accountUpdateMeta(id int64, label, note string) error {
	_, err := getDB().Exec(`UPDATE accounts SET label=?, note=? WHERE id=?`,
		strings.TrimSpace(label), strings.TrimSpace(note), id)
	return err
}

// accountSetCookie menimpa cookie sebuah akun dengan sesi yang baru diekspor.
//
// Dipakai alat sync: browser pemilik sesi, NovaGemini hanya memakai salinan
// terbarunya. Status ikut dipulihkan dan fail_count dinolkan — cookie baru
// adalah bukti hidup yang lebih kuat daripada hitungan kegagalan lama, dan
// tanpa reset akun akan tetap tersaring walaupun sesinya sehat.
//
// last_ok_at sengaja TIDAK diisi: nilai itu berarti "permintaan nyata berhasil",
// bukan "cookie baru dipasang".
func accountSetCookie(id int64, cookie string) error {
	c, ok := normalizeCookie(cookie, "sync dari browser")
	if !ok {
		return fmt.Errorf("cookie tidak bisa diparse")
	}
	if extractSAPISID(c) == "" {
		return fmt.Errorf("cookie tanpa SAPISID — tidak lengkap")
	}
	// Penanda authuser harus ikut dipasang lagi: sync dari browser mengirim
	// cookie polos (tanpa penanda), dan tanpa ini akun indeks >0 akan turun
	// jadi indeks 0 — cookie benar, akun salah.
	var au int64
	_ = getDB().QueryRow(`SELECT auth_user FROM accounts WHERE id=?`, id).Scan(&au)
	c = withAuthUserMarker(c, au)
	_, err := getDB().Exec(
		`UPDATE accounts SET cookie=?, status='enabled', fail_count=0, last_error='' WHERE id=?`,
		c, id)
	invalidateAuthUserCache()
	invalidateXSRF(c)
	return err
}

// accountSetTierOverride menyimpan tier pilihan operator ("pro"/"free"/kosong).
// accountSetCookieSource mencatat profil browser asal cookie akun ini, sekaligus
// sidik jari SID dari cookie yang sedang tersimpan. Dipakai alat sync supaya
// mirror berkala tahu file mana yang harus dibaca - dan bisa memastikan profil
// itu memang masih memegang akun yang sama.
func accountSetCookieSource(id int64, src string) error {
	a := accountByID(id)
	if a == nil {
		return fmt.Errorf("akun #%d tidak ada", id)
	}
	_, err := getDB().Exec(`UPDATE accounts SET cookie_source=?, cookie_sid=? WHERE id=?`,
		src, cookieValue(a.Cookie, "SID"), id)
	return err
}

func accountSetTierOverride(id int64, tier string) error {
	tier = strings.ToLower(strings.TrimSpace(tier))
	if tier != "" && tier != "pro" && tier != "free" {
		return fmt.Errorf("tier harus pro, free, atau kosong")
	}
	_, err := getDB().Exec(`UPDATE accounts SET tier_override=? WHERE id=?`, tier, id)
	return err
}

// accountSetRotateDisabled menandai akun sebagai milik browser (true) atau
// milik NovaGemini (false). Lihat CookieAccount.RotateDisabled.
func accountSetRotateDisabled(id int64, disabled bool) error {
	v := 0
	if disabled {
		v = 1
	}
	_, err := getDB().Exec(`UPDATE accounts SET rotate_disabled=? WHERE id=?`, v, id)
	return err
}

// accountCount 返回 (总数, enabled 数)。
func accountCount() (int, int) {
	var total, enabled int
	_ = getDB().QueryRow(`SELECT COUNT(*), SUM(CASE WHEN status='enabled' THEN 1 ELSE 0 END) FROM accounts`).
		Scan(&total, &enabled)
	return total, enabled
}

// kv 里记迁移/播种状态的键。
const (
	kvLegacyCookieDone = "legacy_single_cookie_migrated"
	kvSeededCookieID   = "seeded_cookie_id"
	kvSeededCookieVal  = "seeded_cookie_value"
)

// seedCookiesFromConfig 把启动参数和历史遗留的单 cookie 并进 cookie 池。
//
// cookie 原来也有两个入口：cookie 池，和「设置」页那个池空时才用的单 cookie 输入
// （值存 kv 的 google_cookie，或来自 --cookie-file）。跟静态代理同一个毛病 ——
// 单 cookie 路径返回的账号 ID 是 0，而 markAccountResult 开头就是 id<=0 直接返回，
// 于是**健康度一个字都不写**：fail_count 恒为 0、last_ok_at 恒为空，也没有轮转。
//
//   - kv 里的 google_cookie：一次性迁入，用独立标记记"迁过了"，原值不动
//   - cfg.CookieFile（--cookie-file）：声明式跟随，内容变了替换同一条记录
func seedCookiesFromConfig() {
	migrateLegacyCookie()
	syncSeededCookieFile()
}

// migrateLegacyCookie 一次性把 kv 里遗留的单 cookie 搬进池子。
// 三条保命规则：走 accountAdopt 不做 SAPISID 校验（旧路径不校验，升级不该改变
// 可用性）、入池成功才标记完成、不去动 kv 里原来的值（回滚到旧版仍可用）。
func migrateLegacyCookie() {
	if kvGet(kvLegacyCookieDone) == "1" {
		return
	}
	raw := strings.TrimSpace(kvGet("google_cookie"))
	if raw == "" {
		_ = kvSet(kvLegacyCookieDone, "1")
		return
	}
	cookie, ok := normalizeCookie(raw, "「设置」页的单 cookie")
	if !ok {
		return
	}
	if !poolHasCookie(cookie) {
		if _, err := accountAdopt("原单 cookie", cookie, "从「设置」页迁入"); err != nil {
			logf("[cookie] 「设置」页的单 cookie 迁入池子失败，原值保留、下次启动重试: %v", err)
			return
		}
		logf("[cookie] 「设置」页的单 cookie 已迁入 cookie 池")
	}
	_ = kvSet(kvLegacyCookieDone, "1")
}

// syncSeededCookieFile 让池子里跟着 --cookie-file 走一条记录。
//
// 跟 --proxy 同一套：文件内容变了就撤下旧的那条再建新的，而不是又加一条。
// 按内容去重挡不住这个 —— 定期轮换 cookie.txt 的部署会一次次往池子里堆死 cookie，
// 而它们仍然 enabled，仍然参与轮转，于是每 N 个请求就有一个注定失败。
// 内容没变时完全不碰池子，面板上的增删改停用都以面板为准。
func syncSeededCookieFile() {
	var cur string
	if cfg.CookieFile != "" {
		data, err := os.ReadFile(cfg.CookieFile)
		if err != nil {
			// 读不到就当没配过：宁可保持现状，也不能因为容器少挂一个卷
			// 就把用户在用的 cookie 撤下来。
			logf("[cookie] 读不了 --cookie-file %s，池子保持不变: %v", cfg.CookieFile, err)
			return
		}
		cur = strings.TrimSpace(string(data))
	}
	if cur != "" {
		if c, ok := normalizeCookie(cur, "--cookie-file"); ok {
			cur = c
		} else {
			return
		}
	}

	prev := kvGet(kvSeededCookieVal)
	if cur == prev {
		return
	}
	dropSeededCookie(prev)
	if cur == "" {
		_ = kvSet(kvSeededCookieVal, "")
		_ = kvSet(kvSeededCookieID, "")
		logf("[cookie] --cookie-file 已移除，对应的池子记录一并撤下")
		return
	}
	if poolHasCookie(cur) {
		_ = kvSet(kvSeededCookieVal, cur)
		_ = kvSet(kvSeededCookieID, "")
		return
	}
	id, err := accountAdopt("cookie-file", cur, "来自 --cookie-file")
	if err != nil {
		logf("[cookie] --cookie-file 入池失败: %v", err)
		return
	}
	_ = kvSet(kvSeededCookieVal, cur)
	_ = kvSet(kvSeededCookieID, strconv.FormatInt(id, 10))
	logf("[cookie] --cookie-file 的 cookie 已加入 cookie 池")
}

// dropSeededCookie 撤掉上一次由 --cookie-file 建的那条。
// 只在内容还是我们写进去的那份时才删——用户在面板改过就说明他接管了。
func dropSeededCookie(prevCookie string) {
	idStr := kvGet(kvSeededCookieID)
	if idStr == "" || prevCookie == "" {
		return
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		return
	}
	for _, a := range accountList() {
		if a.ID == id {
			if a.Cookie == prevCookie {
				if err := accountDelete(id); err != nil {
					logf("[cookie] 撤下旧的 --cookie-file 记录失败: %v", err)
				}
			}
			return
		}
	}
}

// 面板逐项填写模式列出的 cookie，同时也是拼串时的固定顺序。
// 顺序必须确定：同一份 cookie 若因键顺序不同拼出两种串，按内容去重就失效了。
var cookieTemplateOrder = []string{
	"SID", "HSID", "SSID", "APISID", "SAPISID", "__Secure-1PSID", "__Secure-1PSIDTS",
}

// normalizeCookie 把各种输入形态归一化成池子要的裸 "k=v; k=v" 串。
//
// 吃三种：
//   - 裸串，原样返回
//   - 旧单 cookie 路径的 {"cookie":"k=v; k=v","sapisid":"..."}
//   - 面板逐项模式的 {"SID":"a","SAPISID":"b",...}
//
// 不归一化的话池子里会存进一整段 JSON，SAPISID 提取和后续请求全错。
func normalizeCookie(raw, who string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "{") {
		return raw, true
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		logf("[cookie] %s 是 JSON 但解析失败，跳过: %v", who, err)
		return "", false
	}
	if c, ok := m["cookie"].(string); ok && strings.TrimSpace(c) != "" {
		return strings.TrimSpace(c), true
	}

	str := func(k string) string {
		s, _ := m[k].(string)
		return strings.TrimSpace(s)
	}
	var parts []string
	inTemplate := map[string]bool{}
	for _, k := range cookieTemplateOrder {
		inTemplate[k] = true
		if v := str(k); v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	// 用户从 DevTools 多复制几项进来不能丢，附在模板字段后面按名字排序
	var extra []string
	for k := range m {
		if !inTemplate[k] && k != "sapisid" && str(k) != "" {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		parts = append(parts, k+"="+str(k))
	}
	if len(parts) == 0 {
		logf("[cookie] %s 是 JSON 但一项非空值都没有，跳过", who)
		return "", false
	}
	return strings.Join(parts, "; "), true
}

func poolHasCookie(cookie string) bool {
	for _, a := range accountList() {
		if a.Cookie == cookie {
			return true
		}
	}
	return false
}

// pickMu 把「SELECT 最久未用的号 + UPDATE 标记它刚用过」串成原子操作。
// 不加锁时两个并发请求会 SELECT 到同一个"最久未用"的号、各自 UPDATE，于是同一瞬间
// 双双用它，轮转形同虚设（CLAUDE.md 记的已知缺陷）。本进程内一把锁就够——限流器、
// 轮转调度、代理池都是进程内状态，这个反代天生单实例，不存在跨进程并发挑号；也就
// 不必为此上跨方言的事务/行锁（sqlite 无 FOR UPDATE、mysql 无 RETURNING，那条路
// 全是方言分支）。挑号只是一次极快的 SELECT+UPDATE，串行化的争用可忽略。
var pickMu sync.Mutex

// pickCookieAccount 从池里挑一个 enabled 账号，按 last_used_at 最久优先，
// 挑中后立刻把 last_used_at 记为现在（下次轮到别人）。池空返回 (nil,false)。
func pickCookieAccount() (*CookieAccount, bool) {
	return pickCookieAccountExcept(nil)
}

// pickCookieAccountExcept 同上，但跳过本次已经试过的账号。
//
// 存在的理由：一个 cookie 失效不该让整个请求失败。池子里 2 个号坏 1 个，
// 轮转会让大约一半请求撞上坏号 —— 表现就是"成功率莫名其妙很低"，而每次失败
// 看起来都像是上游的问题。
func pickCookieAccountExcept(skip map[int64]bool) (*CookieAccount, bool) {
	return pickCookieAccountFor(ModelConfig{}, skip)
}

// modelRequiresPaidAccount menjawab apakah model ini HANYA bisa dilayani akun
// berbayar. Satu sumber kebenaran, dipakai pemilih akun.
//
// Terukur di mesin ini (2026-11-05): akun free meminta 3.8 Flash dan Google
// menurunkannya ke 3.6 Flash - jawaban 200 dengan model salah. Akun free meminta
// Argon diturunkan ke 3.5 Flash-Lite. Keduanya mustahil dibedakan klien, jadi
// akun free tidak boleh jadi kandidat sama sekali untuk model di daftar ini.
func modelRequiresPaidAccount(mc ModelConfig) bool {
	switch mc.HexID {
	case hexArgon4, hexPro31, hexFlash38:
		return true
	}
	return false
}

// modelDenyTTL adalah berapa lama satu pasangan (akun, model) dihindari setelah
// Google menjawab dengan model lain. Bisa diatur lewat runtime config
// (model_deny_ttl_sec); nilai <= 0 jatuh kembali ke default.
//
// Ini BUKAN penilaian "akun mati" — akunnya sehat, hanya tidak berhak atas model
// itu untuk sementara (paling sering: kuota preview harian habis). Tanpa cooldown,
// setiap request mengulang perjalanan 10-20 detik ke Google untuk hasil yang sama
// persis, dan klien hanya melihat 502 yang lambat.
func modelDenyTTL() time.Duration {
	sec := rtCfg().ModelDenyTTLSec
	if sec <= 0 {
		sec = 120
	}
	return time.Duration(sec) * time.Second
}

var (
	modelDenyMu sync.Mutex
	modelDeny   = map[string]int64{} // key -> unix expiry
)

func modelDenyKey(acctID int64, mc ModelConfig) string {
	// Varian dalam satu keluarga HARUS punya kunci sendiri. Dulu kuncinya hanya
	// (akun|hex|tool), sehingga `gemini-3.6-flash` dan `gemini-3.6-flash-thinking`
	// berbagi kunci yang sama: begitu Google menyajikan model lain untuk yang
	// biasa, varian thinking-nya ikut diblokir 5 menit dan permintaannya jatuh ke
	// jalur anonim — gejalanya "needs a signed-in PAID account" padahal akunnya
	// pro. Mode/Thinking/ThinkLevel ikut masuk kunci supaya varian terpisah.
	return fmt.Sprintf("%d|%s|%d|%t|%d|%d",
		acctID, mc.HexID, mc.Tool, mc.Thinking, mc.ThinkLevel, mc.Mode)
}

// markModelDenied mencatat bahwa akun ini baru saja disajikan model lain.
func markModelDenied(acctID int64, mc ModelConfig) {
	if acctID <= 0 || mc.HexID == "" {
		return
	}
	modelDenyMu.Lock()
	modelDeny[modelDenyKey(acctID, mc)] = time.Now().Add(modelDenyTTL()).Unix()
	modelDenyMu.Unlock()
}

// modelDenied melaporkan apakah akun ini sedang dihindari untuk model tersebut.
func modelDenied(acctID int64, mc ModelConfig) bool {
	if acctID <= 0 || mc.HexID == "" {
		return false
	}
	k := modelDenyKey(acctID, mc)
	modelDenyMu.Lock()
	defer modelDenyMu.Unlock()
	until, ok := modelDeny[k]
	if !ok {
		return false
	}
	if time.Now().Unix() >= until {
		delete(modelDeny, k)
		return false
	}
	return true
}

// modelDenyUntil mengembalikan kapan sebuah pasangan (akun, model) berhenti
// dihindari, atau 0 kalau tidak sedang dihindari.
func modelDenyUntil(acctID int64, mc ModelConfig) int64 {
	if acctID <= 0 || mc.HexID == "" {
		return 0
	}
	modelDenyMu.Lock()
	defer modelDenyMu.Unlock()
	return modelDeny[modelDenyKey(acctID, mc)]
}

// modelDenySoonest menjawab: kalau semua kandidat untuk model ini sedang
// cooldown, berapa lama lagi sampai yang terdekat bebas?
//
// Dipakai pemanggil untuk memutuskan "tunggu sebentar" versus "menyerah".
// Alasannya konkret: cooldown lima menit untuk seluruh pool pernah membuat
// permintaan prompt besar gagal instan selama lima menit penuh, padahal satu
// akun mungkin bebas sepuluh detik lagi. Operator melihatnya sebagai "Argon
// kadang jalan kadang tidak".
func modelDenySoonest(mc ModelConfig) (time.Duration, bool) {
	if mc.HexID == "" {
		return 0, false
	}
	suffix := fmt.Sprintf("|%s|%d|%t|%d|%d",
		mc.HexID, mc.Tool, mc.Thinking, mc.ThinkLevel, mc.Mode)
	now := time.Now()
	modelDenyMu.Lock()
	defer modelDenyMu.Unlock()
	best := time.Duration(0)
	found := false
	for k, until := range modelDeny {
		if !strings.HasSuffix(k, suffix) {
			continue
		}
		d := time.Unix(until, 0).Sub(now)
		if d <= 0 {
			continue
		}
		if !found || d < best {
			best, found = d, true
		}
	}
	return best, found
}

// denyWaitBudget adalah batas tunggu saat semua kandidat cooldown.
func denyWaitBudget() time.Duration {
	sec := rtCfg().DenyWaitBudgetSec
	if sec <= 0 {
		return 0
	}
	return time.Duration(sec) * time.Second
}

// provenModels mencatat akun yang SUDAH terbukti menyajikan sebuah model —
// bukan sekadar lulus filter rollout.
//
// Kenapa perlu: filter rollout hanya melihat flag dari ESY5D, sementara bukti
// sebenarnya adalah jawaban hulu. Begitu sebuah akun berbayar sekali menjawab
// dengan model yang benar, dia setara dengan pemegang rollout dan boleh ikut
// rotasi penuh — sehingga permintaan tersebar ke semua akun yang benar-benar
// mampu, bukan menumpuk di dua akun yang kebetulan punya flag.
//
// Umur 6 jam: cukup untuk menutupi satu sesi kerja, cukup pendek untuk pulih
// sendiri kalau hak akses akun berubah (Google menambah/mencabut rollout kapan
// saja). Hilang saat proses berhenti — sama seperti papan skor latensi: ini
// petunjuk pemilihan, bukan data laporan.
const modelProvenTTL = 6 * time.Hour

var (
	provenMu     sync.Mutex
	provenModels = map[string]int64{} // key -> unix expiry
)

// markModelProven mencatat bukti positif: akun ini menyajikan model yang diminta.
func markModelProven(acctID int64, mc ModelConfig) {
	if acctID <= 0 || mc.HexID == "" {
		return
	}
	provenMu.Lock()
	provenModels[modelDenyKey(acctID, mc)] = time.Now().Add(modelProvenTTL).Unix()
	provenMu.Unlock()
}

// modelProven melaporkan apakah akun ini sudah pernah terbukti melayani model ini.
func modelProven(acctID int64, mc ModelConfig) bool {
	if acctID <= 0 || mc.HexID == "" {
		return false
	}
	k := modelDenyKey(acctID, mc)
	provenMu.Lock()
	defer provenMu.Unlock()
	until, ok := provenModels[k]
	if !ok {
		return false
	}
	if time.Now().Unix() >= until {
		delete(provenModels, k)
		return false
	}
	return true
}

// provenSeedWindow: seberapa jauh ke belakang riwayat dipakai untuk menanam
// ulang bukti saat start.
const provenSeedWindow = 7 * 24 * time.Hour

// seedProvenFromHistory menanam ulang bukti "akun ini pernah menyajikan model ini
// dengan benar" dari tabel requests.
//
// Kenapa perlu: catatan bukti di memori hilang saat proses berhenti, dan kalau
// hanya mengandalkan "terbukti ketika dilayani", akun berbayar yang flag
// rollout-nya tidak ikut terkirim tidak akan pernah dilayani — jadi tidak akan
// pernah terbukti juga (ayam dan telur, terukur: 8 request Argon beruntun
// semuanya mendarat di dua akun pemegang rollout). Riwayat request sudah
// menyimpan pasangan (akun, model, label hulu) yang cocok, jadi bukti itu cukup
// dibaca kembali.
func seedProvenFromHistory() {
	rows, err := getDB().Query(
		`SELECT DISTINCT account_id, model, upstream_model FROM requests
		 WHERE status=200 AND account_id IS NOT NULL AND upstream_model<>'' AND ts>=?`,
		time.Now().Add(-provenSeedWindow).Unix())
	if err != nil {
		return
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var acctID int64
		var model, served string
		if err := rows.Scan(&acctID, &model, &served); err != nil {
			continue
		}
		mc, ok := Models[model]
		if !ok || mc.HexID == "" {
			continue
		}
		if !modelServedMatches(mc, served) {
			continue
		}
		markModelProven(acctID, mc)
		n++
	}
	if n > 0 {
		logf("[pool] %d pasangan akun+model ditandai terbukti dari riwayat", n)
	}
}

// pickCookieAccountFor 挑一个**真的能用这个模型**的账号。
//
// Ini perbaikan inti untuk masalah "model yang dipanggil beda dengan upstream":
// pemilihan akun dulu murni round-robin, jadi request `gemini-4-argon` bisa
// mendarat di akun free. Google tidak menolaknya — dia menjawab 200 dengan
// 3.5 Flash-Lite. Klien melihat sukses, modelnya salah, dan tidak ada cara
// mendeteksinya selain membaca nama model di frame respons.
//
// Sekarang akun disaring dulu menurut hasil probe ESY5D (tier + rollout +
// sisa kuota). Akun yang belum pernah diprobe tidak disaring (supportsModel
// mengembalikan true untuk data kosong), supaya sistem tetap jalan sebelum
// probe pertama.
//
// Kalau tidak ada satu pun akun yang cocok, fungsi ini mengembalikan false
// sehingga pemanggil bisa memberi error yang jelas, bukan diam-diam
// menurunkan model.
func pickCookieAccountFor(mc ModelConfig, skip map[int64]bool) (*CookieAccount, bool) {
	pickMu.Lock()
	defer pickMu.Unlock()
	// 健康的排前面，同样健康的按最久未用轮转。
	//
	// 不这么排的话坏号会跟好号平起平坐地轮到，而挑到坏号时它没有绑定的出口，
	// 出口就按"无偏好"选了；等换号换到好号，出口已经定死 —— 于是好号的出口
	// 粘性被坏号带偏。实测 1 好 2 坏时出口在两个代理间对半分。
	rows, err := getDB().Query(
		`SELECT id, label, cookie, status, note, created_at, last_used_at, last_ok_at, last_error, fail_count, proxy_id, auth_user, tier_override, impersonate, cookie_source, cookie_sid
		 FROM accounts WHERE status='enabled' ORDER BY fail_count ASC, last_used_at ASC, id ASC`)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	// Dua tingkat pilihan: akun BERBAYAR dulu (kalau mendukung model ini), baru
	// akun free. Di dalam satu tingkat tetap urut round-robin dari SQL di atas.
	//
	// Kenapa berbayar dulu (permintaan operator, dan masuk akal buat kuota):
	// akun pro punya rollout dan jatah model besar; akun free kadang bisa
	// melayani model yang sama (3.1 Pro di acc1 terbukti 200) tapi ini cadangan,
	// bukan sasaran utama. Akun yang belum pernah diprobe dianggap non-pro
	// (cadangan), jadi akun pro yang sudah terdeteksi selalu menang.
	// Tiga tingkat prioritas:
	//   2 = TERBUKTI  — rollout eksplisit untuk model ini
	//   1 = BERBAYAR  — belum terbukti, tapi daftar modelnya memuat model ini
	// Tingkat 1 hanya dipakai kalau tingkat 2 benar-benar tidak ada. Jadi akun
	// yang belum terbukti tidak pernah mencuri giliran dari akun yang sudah
	// teruji, tapi model terkunci tidak lagi mati total ketika satu-satunya
	// pemegang rollout sedang cooldown 5 menit.
	type cand struct {
		a    CookieAccount
		prio int
		pro  bool
	}
	var cands []cand
	for rows.Next() {
		var a CookieAccount
		if err := rows.Scan(&a.ID, &a.Label, &a.Cookie, &a.Status, &a.Note,
			&a.CreatedAt, &a.LastUsedAt, &a.LastOkAt, &a.LastError, &a.FailCount, &a.ProxyID, &a.AuthUser, &a.TierOverride, &a.Impersonate, &a.CookieSource, &a.CookieSID); err != nil {
			continue
		}
		if skip[a.ID] {
			continue
		}
		// Akun yang baru saja disajikan model lain untuk model ini dilewati
		// selama cooldown, supaya request berikutnya tidak membayar 10-20 detik
		// lagi untuk hasil yang sama.
		if modelDenied(a.ID, mc) {
			continue
		}
		cap := probeForAccount(a.ID)
		prio := 2
		if mc.HexID != "" && !cap.supportsModel(mc) {
			switch {
			case modelProven(a.ID, mc):
				// Sudah pernah menjawab dengan model yang benar walau flag
				// rollout-nya tidak ikut terkirim: perlakukan sebagai setara
				// pemegang rollout supaya ikut rotasi penuh.
			case cap.supportsModelPaidOnly(mc):
				prio = 1
			default:
				// Model berbayar (Argon, 3.1 Pro, 3.8/3.7 Flash) TIDAK boleh
				// dilayani akun free sekalipun sebagai usaha terakhir: Google
				// tidak menolaknya, dia menjawab dengan model lain - terukur
				// 3.8 diturunkan ke 3.6. Itu jawaban salah yang tidak bisa
				// dibedakan klien, jadi lebih baik tidak memilih sama sekali.
				//
				// Sama untuk model media (image/music/video/canvas): di sana
				// kemampuan adalah cap spesifik (VIDEO_GENERATION, IMAGEN_4,
				// CANVAS, kuota musik), dan akun tanpa cap itu dijawab model
				// chat biasa. Terukur 2026-11-06: gemini-video mendarat di akun
				// tanpa VIDEO_GENERATION lalu "upstream downgraded ... served
				// 3.8 Flash". Usaha terakhir tidak berlaku di sini.
				if modelRequiresPaidAccount(mc) || mc.Tool > 0 {
					continue
				}
				// Daftar model bukan bukti: lima akun berlabel tier=free terbukti
				// melayani "4 Argon" (diuji 2026-11-03, upstream bilang 4 Argon).
				// Jadi akun tetap jadi kandidat paling akhir, bukan dibuang -
				// deteksi downgrade + cooldown yang memutuskan, bukan label.
				prio = 0
			}
		}
		cands = append(cands, cand{a: a, prio: prio, pro: cap.Tier == "pro"})
	}
	var chosen *CookieAccount
	bestPrio, bestPro := -1, false
	for i := range cands {
		c := &cands[i]
		better := c.prio > bestPrio
		if !better && c.prio == bestPrio {
			// Rata-rata 1 jam per akun dipakai sebagai pemecah seri: yang jelas
			// lebih cepat menang, yang praktis sama tetap ikut round-robin.
			switch {
			case c.pro && !bestPro:
				better = true
			case c.pro == bestPro && (chosen == nil || fasterAccount(c.a.ID, chosen.ID)):
				better = true
			}
		}
		if better {
			chosen = &c.a
			bestPrio = c.prio
			bestPro = c.pro
		}
	}
	if chosen == nil {
		// Tidak ada akun yang mendukung model ini: kembalikan false.
		//
		// Dulu di sini ada jalur "upaya terakhir" yang mengembalikan akun pertama yang
		// tersaring. Itu justru lebih buruk daripada tidak memilih sama sekali: akun
		// free tanpa rollout `Argon4p0` selalu menjawab 200 dengan model lain, dan
		// deteksi downgrade di pemanggil lalu mengubahnya jadi 502 pasti. Pengalaman
		// nyata: setiap `gemini-4-argon` + cookie mendarat di akun free dan gagal,
		// sementara jalur anonim untuk model yang sama sukses. Lebih baik serahkan ke
		// pemanggil supaya jatuh ke anonim / memberi error yang jujur.
		if mc.HexID != "" {
			logf("[cookie] tidak ada akun yang mendukung model %s; menyerahkan ke jalur anonim", modelLabel(mc))
		}
		return nil, false
	}
	_, _ = getDB().Exec(`UPDATE accounts SET last_used_at=? WHERE id=?`, time.Now().Unix(), chosen.ID)
	return chosen, true
}

// markCookieByStatus 按上游返回回写 cookie 健康度。
//
// 只把明确的鉴权失败（401/403）算作 cookie 的错。网络错误、代理失败、302 → sorry
// （IP 被 Google 拦）一律不计——实测住宅代理出口退化率高达 75%，把这些算进
// fail_count 会让它变成代理噪音，好 cookie 会被误伤成"失败最多"。
//
// statusCode 为 0 表示压根没拿到响应（网络层失败）。
func markCookieByStatus(id int64, statusCode int, errStr string) {
	// 401/403 belum tentu cookie mati: browser yang masih aktif memakai akun
	// yang sama ikut me-rotate 1PSIDTS, sehingga token kita basi sementara
	// sesinya sehat. Bedakan dulu sebelum menaikkan fail_count.
	if statusCode == 401 || statusCode == 403 {
		if a := accountByID(id); a != nil {
			invalidateXSRF(a.Cookie)
			if _, xerr := getXSRF(a.Cookie, proxyURLByID(a.ProxyID)); xerr == nil {
				logf("[cookie] akun #%d: %d tapi sesi masih hidup "+
					"(rotate conflict) - fail_count tidak dinaikkan", id, statusCode)
				_, _ = getDB().Exec(
					`UPDATE accounts SET last_ok_at=?, last_error='rotate conflict' WHERE id=?`,
					time.Now().Unix(), id)
				return
			}
		}
	}
	switch {
	case statusCode == 200:
		markAccountResult(id, true, "")
	case statusCode == 401 || statusCode == 403:
		markAccountResult(id, false, errStr)
	default:
		// 其余情况责任不在 cookie，不动它的健康度
	}
}

// transportErrorPatterns 是"请求根本没到达 Google"的特征串。命中这些错误时
// 责任在出口（代理/网络），不在 cookie —— 账号不能因为家里网断了被停用。
var transportErrorPatterns = []string{
	"proxyconnect", "dial tcp", "dial unix", "connection refused",
	"no such host", "i/o timeout", "context deadline exceeded",
	"network is unreachable", "tls handshake timeout", "connection reset by peer",
	"proxy authentication", "socks connect",
}

// isTransportError 判断错误是不是出在出口链路上。
func isTransportError(errStr string) bool {
	if errStr == "" {
		return false
	}
	low := strings.ToLower(errStr)
	for _, p := range transportErrorPatterns {
		if strings.Contains(low, p) {
			return true
		}
	}
	return false
}

// markAccountResult 请求结束后回写结果：成功清零 fail_count 并记 last_ok_at；
// 失败累加 fail_count 并记 last_error。
//
// 注意 last_ok_at 的语义是"这个 cookie 参与的请求成功过"，**不等于"cookie 仍然
// 有效"**：cookie 过期后 Gemini 不报错，只是把你当匿名用户，纯文本请求照样 200。
// 要真正验有效性，最省事的判据是请求 gemini-3.1-pro：cookie 有效时服务端回报
// "3.1 Pro" 并带思考链，失效时静默降级成 3.5 Flash-Lite。（xsrf.go 取 token 时
// 若页面里没有 SNlM0e，也会当场判定 cookie 失效。）
func markAccountResult(id int64, ok bool, errStr string) {
	if id <= 0 {
		return
	}
	if ok {
		_, _ = getDB().Exec(
			`UPDATE accounts SET last_ok_at=?, fail_count=0, last_error='' WHERE id=?`,
			time.Now().Unix(), id)
		return
	}
	// 出口挂了不等于 cookie 挂了。曾经因为 WARP 端口掉了，一个健康账号在三次
	// 重试里被记成 fail_count=3 然后自动停用 —— 症状和"cookie 过期"一模一样，
	// 排查方向却完全相反。只记录，不惩罚。
	if isTransportError(errStr) {
		_, _ = getDB().Exec(`UPDATE accounts SET last_error=? WHERE id=?`,
			truncate("transport: "+errStr, 200), id)
		logf("[cookie] akun #%d tidak dihukum: kegagalan di jalur keluar, bukan sesi", id)
		return
	}
	_, _ = getDB().Exec(
		`UPDATE accounts SET fail_count=fail_count+1, last_error=? WHERE id=?`,
		truncate(errStr, 200), id)
	autoDisableIfDead(id)
}

// maxCookieAuthFailures 是连续几次鉴权失败之后自动停用账号。
//
// 只有 401/403 会累加 fail_count（见 markCookieByStatus），网络错误和被 Google 拦
// 都不算，所以连着 3 次基本等于 cookie 真的没了。取 3 不取 1：偶发的 XSRF 页面抖动
// 也会走到这条路上，一次就停用会误伤。
const maxCookieAuthFailures = 3

// reviveAccountAfterSuccess mencabut penanda mati pada akun yang baru saja
// membuktikan dirinya hidup.
//
// Kenapa perlu: status 'disabled' hanya dicabut oleh rotate atau tombol
// "Deteksi" di panel. Padahal akun bisa membuktikan hidupnya lewat jalur lain
// yang lebih sering terjadi — berhasil memperbarui token XSRF, atau lolos
// pemeriksaan downgrade. Tanpa fungsi ini satu kegagalan lama membuat akun
// sehat tetap tersaring selamanya (pool pelan-pelan habis, dan gejalanya
// "kenapa akun saya disabled terus padahal jelas masih jalan").
//
// Hanya menyentuh akun yang memang disabled ATAU masih membawa fail_count;
// akun yang sudah enabled dan bersih tidak diapa-apakan supaya tidak menulis
// DB tiap request.
func reviveAccountAfterSuccess(id int64) {
	if id <= 0 {
		return
	}
	res, err := getDB().Exec(
		`UPDATE accounts SET status='enabled', fail_count=0, last_ok_at=?, last_error=''
		 WHERE id=? AND (status!='enabled' OR fail_count>0 OR last_error!='')`,
		time.Now().Unix(), id)
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		logf("[cookie] akun #%d pulih: status=enabled, fail_count=0, last_ok diperbarui "+
			"(bukti hidup dari request sukses)", id)
	}
}

// repairAccountStatuses menjalankan pemulihan yang sama untuk SEMUA akun saat
// boot.
//
// Kenapa di boot: penanda mati yang dulu ditulis oleh bug rotate tetap ada di
// DB walaupun kodenya sudah diperbaiki. Tanpa sapuan ini, akun yang sehat
// tetap tersaring sampai kebetulan dipilih — dan akun yang tersaring justru
// tidak pernah dipilih, jadi penandanya tidak pernah dicabut sendiri. Sapuan
// ini tidak mengaktifkan akun yang benar-benar mati: akun yang cookie-nya
// memang tidak berlaku akan jatuh lagi ke jalur kegagalan biasa pada request
// berikutnya, dan autoDisableIfDead menutupnya kembali setelah ambang.
//
// Akun yang di-disable MANUAL oleh operator juga dibiarkan: bedanya tidak
// terlihat dari DB, jadi patokannya dua hal sekaligus — last_error harus
// membawa "no SNlM0e" DAN akun harus pernah sukses dalam grace window.
//
// Syarat kedua itu penting: tanpa itu, akun yang mati berhari-hari (cookie
// benar-benar hangus) ikut diaktifkan lagi di setiap boot, lalu satu request
// membayar XSRF round-trip hanya untuk gagal. Prinsipnya sama dengan
// recentOKWindow di rotate.go: "baru sukses" adalah bukti hidup, "lama tidak
// pernah sukses" bukan.
func repairAccountStatuses() {
	cutoff := time.Now().Add(-recentOKWindow).Unix()
	res, err := getDB().Exec(
		`UPDATE accounts SET status='enabled', fail_count=0, last_error=''
		 WHERE status='disabled' AND last_error LIKE '%no SNlM0e%' AND last_ok_at >= ?`,
		cutoff)
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		logf("[cookie] boot: %d akun yang dulu di-disable oleh bug rotate dipulihkan "+
			"(sukses dalam %s terakhir; akan diverifikasi oleh request/rotate berikutnya)",
			n, recentOKWindow)
	}
}

// autoDisableIfDead 把连续失败到头的账号停用。
//
// 为什么要自动停：挑号是 `ORDER BY fail_count ASC` ——坏号排在最后，但**池子里只剩
// 坏号时它照样会被选中**，于是每个请求都要把它试一遍才轮到报错。而 fail_count 只有
// 成功才清零，死号永远不会自己好，等于让每个请求都为它付一次 XSRF 往返。
//
// 停用而不是删除：cookie 是用户导入的数据，判断可能出错（比如出口连续被拦也可能
// 表现成鉴权失败），留着让用户在面板上看到并自己决定。
func autoDisableIfDead(id int64) {
	var fails int64
	var status string
	err := getDB().QueryRow(`SELECT fail_count, status FROM accounts WHERE id=?`, id).
		Scan(&fails, &status)
	if err != nil || status != "enabled" {
		return
	}
	if fails < maxCookieAuthFailures {
		return
	}
	if _, err := getDB().Exec(
		`UPDATE accounts SET status='disabled' WHERE id=? AND status='enabled'`, id); err == nil {
		logf("[cookie] 账号 #%d 连续 %d 次鉴权失败，已自动停用", id, fails)
	}
}

// CookieCheck 是一次 cookie 有效性检测的结果。
type CookieCheck struct {
	OK        bool   `json:"ok"`
	Detail    string `json:"detail"`
	ProxyName string `json:"proxy_name"`
	TookMs    int64  `json:"took_ms"`
}

// checkAccountCookie 判断一条 cookie 还有没有登录态。
//
// 判据是 /app 页面里有没有 SNlM0e：cookie 失效时 Gemini 不报错，只是把你当匿名
// 用户，纯文本请求照样 200 —— 所以不能拿"请求成功"当有效性判据。这个页面没有
// SNlM0e 就说明服务端没认这个登录态。
//
// 只抓页面，不发对话，不消耗生成配额。
func checkAccountCookie(a CookieAccount) CookieCheck {
	t0 := time.Now()
	picked, ok, err := acquireSlot(a.ProxyID)
	if !ok {
		return CookieCheck{Detail: "拿不到出口：" + err.Error()}
	}
	defer releaseSlot(picked.ID)
	proxyURL := picked.URL
	name := picked.Name
	if name == "" {
		name = "直连"
	}

	// 先作废缓存，否则可能拿到几分钟前的旧结论，检测就没意义了
	invalidateXSRF(a.Cookie)
	_, err = getXSRF(a.Cookie, proxyURL)
	took := time.Since(t0).Milliseconds()
	if err != nil {
		markAccountResult(a.ID, false, err.Error())
		return CookieCheck{Detail: explainCookieFailure(err), ProxyName: name, TookMs: took}
	}
	markAccountResult(a.ID, true, "")
	detail := "登录态有效"
	if extractSAPISID(a.Cookie) == "" {
		detail += "，但缺 SAPISID（算不出授权头，部分接口会被当匿名）"
	}
	return CookieCheck{OK: true, Detail: detail, ProxyName: name, TookMs: took}
}

// explainCookieFailure 把底层错误翻成运维看得懂的结论。
func explainCookieFailure(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "no SNlM0e"):
		return "cookie 已失效：页面能打开但没有登录态，请求会被当匿名处理"
	case strings.Contains(msg, "HTTP 302"):
		return "cookie 无效：被重定向到登录页"
	case strings.Contains(msg, "HTTP 429"), strings.Contains(msg, "sorry"):
		return "出口被上游限流，换个代理再试（不是 cookie 的问题）"
	default:
		return "检测失败：" + msg
	}
}

// bindAccountProxy 记住这个账号这次用的出口，下次优先复用。
func bindAccountProxy(accountID, proxyID int64) {
	if accountID <= 0 {
		return
	}
	_, _ = getDB().Exec(`UPDATE accounts SET proxy_id=? WHERE id=?`, proxyID, accountID)
}

// accountDisplayName 面板/记录里显示的账号名，没填标签就用 #id。
func accountDisplayName(a *CookieAccount) string {
	if a.Label != "" {
		return a.Label
	}
	return fmt.Sprintf("#%d", a.ID)
}

// mergeSetCookie 把响应下发的 Set-Cookie 合并进现有 cookie 串。
//
// 服务端几乎每个响应都在刷新 SIDCC / __Secure-1PSIDCC / __Secure-3PSIDCC
// （2 小时抓包里 batchexecute 就刷了 468 次），浏览器收下再带回去。一直发旧值的
// 客户端会被判定为过期会话 —— 实测不合并的号活一两小时就失效。
//
// 顺序保持原样、新键追加在后：cookie 顺序本身不影响语义，但保持稳定能让
// poolHasCookie 那类按内容比对的地方不至于每次都认成新值。
func mergeSetCookie(cookie string, setCookie []string) string {
	if len(setCookie) == 0 {
		return cookie
	}
	updates := map[string]string{}
	for _, sc := range setCookie {
		first := strings.TrimSpace(strings.SplitN(sc, ";", 2)[0])
		i := strings.Index(first, "=")
		if i <= 0 {
			continue
		}
		name, val := first[:i], first[i+1:]
		// 删除指令（过期时间在过去 + 空值）不能当成新值写进去
		if val == "" && strings.Contains(strings.ToLower(sc), "expires=thu, 01 jan 1970") {
			continue
		}
		updates[name] = val
	}
	if len(updates) == 0 {
		return cookie
	}
	var parts []string
	seen := map[string]bool{}
	for _, kv := range splitCookiePairs(cookie) {
		seen[kv[0]] = true
		if v, ok := updates[kv[0]]; ok {
			parts = append(parts, kv[0]+"="+v)
			continue
		}
		parts = append(parts, kv[0]+"="+kv[1])
	}
	for name, val := range updates {
		if !seen[name] {
			parts = append(parts, name+"="+val)
		}
	}
	return strings.Join(parts, "; ")
}

// cookieIdentity 取能代表"这是哪个账号"的那几项。
//
// SAPISID 用来算授权头、__Secure-1PSID 是会话主键，两者都不随刷新变化（2 小时
// 抓包里 0 次变动，变的只有 *SIDCC 和 *SIDTS 那两族）。所以它们变了就说明这份
// cookie 已经不是原来那个账号了。
func cookieIdentity(cookie string) string {
	var sapisid, psid string
	for _, kv := range splitCookiePairs(cookie) {
		switch kv[0] {
		case "SAPISID":
			sapisid = kv[1]
		case "__Secure-1PSID":
			psid = kv[1]
		}
	}
	return sapisid + "|" + psid
}

// updateAccountCookie 把刷新后的 cookie 写回账号。
//
// 写之前先比对身份：合并 Set-Cookie 时上游理论上可以把整套会话换掉（比如响应里
// 带了另一个账号的 SID），照单全收就等于把 A 号的凭据写进 B 号那一行。之后这个号
// 在面板上显示的还是原来的标签，实际发出去的却是别人的会话，而且**完全静默**。
// 身份对不上就不写，宁可让这次刷新白费。
func updateAccountCookie(id int64, cookie string) {
	if id <= 0 || cookie == "" {
		return
	}
	var cur string
	if err := getDB().QueryRow(`SELECT cookie FROM accounts WHERE id=?`, id).Scan(&cur); err == nil {
		if got, want := cookieIdentity(cookie), cookieIdentity(cur); got != want {
			logf("[cookie] 账号 #%d 的刷新结果身份对不上，已丢弃不写回", id)
			return
		}
	}
	_, _ = getDB().Exec(`UPDATE accounts SET cookie=? WHERE id=?`, cookie, id)
}

// accountSetImpersonate menyimpan sidik jari untuk satu akun ("" = ikut global).
func accountSetImpersonate(id int64, profile string) error {
	profile = strings.TrimSpace(profile)
	_, err := getDB().Exec(`UPDATE accounts SET impersonate=? WHERE id=?`, profile, id)
	return err
}

// impersonateForAccount mengembalikan sidik jari efektif sebuah akun.
func impersonateForAccount(id int64) string {
	var p string
	if err := getDB().QueryRow(`SELECT impersonate FROM accounts WHERE id=?`, id).Scan(&p); err != nil {
		return ""
	}
	return strings.TrimSpace(p)
}

// impersonateForCookie mencari sidik jari dari cookie yang tersimpan.
func impersonateForCookie(cookie string) string {
	if strings.TrimSpace(cookie) == "" {
		return ""
	}
	var p string
	if err := getDB().QueryRow(`SELECT impersonate FROM accounts WHERE cookie=?`, cookie).Scan(&p); err != nil {
		return ""
	}
	return strings.TrimSpace(p)
}
