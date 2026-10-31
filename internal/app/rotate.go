package app

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
)

// 会话保活。同一条 POST /RotateCookies 有两种 payload，刷的不是同一族 cookie：
//
//  1. 哨兵 `[000,"-0000000000000000000"]`：无条件换发 `__Secure-1PSIDTS` /
//     `__Secure-3PSIDTS`。这是登录态真正的短命票（约 30 分钟），不刷就会被
//     当匿名。payload 必须是这串 JSPB（前导零合法、严格 JSON 非法），
//     json.Marshal 会变成 `[0,"…"]`，服务端不认。
//
//  2. 浏览器 iframe 那条：先 GET RotateCookiesPage 拿会话 id，再 POST
//     `[658,"<id>"]`。只刷新 SIDCC / `__Secure-1PSIDCC` / `__Secure-3PSIDCC`，
//     间隔由页面 init(...) 最后一个参数给出（实测 600 秒）。
//
// 以前只做了第 2 条，所以号大概半小时就死（issue #6）。Chrome 新版本还有
// DBSC 设备绑定（GET /RotateBoundCookies + 签名 JWT），那条我们复刻不了；
// 哨兵这条对 Firefox 导出、以及未绑定设备的会话有效。Chrome 导出的号如果
// 反复 401，换 Firefox 重新登录再导出。

const (
	rotatePageURL = "https://accounts.google.com/RotateCookiesPage" +
		"?og_pid=658&rot=3&origin=https%3A%2F%2Fgemini.google.com&exp_id=0"
	rotatePostURL = "https://accounts.google.com/RotateCookies"
	// og_pid 是产品标识，Gemini 固定 658；它既作为上面页面的 query，也回显在页面里。
	rotateProductID = 658
	// 服务端没给出间隔时的兜底值。
	defaultRotateInterval = 10 * time.Minute
	// 启动后尽快刷一次：导入时 cookie 可能已经快到期，干等 10 分钟会直接过期。
	firstRotateDelay = 15 * time.Second
	// 哨兵 payload。前导零是故意的，见文件头。
	rotate1PSIDTSBody = `[000,"-0000000000000000000"]`
	// 打太勤会 429。Gemini-API / notebooklm-py 都用 60 秒地板。
	min1PSIDTSInterval = 60 * time.Second
)

// 刷新 1PSIDTS 只带这一对。多带实测会 401。
var rotate1PSIDTSCookies = []string{"__Secure-1PSID", "__Secure-1PSIDTS"}

var (
	psidtsMu     sync.Mutex
	psidtsLastAt = map[int64]time.Time{}
)

// 页面里形如：init('4162200486104360679', 658.0, 0.0, 0.0, 600.0)
// 第一个参数是这个会话的标识，最后一个是下次轮转的间隔秒数。
var rotateInitRe = regexp.MustCompile(`init\('([^']{4,64})'\s*,\s*([0-9.]+)\s*,[^)]*?([0-9.]+)\s*\)`)

// recentOKWindow adalah berapa lama "baru sukses" masih dianggap bukti bahwa
// cookie hidup. Dipakai untuk menahan auto-disable saat rotate gagal sesaat:
// akun yang sukses dalam jendela ini tidak mungkin sesinya tamat karena satu
// percobaan SNlM0e hilang.
const recentOKWindow = 30 * time.Minute

// humanSince memformat selisih detik untuk log. -1 berarti belum pernah sukses.
func humanSince(sec int64) string {
	if sec < 0 {
		return "belum pernah"
	}
	if sec < 3600 {
		return fmt.Sprintf("%dm", sec/60)
	}
	return fmt.Sprintf("%dh", sec/3600)
}

// rotateAccount 给一个账号做一次保活：先刷 1PSIDTS，再刷 SIDCC。
// 返回服务端建议的下次间隔，以及这一轮实际刷新的 cookie 名。
func rotateAccount(a CookieAccount) (time.Duration, []string, error) {
	proxyURL := ""
	if a.ProxyID > 0 {
		proxyURL = proxyURLByID(a.ProxyID)
	}

	cookie := a.Cookie
	var names []string
	var psidtsErr, sidccErr error
	interval := defaultRotateInterval

	c2, n, err := tryRotate1PSIDTS(a.ID, cookie, proxyURL)
	if err != nil {
		psidtsErr = err
		logf("[rotate] 账号 #%d 刷新 1PSIDTS 失败: %v", a.ID, err)
	} else {
		cookie = c2
		names = append(names, n...)
	}

	c3, iv, n2, err := rotateSIDCC(cookie, proxyURL)
	if err != nil {
		sidccErr = err
		logf("[rotate] 账号 #%d SIDCC 保活失败: %v", a.ID, err)
	} else {
		cookie = c3
		names = append(names, n2...)
		if iv > 0 {
			interval = iv
		}
	}

	// === DETEKSI KONFLIK ROTATE ===
	// 401 dari RotateCookies belum tentu berarti cookie mati. Kalau browser
	// masih aktif memakai akun yang sama, browser ikut me-rotate 1PSIDTS dan
	// token kita jadi basi - Google balas 401. Bedanya:
	//   cookie MATI      -> /app tidak membawa SNlM0e sama sekali
	//   cookie DIREBUT    -> /app masih hidup, hanya 1PSIDTS kita yang basi
	// Untuk kasus kedua jangan disable akun; tandai saja perlu resync.
	if psidtsErr != nil && len(names) == 0 {
		invalidateXSRF(cookie)
		if _, xerr := getXSRF(cookie, proxyURL); xerr == nil {
			// Sesi masih hidup -> ini konflik rotate, bukan kematian cookie.
			logf("[rotate] akun #%d: 1PSIDTS ditolak tapi sesi masih hidup "+
				"(browser ikut rotate) - akun dipertahankan", a.ID)
			_, _ = getDB().Exec(
				`UPDATE accounts SET fail_count=0, status='enabled', last_ok_at=?,
				 last_error='rotate conflict: browser holds 1PSIDTS' WHERE id=?`,
				time.Now().Unix(), a.ID)
			// Tetap coba SIDCC: itu jalur berbeda dan sering masih berhasil.
			if c3, iv, n3, err := rotateSIDCC(cookie, proxyURL); err == nil {
				cookie = c3
				names = append(names, n3...)
				if iv > 0 {
					interval = iv
				}
				sidccErr = nil
			}
		} else {
			// Rotate gagal DAN /app tidak membawa SNlM0e. JANGAN langsung disable.
			//
			// Dulu di sini akun ditutup permanen karena satu percobaan: satu
			// RotateCookies yang dijawab 429, atau satu getXSRF yang kena halaman
			// lambat, cukup untuk membuat akun sehat tampil "cookie expired"
			// selamanya. Yang terukur: acc1 punya fail_count=24 tapi last_ok_at
			// hanya beberapa jam sebelumnya — akun yang baru sukses tidak mungkin
			// mati karena satu SNlM0e hilang.
			//
			// Diagnosis yang benar untuk kasus ini: cookie kita TIDAK LENGKAP
			// (kehilangan __Secure-1PSIDTS/3PSIDTS/SIDCC), bukan sesi tamat. Itu
			// bisa pulih sendiri lewat rotate berikutnya, jadi perlakukan sebagai
			// sinyal "perlu resync": naikkan fail_count, biarkan autoDisableIfDead
			// yang memutuskan setelah 3 kegagalan beruntun — aturan yang sama
			// dengan jalur 401/403.
			secondsSinceOK := int64(-1)
			if a.LastOkAt > 0 {
				secondsSinceOK = time.Now().Unix() - a.LastOkAt
			}
			recent := secondsSinceOK >= 0 && secondsSinceOK < int64(recentOKWindow.Seconds())
			logf("[rotate] akun #%d: /app tanpa SNlM0e (rotate gagal, terakhir sukses %s lalu) - "+
				"tidak di-disable, tandai perlu resync", a.ID, humanSince(secondsSinceOK))
			_, _ = getDB().Exec(
				`UPDATE accounts SET fail_count=fail_count+1, last_error=? WHERE id=?`,
				"no SNlM0e in page (cookie incomplete or rotate in progress)", a.ID)
			if !recent {
				// Baru kalau memang sudah lama tidak sukses, kegagalan ini boleh
				// dihitung sebagai kematian (autoDisableIfDead butuh 3 beruntun).
				autoDisableIfDead(a.ID)
			}
		}
	}

	names = uniqueKeepOrder(names)
	if cookie != a.Cookie {
		old := a.Cookie
		updateAccountCookie(a.ID, cookie)
		invalidateXSRF(old)
	}
	// Rotate berhasil = bukti kuat cookie masih hidup. Kalau sebelumnya akun
	// di-disable karena dianggap mati (mis. satu kegagalan "no SNlM0e"),
	// pulihkan sekarang: reset fail_count, enable lagi, catat last_ok_at.
	// Tanpa ini statusnya stale - akun sehat tetap tampil "dead" selamanya
	// karena akun yang disabled tidak pernah dipilih untuk request.
	if len(names) > 0 {
		_, _ = getDB().Exec(
			`UPDATE accounts SET fail_count=0, status='enabled', last_ok_at=?, last_error=''
			 WHERE id=? AND (fail_count>0 OR status!='enabled')`,
			time.Now().Unix(), a.ID)
		logf("[rotate] akun #%d pulih: fail_count=0, status=enabled", a.ID)
	}

	if len(names) > 0 {
		logf("[rotate] 账号 #%d 刷新了 %s", a.ID, strings.Join(names, ", "))
	}
	// 两条路都失败才算失败。1PSIDTS 刷到了但 iframe 页没 init，仍然是续命成功。
	if cookie == a.Cookie && len(names) == 0 {
		if psidtsErr != nil {
			return 0, nil, psidtsErr
		}
		if sidccErr != nil {
			return 0, nil, sidccErr
		}
	}
	return interval, names, nil
}

// tryRotate1PSIDTS 用哨兵 payload 换发 1PSIDTS。没 __Secure-1PSID 或距上次不足
// 60 秒就跳过（不算失败）。
func tryRotate1PSIDTS(id int64, cookie, proxyURL string) (string, []string, error) {
	if cookieValue(cookie, "__Secure-1PSID") == "" {
		return cookie, nil, nil
	}
	if ok, _ := allow1PSIDTSRotate(id); !ok {
		logf("[rotate] 账号 #%d 跳过 1PSIDTS 刷新（距上次不足 %s，避免 429）", id, min1PSIDTSInterval)
		return cookie, nil, nil
	}
	c2, names, err := rotate1PSIDTS(cookie, proxyURL)
	note1PSIDTSAttempt(id, err)
	if err != nil {
		return cookie, nil, err
	}
	return c2, names, nil
}

func allow1PSIDTSRotate(id int64) (bool, time.Duration) {
	psidtsMu.Lock()
	defer psidtsMu.Unlock()
	last := psidtsLastAt[id]
	if last.IsZero() {
		return true, 0
	}
	elapsed := time.Since(last)
	if elapsed >= min1PSIDTSInterval {
		return true, 0
	}
	return false, min1PSIDTSInterval - elapsed
}

func note1PSIDTSAttempt(id int64, err error) {
	// 成功、401/403、429 都记时间，避免紧接着再打。网络错误不记，允许立刻重试。
	if err != nil {
		msg := err.Error()
		if !strings.Contains(msg, "HTTP 401") &&
			!strings.Contains(msg, "HTTP 403") &&
			!strings.Contains(msg, "HTTP 429") {
			return
		}
	}
	psidtsMu.Lock()
	psidtsLastAt[id] = time.Now()
	psidtsMu.Unlock()
}

// rotate1PSIDTS POST 哨兵 payload，把响应里的 Set-Cookie 合并回完整 cookie 串。
func rotate1PSIDTS(cookie, proxyURL string) (string, []string, error) {
	subset := cookieSubset(cookie, rotate1PSIDTSCookies)
	headers := map[string]string{
		"Accept":         "*/*",
		"Content-Type":   "application/json",
		"Origin":         "https://accounts.google.com",
		"Cookie":         subset,
		"Cache-Control":  "no-cache",
		"Pragma":         "no-cache",
		"Sec-Fetch-Dest": "empty",
		"Sec-Fetch-Mode": "cors",
		"Sec-Fetch-Site": "same-origin",
	}
	status, setCookie, respBody, err := rotatePost(rotatePostURL, headers, []byte(rotate1PSIDTSBody), proxyURL)
	if err != nil {
		return cookie, nil, err
	}
	if status == 401 || status == 403 {
		// 401 berulang = sesi ini memang dipegang perangkat lain. Setelah tiga
		// kali, serahkan rotasi ke browser alih-alih mengulanginya.
		noteRotate401(id)
		return cookie, nil, fmt.Errorf("RotateCookies 1PSIDTS 返回 HTTP %d（Chrome 导出的 cookie 可能受设备绑定限制，建议用 Firefox 重新导出）: %s",
			status, truncate(string(respBody), 120))
	}
	if status != 200 {
		return cookie, nil, fmt.Errorf("RotateCookies 1PSIDTS 返回 HTTP %d: %s", status, truncate(string(respBody), 120))
	}
	merged := mergeSetCookie(cookie, setCookie)
	clearRotate401(id)
	return merged, setCookieNames(setCookie), nil
}

// rotateSIDCC 走浏览器 iframe 那条：GET 轮转页拿会话 id，再 POST [658, id]。
func rotateSIDCC(cookie, proxyURL string) (string, time.Duration, []string, error) {
	id, interval, pageSet, err := fetchRotateParams(cookie, proxyURL)
	if err != nil {
		return cookie, 0, nil, err
	}
	cookie = mergeSetCookie(cookie, pageSet)

	body := fmt.Sprintf(`[%d,"%s"]`, rotateProductID, id)
	headers := rotatePostHeaders()
	headers["Cookie"] = stripAuthUserMarker(cookie)
	status, setCookie, respBody, err := rotatePost(rotatePostURL, headers, []byte(body), proxyURL)
	if err != nil {
		return cookie, 0, nil, err
	}
	if status != 200 {
		return cookie, 0, nil, fmt.Errorf("RotateCookies 返回 HTTP %d: %s", status, truncate(string(respBody), 120))
	}
	cookie = mergeSetCookie(cookie, setCookie)
	names := setCookieNames(append(append([]string{}, pageSet...), setCookie...))
	return cookie, interval, names, nil
}

func uniqueKeepOrder(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// setCookieNames 把 Set-Cookie 头里的名字抽出来去重，只用于日志。
func setCookieNames(headers []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range headers {
		name := h
		if i := strings.Index(name, "="); i > 0 {
			name = name[:i]
		}
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// fetchRotateParams 抓 RotateCookiesPage，取会话标识、服务端指定的间隔，以及这一发
// 自己带回的 Set-Cookie。
func fetchRotateParams(cookie, proxyURL string) (string, time.Duration, []string, error) {
	status, setCookie, body, err := rotateGet(rotatePageURL, cookie, proxyURL)
	if err != nil {
		return "", 0, nil, err
	}
	if status != 200 {
		return "", 0, nil, fmt.Errorf("取轮转页返回 HTTP %d", status)
	}
	m := rotateInitRe.FindSubmatch(body)
	if m == nil {
		// 页面拿到了却没有 init(...)，最可能是 cookie 已失效跳到了登录页。
		return "", 0, nil, fmt.Errorf("轮转页里没有 init(...)（cookie 可能已失效）")
	}
	id := string(m[1])
	interval := defaultRotateInterval
	if sec, e := strconv.ParseFloat(string(m[3]), 64); e == nil && sec >= 60 && sec <= 3600 {
		interval = time.Duration(sec) * time.Second
	}
	return id, interval, setCookie, nil
}

// 下面两组 header 逐项抄自抓包（wireHeaders，不是 headers —— 后者不含 cookie）。
// 抓包里还有 sec-ch-ua-arch / -bitness / -form-factors / -full-version-list /
// -model / -platform-version / -wow64 和 x-browser-* / x-client-data /
// x-chrome-id-consistency-request，那些是 Chrome 自己贴的浏览器身份，我们贴了反而
// 会跟 TLS 指纹对不上，所以不贴。

// rotateGet 取轮转页。它在浏览器里是个 iframe 导航，所以 sec-fetch 那组跟普通
// XHR 完全不同（dest=iframe / mode=navigate / site=same-site），别套用默认值。
func rotateGet(url, cookie, proxyURL string) (int, []string, []byte, error) {
	return rotateDo("GET", url, map[string]string{
		"Cookie":                    stripAuthUserMarker(cookie),
		"Accept":                    "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7",
		"Referer":                   "https://gemini.google.com/",
		"Sec-Fetch-Dest":            "iframe",
		"Sec-Fetch-Mode":            "navigate",
		"Sec-Fetch-Site":            "same-site",
		"Sec-Fetch-User":            "?1",
		"Upgrade-Insecure-Requests": "1",
		"Priority":                  "u=0, i",
	}, nil, proxyURL)
}

// rotatePostHeaders 是 POST /RotateCookies 那一发的完整头，调用方只补 Cookie。
func rotatePostHeaders() map[string]string {
	return map[string]string{
		"Accept":         "*/*",
		"Content-Type":   "application/json",
		"Origin":         "https://accounts.google.com",
		"Referer":        rotatePageURL,
		"Cache-Control":  "no-cache",
		"Pragma":         "no-cache",
		"Priority":       "u=1, i",
		"Sec-Fetch-Dest": "empty",
		"Sec-Fetch-Mode": "same-origin",
		"Sec-Fetch-Site": "same-origin",
	}
}

func rotatePost(url string, headers map[string]string, body []byte, proxyURL string) (
	int, []string, []byte, error) {
	return rotateDo("POST", url, headers, body, proxyURL)
}

// rotateDo 走跟正式请求同一个出口：保活从别的 IP 发，等于告诉上游这个会话在两处活动。
//
// 两条传输路径共用同一份 header。以前只有走代理那条调 applyChromeHeaders，直连那条
// 连 User-Agent 都不发 —— 同一个账号在上游看来会因为走没走代理而呈现两种客户端。
func rotateDo(method, url string, headers map[string]string, body []byte, proxyURL string) (
	int, []string, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = strings.NewReader(string(body))
	}
	merged := map[string]string{
		"User-Agent":         ChromeUA,
		"Accept-Language":    "en-US,en;q=0.9",
		"Sec-CH-UA":          `"Chromium";v="146", "Google Chrome";v="146", "Not?A_Brand";v="24"`,
		"Sec-CH-UA-Mobile":   "?0",
		"Sec-CH-UA-Platform": `"Windows"`,
	}
	for k, v := range headers {
		merged[k] = v
	}
	if proxyURL != "" {
		req, err := http.NewRequest(method, url, rdr)
		if err != nil {
			return 0, nil, nil, err
		}
		for k, v := range merged {
			req.Header.Set(k, v)
		}
		resp, err := getStdlibClient(proxyURL).Do(req)
		if err != nil {
			return 0, nil, nil, err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Values("Set-Cookie"), b, err
	}
	req, err := fhttp.NewRequest(method, url, rdr)
	if err != nil {
		return 0, nil, nil, err
	}
	for k, v := range merged {
		req.Header.Set(k, v)
	}
	resp, err := getTLSClient().Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Values("Set-Cookie"), b, err
}

// rotateAllAccounts 给池子里每个启用的账号做一次保活，返回下次该等多久。
//
// 失败**不计入健康度**：保活打的是 accounts.google.com，跟对话能不能用是两码事，
// 网络抖一下就把号标成坏的，会让它在挑号时沉底，反而伤可用性。
// rotate401Streak menghitung berapa kali berturut-turut RotateCookies menjawab
// 401 (sesi dipegang browser / terikat perangkat). Setelah tiga kali, akun itu
// diserahkan kembali ke browser: rotasi dimatikan untuk akun tersebut, bukan
// dipaksa berulang-ulang. Ini meniru cara kerja alat lain yang memakai token
// OAuth - di sana pun kredensial yang dicabut ditandai "perlu auth ulang",
// bukan diulang terus.
var (
	rotate401Mu     sync.Mutex
	rotate401Streak = map[int64]int{}
)

func noteRotate401(accountID int64) {
	if accountID <= 0 {
		return
	}
	rotate401Mu.Lock()
	rotate401Streak[accountID]++
	n := rotate401Streak[accountID]
	rotate401Mu.Unlock()
	if n < 3 {
		return
	}
	_, err := getDB().Exec(
		`UPDATE accounts SET rotate_disabled=1,
		 last_error='sesi dipegang browser (401 berulang) - rotasi diserahkan ke browser'
		 WHERE id=? AND rotate_disabled=0`, accountID)
	if err == nil {
		logf("[rotate] akun #%d: 401 tiga kali berturut, rotasi dimatikan (browser-owned)", accountID)
	}
}

func clearRotate401(accountID int64) {
	rotate401Mu.Lock()
	delete(rotate401Streak, accountID)
	rotate401Mu.Unlock()
}

func rotateAllAccounts() time.Duration {
	next := defaultRotateInterval
	for _, a := range accountList() {
		// Akun yang sesinya dipegang browser dilewati: merotasi tokennya di sini
		// akan membatalkan token yang sedang dipakai browser, dan browser akan
		// membalas dengan membatalkan milik kita. Dua pemilik satu sesi selalu
		// berakhir dengan "no SNlM0e" pada salah satu pihak.
		if a.RotateDisabled != 0 {
			continue
		}
		// Akun disabled tetap dicoba rotate: kalau cookie-nya ternyata masih
		// hidup, rotateAccount akan memulihkannya (reset fail_count + enable).
		// Tanpa ini akun yang sempat gagal sekali tidak akan pernah kembali.
		iv, _, err := rotateAccount(a)
		if err != nil {
			logf("[rotate] 账号 #%d 保活失败: %v", a.ID, err)
			continue
		}
		if iv > 0 {
			next = iv
		}
	}
	return next
}
