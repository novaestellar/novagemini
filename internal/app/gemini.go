package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/google/uuid"
)

// Gemini 服务端认的模型 id，来自 batchexecute?rpcids=otAQ7b 返回的权威清单。
const (
	hexFlash36   = "fbb127bbb056c959" // 3.6 Flash
	hexFlashLite = "cf41b0e0dd7d53e5" // 3.5 Flash-Lite
	hexPro31     = "9d8ca3786ebdfbea" // 3.1 Pro
	// 3.7 Flash：按账号灰度放出。hex 是 otAQ7b 里 3.7 条目的**主 hex**（第一个元素），
	// 两个独立已灰度账号的清单里都是它、且有 3.7 号的用户实测发它回报 "3.7 Flash"
	// （issue #4 / PR #11）。注意别用 compat 列表里那个 797f3d0293f288ad —— 那是
	// "当前 Flash" 泛指针，老批次号发它拿到的是 3.6，会冒充 3.7。老批次号发这个主 hex
	// 会干净降级成 3.5 Flash-Lite（跟 3.1 Pro 一样），所以 gate 成要 cookie。
	hexFlash38 = "56fdd199312815e2" // 3.8 Flash（原记 3.7；Google 把这个 hex 原地升成 3.8，HAR 响应帧显示名已是 "3.8 Flash"）
	// 4 Argon（#35）：otAQ7b 清单里主 hex 95a4c0abcba24364、短名 Argon、代号 Argon4p0、
	// 显示名 "4 Argon"。付费/Ultra 号专属。实测**真正选中它靠的是 MODE=8（header 下标 14 /
	// inner[79]），不是 hex**：借来的 Ultra 号上，hex=Argon 配 mode=1 回报 "3.8 Flash"（mode 1
	// 那个模型），hex=3.8Flash 配 mode=8 反而回报 "4 Argon"——mode 决定、hex 被忽略。所以 Argon
	// 条目的 Mode 必须填 8。仍按浏览器把 hex 配成 Argon 主 hex（最忠实、将来若改回认 hex 也不塌）。
	hexArgon4 = "95a4c0abcba24364"
)

// innerSlots 是 payload 里 inner 数组的长度。浏览器发 97-98 槽，我们原来只开 80，
// 于是 inner[80]（扩展思考）连位置都没有、想填也填不进去。
//
// 加长本身是安全的：曾经把它从 80 加到 102 来试 inner[79] 会不会复活，结论是不会
// —— 模型选择完全由 x-goog-ext-525001261-jspb header 决定，长度不影响。
const innerSlots = 97

// inner[80] / 模型 header 下标 15 的取值：1=普通，2=扩展思考。
//
// **只在登录态生效**：匿名请求带上它服务端静默忽略，回报的仍是普通模型、思考链 0
// 字符（对齐登录态抓包参数复测 3 组，8 次全灭）。所以带这个的模型跟 3.1 Pro 一样，
// 没 cookie 时不暴露。
const (
	thinkingNormal   = 1
	thinkingExtended = 2

	// Argon-class reasoning depth, encoded at index 15 of
	// x-goog-ext-525001261-jspb. Values verified against the live web client:
	// 1 = low / fast, 4 = medium / balanced, 2 = high / deep.
	thinkLevelLow    = 1
	thinkLevelMedium = 4
	thinkLevelHigh   = 2
)

// ModelConfig holds the server-side model id plus the legacy MODE_CATEGORY value.
type ModelConfig struct {
	// HexID 走 x-goog-ext-525001261-jspb header，是服务端唯一认的模型开关。
	// 实测：不发这个 header 时 inner[79] 取 1..6 全部落到 3.5 Flash-Lite；
	// header 写 3.6 而 inner[79] 写 6 时拿到的是 3.6 —— header 压过 inner[79]。
	HexID string
	Mode  int
	Desc  string
	// Thinking 为真时填 inner[80]=2，即网页 UI 上的「扩展思考」。跟 HexID 正交
	// —— 三个模型都能开，不是某个专属模型。
	Thinking bool
	// ThinkLevel mengisi index 15 pada header x-goog-ext-525001261-jspb.
	// 0 = tidak diisi (perilaku lama). 1/4/2 = low/medium/high untuk Argon.
	ThinkLevel int
	// Tool 非 0 时填 inner[49]，让服务端换后端模型生成媒体产物（14=生图 Nano Banana
	// / 21=音乐 Lyria）。产物不在 StreamGenerate 响应里，要再走 hNvQHb 拿下载链、
	// 用下载 host 认的 cookie 子集下回原始字节，见 media.go。跟登录态绑定：匿名请求
	// 这个会被静默降级成一句「Are you signed in?」文本。
	Tool int
}

// 只暴露服务端清单（batchexecute?rpcids=otAQ7b）里真实存在的模型。
// 旧的 gemini-3.5-flash / -thinking / -thinking-lite / gemini-auto /
// gemini-flash-lite 别名已移除：它们在服务端没有对应条目，留着只会让人
// 以为有五种不同的模型可选。
var Models = map[string]ModelConfig{
	"gemini-3.6-flash":      {HexID: hexFlash36, Mode: 1, Desc: "Latest all-around model"},
	"gemini-3.5-flash-lite": {HexID: hexFlashLite, Mode: 6, Desc: "Fastest, lightweight"},
	"gemini-3.1-pro":        {HexID: hexPro31, Mode: 3, Desc: "Most capable; needs a signed-in cookie (downgraded to Flash-Lite without one)"},
	// 3.8 Flash：Google 把原 3.7 那个 hex 原地升级了（同 hex，服务端显示名从 3.7 变 3.8）。
	// 实测是**付费号专属**（不是灰度）：只有付费 Gemini 账号有，免费号降级成 3.5 Flash-Lite。
	// gemini-3.7-flash 保留为别名（同 hex）。
	"gemini-3.8-flash": {HexID: hexFlash38, Mode: 1, Desc: "3.8 Flash; needs a signed-in PAID Google account (free accounts get downgraded to 3.5 Flash-Lite)"},
	"gemini-3.7-flash": {HexID: hexFlash38, Mode: 1, Desc: "alias of gemini-3.8-flash (same hex; Google renamed 3.7→3.8)"},
	// 4 Argon（#35）：Mode=8 是真正的选择开关（见 hexArgon4 注释）。付费/Ultra 号专属，
	// 免费号发它会被静默降级成 3.5 Flash-Lite，所以 gate 成要 cookie（modelNeedsLogin）。
	"gemini-4-argon":        {HexID: hexArgon4, Mode: 8, Desc: "4 Argon (complex tasks); needs a signed-in PAID/Ultra Google account (free accounts get downgraded to 3.5 Flash-Lite)"},
	"gemini-4-argon-low":    {HexID: hexArgon4, Mode: 8, ThinkLevel: thinkLevelLow, Desc: "4 Argon reasoning depth low/fast (index15=1)"},
	"gemini-4-argon-medium": {HexID: hexArgon4, Mode: 8, ThinkLevel: thinkLevelMedium, Desc: "4 Argon reasoning depth medium/balanced (index15=4)"},
	"gemini-4-argon-high":   {HexID: hexArgon4, Mode: 8, ThinkLevel: thinkLevelHigh, Desc: "4 Argon reasoning depth high/deep (index15=2)"},

	// 扩展思考版。inner[80]=2 跟模型 hex 正交，都能开；但只在登录态生效，
	// 所以跟 3.1 Pro 一样在没 cookie 时不暴露。
	"gemini-3.6-flash-thinking":      {HexID: hexFlash36, Mode: 1, Thinking: true, Desc: "3.6 Flash with extended thinking; needs a signed-in cookie"},
	"gemini-3.5-flash-lite-thinking": {HexID: hexFlashLite, Mode: 6, Thinking: true, Desc: "3.5 Flash-Lite with extended thinking; needs a signed-in cookie"},
	"gemini-3.1-pro-thinking":        {HexID: hexPro31, Mode: 3, Thinking: true, Desc: "3.1 Pro with extended thinking; needs a signed-in cookie"},
	"gemini-3.8-flash-thinking":      {HexID: hexFlash38, Mode: 1, Thinking: true, Desc: "3.8 Flash with extended thinking; needs a signed-in cookie"},
	"gemini-3.7-flash-thinking":      {HexID: hexFlash38, Mode: 1, Thinking: true, Desc: "alias of gemini-3.8-flash-thinking"},
	// 4 Argon + 扩展思考：Mode=8（选 Argon）+ inner[80]=2（上游现在把这档叫 "High"）。
	// 实测回报 "4 Argon High" 且带思考链（硬题 985 字符，走既有 [4][0][37][0][0] 路径，
	// 无需改抽取代码）→ 映射成 reasoning_content。付费/Ultra 专属。
	"gemini-4-argon-thinking": {HexID: hexArgon4, Mode: 8, Thinking: true, ThinkLevel: thinkLevelHigh, Desc: "4 Argon with extended thinking (High reasoning tier); emits reasoning_content; needs a signed-in PAID/Ultra account"},

	// 媒体生成。inner[49] 一填，服务端换后端模型出图/出乐；产物走 hNvQHb + 下载 host
	// 取回，以 base64 data URL 塞进 content 返回。都要登录态，没 cookie 时不暴露。
	"gemini-image": {HexID: hexFlash36, Mode: 1, Tool: toolImage, Desc: "Image generation (Nano Banana); returns a base64 data URL; needs a signed-in cookie"},
	"gemini-music": {HexID: hexFlash36, Mode: 1, Tool: toolMusic, Desc: "Music generation (Lyria, ~30s); returns a base64 data URL; needs a signed-in cookie"},
	"gemini-video": {HexID: hexFlash36, Mode: 1, Tool: toolVideo, Desc: "Video generation (Veo, async); returns a base64 data URL; needs a signed-in cookie (usually a paid account)"},
	// 画布：生成 immersive 交互 HTML 文档，内联返回（不是二进制、不用下载）。要登录态。
	"gemini-canvas": {HexID: hexFlash36, Mode: 1, Tool: toolCanvas, Desc: "Canvas: generates an interactive HTML document (returned inline as a ```html block); needs a signed-in cookie"},
}

// hasCookie 表示 cookie 池里有没有可用账号。决定 3.1 Pro 是否出现在模型列表里。
func hasCookie() bool {
	_, enabled := accountCount()
	return enabled > 0
}

// modelNeedsLogin 判断一个模型是否必须登录态才真正生效。匿名请求这批会被服务端
// 静默降级：3.1 Pro / 3.8 Flash → 3.5 Flash-Lite；思考链消失；媒体工具变成一句
// "Are you signed in?" 文本。是「没 cookie 时排除哪些模型」和「#20 匿名优先要不要
// 占一个 cookie 账号」共用的单一判据。
//
// 按 HexID + Thinking + Tool 判，跟旧的按模型名（3.1-pro/3.8-flash/3.7-flash）判
// 逐模型核对等价：那三个正名的 HexID 就是 hexPro31 / hexFlash38，其余登录模型都被
// Thinking 或 Tool 覆盖。
func modelNeedsLogin(mc ModelConfig) bool {
	return mc.HexID == hexFlash38 || mc.HexID == hexPro31 ||
		mc.Thinking || mc.Tool > 0
}

// argonNeedsCookie menandai model yang WAJIB lewat jalur cookie kalau pool berisi
// akun — kebalikan dari aturan lama `preferAnonModel`.
//
// Aturan lama salah karena cuma menguji satu kasus (nova-ff2, prompt kecil) lalu
// digeneralisasi: "Argon + cookie -> isinya error, jadi Argon selalu anonim".
// Kenyataannya jalur anonim punya dua cacat yang jauh lebih mahal:
//   - anonim tidak bisa mengunggah, jadi prompt > 128 KB mustahil dikirim
//     (prepareContextFile menyerah saat cookie kosong) -> 502 pasti;
//   - anonim tidak punya kuota/rollout akun, jadi tidak ada yang menjamin
//     model yang menjawab benar-benar Argon.
//
// Jadi Argon diperlakukan seperti model berbayar lain: pakai cookie.
//
// Ini bukan larangan mutlak: kalau pool kosong (atau semua akun gagal),
// pickCookieAccountFor mengembalikan nil dan permintaan tetap jatuh ke anonim —
// Argon anonim masih lebih berguna daripada tidak bisa dipakai sama sekali.
func argonNeedsCookie(mc ModelConfig) bool {
	return mc.HexID == hexArgon4 && mc.Tool == 0
}

// modelRequiresCookie menjawab: model ini TIDAK BOLEH dijawab lewat anonim,
// karena jalur anonim bukan "tetap jalan" melainkan jawaban salah.
//
// Kelompok ini adalah model yang dibuka dari 3.1 Pro / 3.8 Flash / Thinking /
// media (modelNeedsLogin): anonim tidak menolak, tapi melayani 3.5 Flash-Lite
// dengan konteks jauh lebih kecil. Klien tidak bisa membedakan itu dari jawaban
// benar, jadi lebih baik gagal dengan sebab yang jelas.
//
// Argon sengaja TIDAK ada di sini: Argon TIDAK bisa dibuka oleh 3.5 Flash-Lite
// (mode 8 memilih Argon di header), dan uji langsung membuktikan jalur anonim
// benar-benar menyajikan "4 Argon" — jadi Argon BOLEH jatuh ke anonim sebagai
// cadangan bila semua akun berbayar sedang tidak berhak (kuota habis). Primary
// tetap akun pro (argonNeedsCookie memaksa pick dulu); anonim hanya fallback.
func modelRequiresCookie(mc ModelConfig) bool {
	return modelNeedsLogin(mc)
}

// anonFirstEligible 判断「匿名优先」开关下这次请求能不能走匿名（不占 cookie 账号）：
// 开关开着 + 没带附件 + 模型不需要登录态 + prompt 没超协议上限。
//
// 三个例外会强制走 cookie：
//   - 附件（图/视频）：在对话里引用必须登录，匿名会被上游回 1100。
//   - prompt 超过 MaxPromptBytes：匿名没法上传，prepareContextFile 直接放弃
//     转换，请求必然 502。这是会话被撑大后整段对话死掉的直接原因。
//   - Argon（argonNeedsCookie）：它需要付费号的 rollout 与配额，匿名既发不了
//     大 prompt，也没有任何机制保证真拿到 Argon。
//
// 开关关着永远返回 false（保持旧行为：池里有号就用号）。
func anonFirstEligible(mc ModelConfig, hasAttachment bool, promptBytes int) bool {
	if hasAttachment {
		return false
	}
	// prompt 太大时唯一的活路是上传成文本附件，而上传需要 cookie。这里宁可
	// 打破模型的匿名偏好，也好过必然 502 —— 让付费号来答总比答不了强。
	if budget := rtCfg().MaxPromptBytes; budget > 0 && promptBytes > budget {
		return false
	}
	// Argon 一律先挑号。池子空的时候下面 pickCookieAccountFor 返回 nil，
	// 请求自然回落匿名，所以这里不需要额外判断 hasCookie()。
	if argonNeedsCookie(mc) {
		return false
	}
	return rtCfg().AnonFirst && !modelNeedsLogin(mc)
}

// temporaryChatEligible 判断这次请求该不该走网页端原生「临时对话」（inner[45]=1，#34）。
//
// 与 multi_turn 冲突：临时对话不进会话列表，而 multi_turn 要按历史认出续接、靠服务端
// 留着这串对话——两个一起开，续接那半就没了。所以 multi_turn 开着时一律不发 inner[45]，
// 由 multi_turn 决定会话怎么留。调用点还会再要求有登录 cookie：匿名本来就不落账号，
// 发 inner[45] 是没验过的无谓偏离。
func temporaryChatEligible() bool {
	return rtCfg().TemporaryChat && !rtCfg().MultiTurn
}

// temporaryChatEligibleFor 判断这次请求是否该走临时对话。
//
// 媒体工具（图/音乐/视频）必须排除：产物下载链在 hNvQHb 历史里（音乐/视频），
// 图片虽然链在响应里，但链接 403 时唯一的补救就是从历史取新链。临时对话不落历史，
// hNvQHb 一律回 HTTP 400，等于把补救路径彻底掐断。代价是媒体请求会在会话列表
// 留一条记录，由 AutoDeleteConversation 收尾。
func temporaryChatEligibleFor(tool int) bool {
	if tool == toolImage || tool == toolMusic || tool == toolVideo || tool == toolCanvas {
		return false
	}
	return temporaryChatEligible()
}

// availableModels 返回当前配置下值得暴露的模型。
//
// 没配 cookie 时排除 3.1 Pro：实测匿名请求它会被静默降级成 3.5 Flash-Lite，
// 客户端还以为自己用上了 Pro。与其让它"成功"，不如直接不提供、让选型时就报错。
//
// 配了有效 cookie 时它是真能用的：连打 6 次服务端回报的都是 "3.1 Pro" 本身，
// 且每次都带思考链（118-152 字符，普通 3.6 Flash 为 0）。早前记录的「免费号
// 登录也只能拿到 3.6 Flash 扩展」是在缺 XSRF token 的条件下测的，那时候带
// cookie 的请求根本发不出去（见 xsrf.go）。
func availableModels() map[string]ModelConfig {
	if hasCookie() {
		return Models
	}
	out := make(map[string]ModelConfig, len(Models))
	for k, v := range Models {
		if modelNeedsLogin(v) {
			continue
		}
		out[k] = v
	}
	return out
}

// resolveModel maps a model name to its config.
//
// "name@think=N" 后缀会被剥掉并忽略。旧版本把它写进 inner[17] 当思考深度，
// 那是误读：抓包显示 inner[17] 是会话内的轮次索引（首轮 [[0]]，带会话 id 的
// 第二轮 [[1]]，逐轮递增），跟思考深度无关。我们每次都开新会话，该值恒为 0。
// 后缀不报错只忽略，避免打断已经配了这个写法的客户端。
func resolveModel(modelName string) (string, ModelConfig, error) {
	if idx := strings.Index(modelName, "@think="); idx >= 0 {
		modelName = modelName[:idx]
	}
	mc, ok := availableModels()[modelName]
	if !ok {
		if full, exists := Models[modelName]; exists && !hasCookie() {
			downgrade := "are silently downgraded to 3.5 Flash-Lite"
			if full.Tool > 0 {
				downgrade = "are silently downgraded to a plain \"Are you signed in?\" text reply"
			}
			return "", ModelConfig{}, fmt.Errorf(
				"%s is unavailable without a Google account cookie: anonymous requests for it "+
					"%s. Add a cookie in the admin panel (Cookie pool) or via --cookie-file to enable it",
				modelName, downgrade)
		}
		return "", ModelConfig{}, fmt.Errorf("unknown model: %s", modelName)
	}
	return modelName, mc, nil
}

// StreamResult holds raw body + per-request proxy + timing info.
type StreamResult struct {
	// Emitted 是流式模式下已经通过 onDelta 发出去的文本；非流式为空。
	Emitted string
	Raw     string
	// Reasoning 是模型的思考链（只有 3.1 Pro 会产出）。
	// 上游每次都发，我们以前只取正文、把它扔了。
	Reasoning string
	// EmittedReasoning 是流式下已经通过 onReasoning 发出去的思考链；非流式为空。
	EmittedReasoning string
	// UpstreamModel 是服务端在响应帧 [42] 里自报的模型显示名。
	// 跟请求的模型未必一致：gemini-3.1-pro 匿名时被静默降级成 3.5 Flash-Lite，
	// 只看请求名根本发现不了，所以这个字段要一直记着。
	UpstreamModel string
	ProxyID       int64
	ProxyName     string
	// 用了 cookie 池里的哪个账号，0 = 匿名。失败的请求也要带上——
	// 排查"加了 cookie 就大面积失败"时，最需要知道的正是失败那条用的哪个号。
	AccountID    int64
	AccountLabel string
	TTFBMs       int64
	TotalMs      int64
	// Artifacts 是媒体模型（生图/音乐）取回的产物原始字节，非媒体模型为空。
	Artifacts []MediaArtifact
	// MediaErr 记媒体产物取回失败的原因：生成本身 200 了、但走 hNvQHb / 下载那步挂了。
	// 调用方据此报错，而不是返回一个只有文字没有图的「半成功」。
	MediaErr string
}

// RateLimitError 表示所有 IP slot 都达到了限流上限。
// HTTP handler 看到这个错时返回 429 给客户端。
type RateLimitError struct {
	Reason  string // "concurrent" / "rpm" / "rph"
	ProxyID int64  // 0 = 直连 slot 满
}

func (e *RateLimitError) Error() string {
	if e.ProxyID == 0 {
		return "direct IP slot full: " + e.Reason + " limit reached (configure proxies to scale)"
	}
	return "all proxy slots full: " + e.Reason + " limit reached"
}

// acquireSlot 选一个有容量的 slot 给本次请求用。
// 优先级：代理池里有容量的代理 → 直连。
// 全满返回 *RateLimitError。
//
// 调用方拿到 (proxy, ok=true) 必须配 deferred releaseSlot()。
func acquireSlot(preferProxyID int64) (Proxy, bool, error) {
	// 1. 先试代理池（只算启用的代理）。
	//
	// Dulu `len(proxyCache) > 0` dipakai sebagai penanda "ada proxy". Pool yang
	// isinya satu proxy tapi sudah dimatikan (enabled=0) tetap dianggap ada,
	// sehingga setiap request masuk jalur proxy, gagal memilih, lalu dilaporkan
	// "rph limit reached" — padahal penghitungnya nol dan jalur langsung sehat.
	// Gejalanya: model berbayar 502 instan sementara akunnya baik-baik saja.
	proxyMu.RLock()
	enabledProxies := 0
	for _, p := range proxyCache {
		if p.Enabled {
			enabledProxies++
		}
	}
	proxyMu.RUnlock()

	if enabledProxies > 0 {
		if p, ok := pickProxyPreferring(preferProxyID); ok {
			return p, true, nil
		}
		// 代理池里有代理但一个都用不上（限流满 / 全禁用 / 全熔断且没过冷却）。
		// 默认**不退回直连** —— 配了代理池就意味着不想让上游看到本机 IP，
		// 悄悄直连等于把这个前提废掉，而且日志上只是几条普通请求，很难发现。
		// 要可用性优先于隐藏 IP 的部署可以打开 fallback_direct。
		// Muat ulang cache sekali sebelum memutuskan: proxy yang baru dimatikan
		// lewat panel/DB masih terhitung enabled sampai proses restart, sehingga
		// setiap request masuk jalur proxy lalu gagal memilih. Dulu hasilnya
		// dilaporkan "rph limit reached" padahal penghitungnya nol.
		loadProxies()
		proxyMu.RLock()
		stillEnabled := 0
		for _, pp := range proxyCache {
			if pp.Enabled {
				stillEnabled++
			}
		}
		proxyMu.RUnlock()
		if stillEnabled > 0 {
			if p, ok := pickProxyPreferring(preferProxyID); ok {
				return p, true, nil
			}
			if !rtCfg().FallbackDirect {
				return Proxy{}, false, &RateLimitError{Reason: "proxy_unavailable", ProxyID: -1}
			}
		}
		if ok, reason := trySlotAcquire(0); ok {
			logf("[proxy] 代理池无可用出口，本次退回直连（fallback_direct 已开）")
			return Proxy{}, true, nil
		} else {
			return Proxy{}, false, &RateLimitError{Reason: reason, ProxyID: 0}
		}
	}

	// 2. 没配代理池 → 用直连 slot（id=0）
	if ok, reason := trySlotAcquire(0); ok {
		return Proxy{}, true, nil // ProxyID=0 表示直连
	} else {
		return Proxy{}, false, &RateLimitError{Reason: reason, ProxyID: 0}
	}
}

// 一个请求最多试几个 cookie。池子大时挨个试到底会让失败请求拖很久，
// 而连试 3 个都不行基本说明是池子整体的问题，不是撞上个别坏号。
const maxCookieTries = 3

// releaseSlot 释放占用。proxyID=0 表示直连。
func releaseSlot(proxyID int64) {
	slotRelease(proxyID)
}

// deltaTracker 把上游的累积帧转成增量。
//
// 上游每帧带的是**到目前为止的全文**，不是新增部分，所以要跟已发出的做前缀
// 比对。帧之间偶尔不满足前缀关系（模型改写、或 clean 掉的 artifact 落在边界
// 上），这时宁可跳过也不能发——发了就等于把重复内容推给客户端，而已发出的
// 内容收不回来。漏掉的部分由调用方在结束时用 remainingText 补齐。
type deltaTracker struct{ emitted string }

// Push 吃进一帧的累积全文，返回相对上一次的增量；没有新增或无法安全 diff 时返回 ""。
func (d *deltaTracker) Push(fullText string) string {
	cleaned := cleanGeminiText(fullText)
	if len(cleaned) <= len(d.emitted) || !strings.HasPrefix(cleaned, d.emitted) {
		return ""
	}
	delta := cleaned[len(d.emitted):]
	d.emitted = cleaned
	return delta
}

// streamGenerate POSTs to Gemini's StreamGenerate endpoint and returns raw body
// plus proxy/timing telemetry for the metrics layer.
// The 80-slot inner array is verbatim from the Python reference.
// onDelta 非 nil 时开启真流式：上游每写一帧就解析一次，跟已发出的内容做前缀
// diff，把新增部分立刻回调出去。上游每帧带的是累积全文而不是增量，diff 必须
// 自己做。一旦已经吐过内容就不再重试——重试会让客户端收到重复文本。
func streamGenerate(prompt, latest string, mc ModelConfig,
	onDelta, onReasoning func(string)) (*StreamResult, error) {
	return streamGenerateWithFiles(nil, prompt, latest, mc, nil, onDelta, onReasoning)
}

// fileRef 是一个已上传附件的引用。
type fileRef struct {
	Ref  string // 上传返回的路径，形如 /contrib_service/ttl_1d/…
	Name string // 展示给模型看的文件名
	Kind int    // 附件类型：1=图片，3=文本/普通文件
	Mime string // 内容类型，服务端按它决定怎么解析附件
}

// streamGenerateWithFiles 同上，但可以带附件。
//
// 附件填 inner[0][3]，形状 [[[ref, 1], "文件名"], …]。附件只在登录态可用：
// 匿名能把文件传上去，但对话里一引用就被服务端回 1100。
func streamGenerateWithFiles(ctx context.Context, prompt, latest string, mc ModelConfig, pending []pendingUpload,
	onDelta, onReasoning func(string)) (*StreamResult, error) {
	var files []fileRef

	// 先挑号，再按它上次绑的出口挑代理 —— 同一个账号要尽量固定从同一个 IP 出去，
	// 否则一个号在几十个出口之间跳，在 Google 眼里就是账号共享的特征。
	//
	// 挑号排在 acquireSlot 之前不违反「取 XSRF 必须走正式出口」：挑号只读库、
	// 不发请求，真正发请求的是下面的 getXSRF，它在拿到 slot 之后。
	// #20 匿名优先：不需要登录态能力时不占用 cookie 账号，走匿名省额度（见 anonFirstEligible）。
	// 带附件（图/视频）时不能走匿名——下面 uploadBytes 那段会因 cookieStr=="" 直接报错。
	// prompt 超过 128 KB 时同样不能走匿名：prepareContextFile 只能靠 cookie 把
	// 超长对话转成文本附件，匿名走这条路必然 502。
	var acct *CookieAccount
	if !anonFirstEligible(mc, len(pending) > 0, len(prompt)) {
		if a, ok := pickCookieAccountFor(mc, nil); ok {
			acct = a
		} else if d, ok := modelDenySoonest(mc); ok && d <= denyWaitBudget() {
			// Semua kandidat cooldown untuk model ini. Cooldown itu heuristik
			// ("Google menyajikan model lain" = jendela kuota akun itu tutup),
			// bukan hukum - dan menunggu sisa terdekat jauh lebih berguna
			// daripada menolak instan, terutama untuk prompt besar yang tidak
			// boleh lewat jalur anonim sama sekali.
			logf("[cookie] %s: semua kandidat cooldown, tunggu %s lalu pilih ulang",
				modelLabel(mc), d.Round(time.Second))
			time.Sleep(d + 200*time.Millisecond)
			if a, ok := pickCookieAccountFor(mc, nil); ok {
				acct = a
			}
		}
	}
	// WAJIB cookie untuk prompt kebesaran — bukan preferensi.
	//
	// prepareContextFile hanya bisa mengubah prompt > batas menjadi lampiran teks
	// kalau ada cookie; jalur anonim pasti ditolak (file yang diunggah tak bisa
	// dirujuk). Jadi kalau di sini acct masih nil, jangan diam-diam lanjut ke
	// anonim dan menunggu 502 dari Google: berhenti dengan pesan yang menyebut
	// sebab sebenarnya.
	if acct == nil {
		budget := rtCfg().MaxPromptBytes
		oversized := budget > 0 && len(prompt) > budget
		if oversized {
			_, enabled := accountCount()
			// Pool yang sedang cooldown BUKAN soal ukuran prompt dan bukan
			// kegagalan permanen: begitu cooldown terdekat lewat, request yang
			// sama berhasil. Karena itu jawab 503 (klien boleh ulang), bukan
			// 400 context_length_exceeded yang dibaca klien sebagai "prompt-nya
			// salah, jangan diulang" - kekeliruan kelas galat itulah yang bikin
			// operator melihat notifikasi galat permanen padahal sementara.
			if enabled > 0 {
				waitSec := 0
				if d, ok := modelDenySoonest(mc); ok {
					waitSec = int(d.Seconds() + 0.5)
				}
				detail := fmt.Sprintf("no signed-in account is free for %s right now: "+
					"the prompt is %d bytes so the anonymous path cannot carry it as an "+
					"attachment, and every enabled account is cooling down after Google "+
					"served a different model for it. This clears by itself - the nearest "+
					"account frees up in about %ds; the same request will go through then.",
					modelLabel(mc), len(prompt), waitSec)
				return &StreamResult{}, &PoolUnavailableError{
					Model: modelLabel(mc), Enabled: enabled, PromptBytes: len(prompt),
					RetryAfterSec: waitSec, Detail: detail,
				}
			}
			why := fmt.Sprintf("no enabled account supports %s", modelLabel(mc))
			msg := fmt.Sprintf("prompt is %d bytes and the anonymous path cannot upload "+
				"attachments, so it can only be sent through a signed-in account; %s. "+
				"Add or re-enable an account in the admin panel (Cookie pool).",
				len(prompt), why)
			return &StreamResult{}, &PromptTooLongError{
				Bytes: len(prompt), Budget: budget, HasCookie: false, Detail: msg,
			}
		}
	}
	// Model yang di jalur anonim PASTI turun kelas — modelnya diganti, dan
	// konteksnya ikut mengecil — tidak boleh dijawab lewat anonim.
	//
	// Ini permintaan operator dan juga logika yang benar: jawaban dari model lain
	// dengan konteks lebih kecil bukan "tetap jalan", itu jawaban salah yang tidak
	// bisa dideteksi klien. Lebih baik gagal dengan sebab yang jelas.
	if acct == nil && modelRequiresCookie(mc) {
		_, enabled := accountCount()
		var why string
		switch {
		case enabled == 0:
			why = "there is no enabled account in the pool"
		default:
			why = fmt.Sprintf("all %d enabled account(s) are unavailable for it "+
				"(cooling down after Google served a different model, or filtered out "+
				"because they lack the required rollout)", enabled)
			if mc.Tool == toolMusic {
				why = fmt.Sprintf("none of the %d enabled account(s) report music quota, so "+
					"Google would answer with a chat model instead of generating audio; "+
					"add or re-enable an account that can create music in the Gemini web UI", enabled)
			}
		}
		return &StreamResult{}, fmt.Errorf("model %s needs a signed-in PAID account: "+
			"the anonymous path silently replaces it with 3.5 Flash-Lite and a much "+
			"smaller context, so it is refused instead of answered with the wrong "+
			"model; %s. Add or re-enable an account in the admin panel (Cookie pool).",
			modelLabel(mc), why)
	}
	preferProxy := int64(0)
	if acct != nil {
		preferProxy = acct.ProxyID
	}

	// 出错时也要把「用了哪个号 / 哪个出口」带回去，否则失败记录里全是空白。
	// picked 是拿到 slot 之后才填的，闭包捕获它，后面每次 attrib 都带上当时的出口。
	var picked Proxy
	var cookieID int64
	var cookieLabel string
	attrib := func(err error) (*StreamResult, error) {
		return &StreamResult{
			AccountID: cookieID, AccountLabel: cookieLabel,
			ProxyID: picked.ID, ProxyName: picked.Name,
		}, err
	}

	// 通过限流器拿一个 slot（代理或直连）。所有 slot 满 → 直接 429。
	p, slotOK, slotErr := acquireSlot(preferProxy)
	if !slotOK {
		return attrib(slotErr)
	}
	picked = p
	defer releaseSlot(picked.ID) // picked.ID=0 表示直连 slot

	// picked.URL 为空 = 直连 slot。代理只有代理池一个入口，没有别的兜底出口了
	// （原来那个「静态代理」字段已并进池子，见 seedProxiesFromConfig）。
	proxyURL := picked.URL
	pickedOK := picked.ID > 0 // 是否真用了代理池里的代理

	// endpoint 要等出口定下来才能拼：currentBL 可能顺手踢一次后台抓取，
	// 那个抓取必须跟正式请求走同一个出口，否则配了代理池也会从本机 IP 漏一次。
	reqid := time.Now().Unix() % 1000000
	endpoint := fmt.Sprintf(
		"https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate?bl=%s&hl=en&_reqid=%d&rt=c",
		currentBL(proxyURL), reqid,
	)

	// 取 XSRF token。一个 cookie 失效不该让整个请求失败：当前号取不到就换下一个，
	// 最多试 maxCookieTries 个。不这么做的话，池子里 2 个号坏 1 个就会让大约一半
	// 请求挂掉，而每次失败看起来都像上游的问题，用户只会觉得"成功率莫名很低"。
	//
	// 换号**不换出口**：出口已经按第一个号的绑定选定了，同一个请求里再换出口没道理。
	cookieStr, sapisid, xsrfToken := "", "", ""
	var lastCookieErr error
	tried := map[int64]bool{}
	// 每个号只给一次「轮转后重试」的机会，避免在一个请求里反复打 accounts.google.com
	rotatedOnce := map[int64]bool{}
	for acct != nil && len(tried) < maxCookieTries {
		tried[acct.ID] = true
		// 归属先记上：这一轮失败了也留痕，面板上看得出是哪个号在坏
		cookieID, cookieLabel = acct.ID, accountDisplayName(acct)
		tok, err := getXSRF(acct.Cookie, proxyURL, ctx)
		if err == nil {
			cookieStr, sapisid, xsrfToken = acct.Cookie, extractSAPISID(acct.Cookie), tok
			// 只在「还没绑过」或「绑的出口已经没了」时写绑定。
			//
			// 绝不因为"这次走的是别的出口"就覆盖：出口是按**本次第一个挑中的号**
			// 的绑定选的，而挑号会换（新加的号 last_used_at=0 排在最前，撞上坏号
			// 就会换）。拿别人的出口覆盖当前号的绑定，等于每次撞上坏号就把好号的
			// 粘性打散一次 —— 实测就是这么散掉的。
			if acct.ProxyID == 0 || !proxyUsableByID(acct.ProxyID) {
				bindAccountProxy(acct.ID, picked.ID)
			}
			break
		}
		// 取不到 SNlM0e 基本等于这个 cookie 已失效（页面把我们当匿名用户了）。
		// 换号之前先给它一次机会：强制轮转一次再重取。
		//
		// 轮转会换发 __Secure-1PSIDTS（约 30 分钟过期的那张票）并合并 *SIDCC。
		// 只是票过期、持久身份还在的号，这一步能救回来；救不回来再换号。
		// 只试一次，且只在这一轮。
		if !rotatedOnce[acct.ID] && acct.RotateDisabled == 0 {
			rotatedOnce[acct.ID] = true
			if _, _, rerr := rotateAccount(*acct); rerr == nil {
				if fresh := accountByID(acct.ID); fresh != nil {
					if tok2, err2 := getXSRF(fresh.Cookie, proxyURL, ctx); err2 == nil {
						logf("[cookie] 账号 #%d 轮转后恢复可用", acct.ID)
						acct = fresh
						cookieStr, sapisid, xsrfToken = fresh.Cookie, extractSAPISID(fresh.Cookie), tok2
						if fresh.ProxyID == 0 || !proxyUsableByID(fresh.ProxyID) {
							bindAccountProxy(fresh.ID, picked.ID)
						}
						break
					}
				}
			}
		}
		// 救不回来：记一次失败让面板上看得出是哪个号该换了，然后换下一个。
		markCookieByStatus(acct.ID, 401, err.Error())
		lastCookieErr = err
		logf("[cookie] 账号 #%d 不可用，换下一个：%v", acct.ID, err)
		acct, _ = pickCookieAccountFor(mc, tried) // 取不到时返回 nil，循环自然结束
	}
	if lastCookieErr != nil && cookieStr == "" {
		if !rtCfg().FallbackAnon {
			// 默认报错而不是降级：cookie 失效后上游不会拒绝，只是把你当匿名用户，
			// 纯文本请求照样 200 —— 于是 3.1 Pro 被静默降级成 3.5 Flash-Lite、
			// 思考链消失，客户端完全看不出来。宁可明确失败也不给假的成功。
			return attrib(fmt.Errorf("cookie 池里 %d 个账号都不可用（最后一个：%w）；"+
				"到面板「Cookie 池」用「检测」按钮逐个排查，或打开 fallback_anon 降级匿名",
				len(tried), lastCookieErr))
		}
		logf("[cookie] 试过的 %d 个账号都不可用，本次降级匿名（能力会退化到匿名档）", len(tried))
		cookieID, cookieLabel = 0, ""
	}
	// 图片附件：上传要 cookie，而且必须走跟正式请求同一个出口，所以排在这里。
	if len(pending) > 0 {
		if cookieStr == "" {
			return attrib(fmt.Errorf("image input needs a Google account cookie: " +
				"anonymous uploads succeed but referencing them in a conversation is " +
				"rejected upstream. Add a cookie in the admin panel (Cookie pool)"))
		}
		nImg, nVid := 0, 0
		for _, u := range pending {
			ref, uerr := uploadBytes(cookieStr, proxyURL, u.Data, u.Name, ctx)
			if uerr != nil {
				return attrib(fmt.Errorf("上传附件 %s 失败: %w", u.Name, uerr))
			}
			files = append(files, fileRef{Ref: ref, Name: u.Name, Kind: u.Kind, Mime: u.Mime})
			if u.Kind == 2 {
				nVid++
			} else {
				nImg++
			}
		}
		logf("[vision] 上传了 %d 张图 / %d 个视频", nImg, nVid)
	}

	// prompt 超长时转成文本附件。要等挑完号和出口才能做：上传要 cookie，
	// 而且必须走跟正式请求同一个出口。
	budget := rtCfg().MaxPromptBytes
	if p, f, used, ferr := prepareContextFile(prompt, latest, budget, cookieStr, proxyURL, ctx); ferr != nil {
		return attrib(ferr)
	} else if used {
		prompt = p
		files = append(files, f...)
	}
	if budget > 0 && len(prompt) > budget {
		return attrib(&PromptTooLongError{
			Bytes: len(prompt), Budget: budget, HasCookie: cookieStr != "",
		})
	}

	inner := make([]interface{}, innerSlots)
	if len(files) > 0 {
		// 形状逐字取自浏览器抓包：
		//   [[[路径, 类型, null, mime], "文件名", null×6, [0]], …]
		// 类型位是 1=图片 / 3=文本文件 —— 拿 1 传文本文件等于告诉服务端"这是张图"。
		refs := make([]interface{}, 0, len(files))
		for _, f := range files {
			kind := f.Kind
			if kind == 0 {
				kind = 3
			}
			mime := f.Mime
			if mime == "" {
				mime = "text/plain"
			}
			refs = append(refs, []interface{}{
				[]interface{}{f.Ref, kind, nil, mime}, f.Name,
				nil, nil, nil, nil, nil, nil,
				[]interface{}{0},
			})
		}
		inner[0] = []interface{}{prompt, 0, nil, refs, nil, nil, 0}
	} else {
		inner[0] = []interface{}{prompt, 0, nil, nil, nil, nil, 0}
	}
	inner[1] = []interface{}{"en"}
	inner[2] = []interface{}{"", "", "", nil, nil, nil, nil, nil, nil, ""}
	inner[6] = []interface{}{0}
	inner[7] = 1
	inner[10] = 1
	inner[11] = 0
	// 会话内轮次索引；我们每次都是新会话，恒为 0。
	inner[17] = []interface{}{[]interface{}{0}}
	inner[18] = 0
	inner[27] = 1
	inner[30] = []interface{}{4}
	// 抓包里浏览器三种场景（有 cookie / 无 cookie / 扩展思考）全是 [1]。
	// 我们原来写 [2]，是早期抄来的值、协议层已被证伪。含义仍未知，
	// 匿名两个值都能通，但没有理由继续偏离浏览器。
	inner[41] = []interface{}{1}
	// 临时对话（#34）：inner[45]=1 让这条不进账号的会话列表、不入训练。只在有登录
	// cookie 时发——匿名本就不落账号，发了也没意义且没验过。multi_turn 开着时
	// temporaryChatEligible 直接为假（两者冲突，见那里）。媒体工具一律不发：
	// 产物补救要读 hNvQHb 历史，临时对话没有历史（见 temporaryChatEligibleFor）。
	if temporaryChatEligibleFor(mc.Tool) && cookieStr != "" {
		inner[45] = 1
	}
	inner[53] = 0
	reqUUID := uuid.NewString()
	inner[59] = reqUUID
	inner[61] = []interface{}{}
	inner[68] = 1
	inner[79] = mc.Mode
	inner[80] = thinkingNormal
	inner[91] = 0
	inner[96] = 0
	if mc.Thinking {
		inner[80] = thinkingExtended
		inner[96] = 1
	}
	// 媒体工具开关。填了服务端就换后端模型出图/出乐（响应里带产物引用，字节要另取）。
	if mc.Tool > 0 {
		inner[49] = mc.Tool
	}
	// 视频还要在 inner[55] 指定画幅比例：[[16]]=16:9，[[17]]=9:16（抓包）。
	if mc.Tool == toolVideo {
		inner[55] = []interface{}{[]interface{}{16}}
	}

	innerJSON, err := json.Marshal(inner)
	if err != nil {
		return nil, err
	}
	outer := []interface{}{nil, string(innerJSON)}
	outerJSON, err := json.Marshal(outer)
	if err != nil {
		return nil, err
	}

	// 带 cookie 时必须多发一个表单字段 at（XSRF token），否则上游直接 400。
	// 匿名请求不需要，getXSRF 对空 cookie 返回空串。见 xsrf.go。
	buildBody := func(at string) string {
		form := url.Values{}
		form.Set("f.req", string(outerJSON))
		if at != "" {
			form.Set("at", at)
		}
		return form.Encode()
	}

	body := buildBody(xsrfToken)

	thinkVal := thinkingNormal
	if mc.Thinking {
		thinkVal = thinkingExtended
	}
	// 抓包里 header 下标 16 和 inner[59] 是两个不同的 uuid，各生成各的。
	modelHeader := buildModelHeader(mc.HexID, mc.Mode, thinkVal, mc.ThinkLevel, uuid.NewString())
	sessionHeader := fmt.Sprintf(`["%s",1]`, reqUUID)

	geminiHeaders := buildGeminiHeaders(cookieStr, sapisid, mc.HexID)
	endpoint = geminiURLWithAuthUser(endpoint, cookieStr)
	geminiHeaders["x-goog-ext-525001261-jspb"] = modelHeader
	geminiHeaders["x-goog-ext-525005358-jspb"] = sessionHeader
	var lastErr error
	// 最后一次拿到的 HTTP 状态码，0 表示网络层就失败了没拿到响应。
	// cookie 健康度只认 401/403，别的状态不算 cookie 的错，见 markCookieByStatus。
	lastStatus := 0
	xsrfRetried := false // XSRF 自愈只做一次，避免死循环
	t0 := time.Now()

	tracker := &deltaTracker{}
	rtracker := &deltaTracker{}
	var lineCB func(string)
	if onDelta != nil || onReasoning != nil {
		lineCB = func(line string) {
			// 思考链先推完才轮到正文，所以先处理它，客户端拿到的顺序才对。
			if onReasoning != nil {
				if r := reasoningInLine(line); r != "" {
					if d := rtracker.Push(r); d != "" {
						onReasoning(d)
					}
				}
			}
			if onDelta == nil {
				return
			}
			for _, t := range textsInLine(line) {
				if d := tracker.Push(t); d != "" {
					onDelta(d)
				}
			}
		}
	}

	// Model media (video/music/image/canvas) tidak di-retry berulang.
	// Satu job yang gagal lalu diulang 3x membuat Google melihat pola
	// request gagal berturut-turut ke endpoint media - itu pemicu utama
	// sesi di-revoke. Model teks tetap dapat retry penuh karena murah
	// dan cepat.
	maxAttempts := rtCfg().RetryAttempts
	if mc.Tool > 0 && maxAttempts > 1 {
		maxAttempts = 1
	}
	// Sidik jari harus cocok dengan browser yang membuat sesi ini: cookie dari
	// Firefox dikirim dengan profil Firefox. Tanpa ini Google melihat "sesi
	// dibuat di perangkat A, dipakai dari perangkat B" dan mencabutnya.
	if cookieID > 0 {
		if prof := impersonateForAccount(cookieID); prof != "" {
			ctx = withTLSProfile(ctx, prof)
		}
	}

	// Penolakan generik ("I'm having a hard time fulfilling your request") adalah
	// beban sesaat di sisi Google, bukan kuota dan bukan akses. Berpindah akun
	// membantu, tapi tiap perpindahan berarti satu perjalanan penuh ke Google
	// (10-20 detik). Penghitung terpisah ini yang membatasinya.
	//
	// Dulu batasnya memakai `attempt`, tapi blok pindah-akun menulis `attempt--`
	// supaya perulangan tidak menghabiskan jatah retry — efek sampingnya syarat
	// `attempt < maxRefusalRetries-1` SELALU benar, sehingga satu penolakan
	// berjalan melewati SEMUA akun di pool. Terukur: 11 akun x ~20 detik = 223
	// detik untuk satu permintaan yang akhirnya tetap gagal. Sebelum jumlah akun
	// bertambah, gejala ini tidak kelihatan karena perjalanannya pendek.
	refusalSwitches := 0
	const maxRefusalAccountSwitches = 2

	for attempt := 0; attempt < maxAttempts; attempt++ {
		statusCode, raw, ttfb, setCookie, err := doGeminiRequest(endpoint, body, geminiHeaders, proxyURL, lineCB, mc.Tool, ctx)
		if len(setCookie) > 0 && cookieID > 0 {
			if merged := mergeSetCookie(cookieStr, setCookie); merged != cookieStr {
				cookieStr = merged
				updateAccountCookie(cookieID, merged)
			}
		}
		if err != nil {
			lastErr = err
			if pickedOK {
				recordProxyResult(picked.ID, false, err.Error())
			}
			// 已经往客户端吐过内容就不能重试，否则会重复（思考链也算吐过）。
			if tracker.emitted != "" || rtracker.emitted != "" {
				break
			}
			// Stall = akun/exit ini yang berhenti dilayani, bukan request-nya
			// yang salah. Mengulang di akun yang sama hampir pasti macet lagi
			// dan membuang seluruh jatah retry. Pindah akun dulu; baru kalau
			// tidak ada akun lain, ulangi di tempat yang sama.
			var stall *errUpstreamStall
			if errors.As(err, &stall) {
				if switched := switchToAccountSupporting(mc, tried, &acct, &cookieStr,
					&cookieID, &cookieLabel, &sapisid, &xsrfToken); switched {
					logf("[model] %s macet di akun sebelumnya (%v), pindah ke akun #%d",
						modelLabel(mc), stall, cookieID)
					lastErr = nil
					attempt--
					continue
				}
			}
			if attempt < maxAttempts-1 {
				logf("retry %d/%d: %v", attempt+1, rtCfg().RetryAttempts, err)
				time.Sleep(time.Duration(rtCfg().RetryDelaySec) * time.Second)
			}
			continue
		}
		if statusCode != 200 {
			// token 过期时上游回 400 + xsrf. Dua jalur, dari yang termurah:
			//
			//  1. Token pengganti sudah IKUT di respons error -> pakai langsung.
			//     Nol round-trip tambahan. Ini yang membuat cookie basi pulih
			//     dalam satu retry, bukan lewat fetch /app yang justru sering
			//     kena 429 saat semua request sedang gagal.
			//  2. Tidak ada token di respons -> baru fetch ulang /app.
			//
			// Keduanya tidak memakan jatah retry: sekali token basi bukan
			// kegagalan request, dan menghabiskannya membuat request sehat
			// ikut mati bersama token lamanya.
			if statusCode == 400 && isXSRFError(string(raw)) && cookieStr != "" && !xsrfRetried {
				xsrfRetried = true
				invalidateXSRF(cookieStr)
				tok, src := cacheXSRFFromError(cookieStr, string(raw)), "dari respons error"
				if tok == "" {
					if t, e := getXSRF(cookieStr, proxyURL, ctx); e == nil {
						tok, src = t, "dari /app"
					}
				}
				if tok != "" {
					logf("[xsrf] %s: token diperbarui %s, ulangi tanpa memakai jatah retry",
						modelLabel(mc), src)
					// Akun yang bisa memulihkan tokennya sendiri jelas HIDUP.
					// Tanpa ini akun tetap "disabled" dari kegagalan sebelumnya
					// walaupun request berikutnya sukses, dan pool pelan-pelan
					// habis karena penanda mati tidak pernah dicabut.
					reviveAccountAfterSuccess(cookieID)
					body = buildBody(tok)
					geminiHeaders = buildGeminiHeaders(cookieStr, sapisid, mc.HexID)
					endpoint = geminiURLWithAuthUser(endpoint, cookieStr)
					geminiHeaders["x-goog-ext-525001261-jspb"] = modelHeader
					geminiHeaders["x-goog-ext-525005358-jspb"] = sessionHeader
					attempt--
					continue
				}
			}
			lastErr = fmt.Errorf("upstream HTTP %d: %s", statusCode, truncate(string(raw), 200))
			lastStatus = statusCode
			if pickedOK {
				recordProxyResult(picked.ID, false, lastErr.Error())
			}
			if tracker.emitted != "" || rtracker.emitted != "" {
				break
			}
			if attempt < maxAttempts-1 {
				time.Sleep(time.Duration(rtCfg().RetryDelaySec) * time.Second)
			}
			continue
		}
		// HTTP 200 但一个内容帧都没有 —— 上游的瞬时拒绝（响应里那个 1155）。
		// 它不是限流：干净 IP 间隔 1s 连打 15 次全过、同 IP 并发 10 共 18 次全过、
		// 打了 60+ 次的 IP 之后照样成功，没有可预测阈值，同样的请求有时成功有时失败。
		// 重发一次通常就好，所以必须纳入重试——不然一次抖动就变成客户端可见的 502。
		//
		// 判据是**有没有内容帧**，不是 BardErrorInfo：正常响应的结束帧里也带错误码
		// （1096 = 会话未持久化），拿它判错会把每个正常响应都判成失败。
		// Video (Veo) dan musik (Lyria) bersifat ASINKRON: StreamGenerate hanya
		// mengembalikan "I'm generating your ..." + job id, lalu artefak diambil
		// lewat polling hNvQHb. Jadi untuk keduanya, tidak adanya content frame
		// BUKAN kegagalan - itu respons normal. Cukup pastikan raw tidak kosong.
		// (Dulu hanya video yang dikecualikan, sehingga setiap permintaan musik
		// gagal dengan "no content frame" padahal lagunya benar-benar dibuat.)
		noContent := !hasContentFrame(string(raw))
		if (mc.Tool == toolVideo || mc.Tool == toolMusic) && len(raw) > 0 {
			noContent = false
		}
		if noContent {
			lastErr = fmt.Errorf("upstream returned no content frame (raw %d bytes)", len(raw))
			if pickedOK {
				// 记进代理健康度：1155 跟出口质量强相关（干净出口 60+ 次 0 发生，
				// 脏出口一天约 9 次），连续踩中说明这个出口该歇了。
				recordProxyResult(picked.ID, false, lastErr.Error())
			}
			if tracker.emitted != "" || rtracker.emitted != "" {
				break
			}
			if attempt < maxAttempts-1 {
				logf("retry %d/%d: 空响应（无内容帧，%d 字节）", attempt+1, rtCfg().RetryAttempts, len(raw))
				time.Sleep(time.Duration(rtCfg().RetryDelaySec) * time.Second)
			}
			continue
		}
		// Deteksi downgrade: Google kadang menerima request tapi menyajikan model
		// lain (video/canvas/argon turun ke chat biasa). Kalau itu terjadi, jangan
		// lanjut ke pipeline media - gagal cepat dengan pesan yang jelas, bukan
		// menggantung 90 detik lalu 502.
		// Canvas sengaja memakai model chat (hexFlash36) dan mengembalikan HTML
		// inline, bukan artefak media. Jadi deteksi downgrade tidak berlaku:
		// kalau ikut diperiksa, canvas selalu ditolak karena "served 3.8 Flash".
		if mc.Tool > 0 && mc.Tool != toolCanvas {
			// Tiga kondisi yang dulu disamakan jadi satu pesan:
			//   1. kuota habis      -> Google balas "limit resets", tanpa artefak
			//   2. tak punya akses  -> model turun ke chat biasa (3.x Flash)
			//   3. jalan            -> lanjut ke pipeline media
			if reason := mediaQuotaExhausted(string(raw)); reason != "" {
				// Untuk video, "limit resets" hampir selalu berarti job sebelumnya
				// masih dirender Google - bukan kuota nol. Retry justru memperkuat
				// penolakan, jadi serahkan ke pipeline polling di bawah.
				if mc.Tool == toolVideo {
					logf("[media] video: Google bilang limit, lanjut polling job yang ada")
				} else {
					lastErr = fmt.Errorf("%s quota exhausted: %s", modelLabel(mc), reason)
					if pickedOK {
						recordProxyResult(picked.ID, false, lastErr.Error())
					}
					// Kuota habis bukan kegagalan sementara: akun ini tidak akan
					// berhasil untuk model ini hari ini. Jangan retry akun yang sama
					// (buang-buang waktu) - pindah ke akun berikutnya di pool.
					if switched := switchToAccountSupporting(mc, tried, &acct, &cookieStr,
						&cookieID, &cookieLabel, &sapisid, &xsrfToken); switched {
						lastErr = nil
						attempt--
						continue
					}
					lastErr = fmt.Errorf("%s quota exhausted on all %d account(s): %s",
						modelLabel(mc), len(tried), reason)
					break
				}
			}
		}
		// Deteksi downgrade berlaku untuk model media MAUPUN teks: Google tidak
		// menolak request yang akunnya tidak berhak, dia menyajikan model lain
		// dengan status 200. Untuk model teks ini dulu lolos tanpa terdeteksi,
		// jadi klien menerima "sukses" dengan model yang salah.
		if served := extractUpstreamModel(string(raw)); served != "" && !modelServedMatches(mc, served) {
			lastErr = fmt.Errorf(
				"upstream downgraded: requested %s but Google served %q "+
					"(this account has no access to that model)", modelLabel(mc), served)
			if pickedOK {
				recordProxyResult(picked.ID, false, lastErr.Error())
			}
			// Akun ini sehat, hanya tidak berhak atas model tersebut. Jangan
			// tandai sebagai gagal (bukan salah cookie), cukup pindah akun.
			markCookieByStatus(cookieID, 200, "")
			// Catat supaya permintaan berikutnya tidak mengulang perjalanan
			// 10-20 detik ke akun yang sama untuk hasil yang sama.
			markModelDenied(cookieID, mc)
			if switched := switchToAccountSupporting(mc, tried, &acct, &cookieStr,
				&cookieID, &cookieLabel, &sapisid, &xsrfToken); switched {
				logf("[model] %s ditolak akun #%d (disajikan %q), pindah akun",
					modelLabel(mc), cookieID, served)
				lastErr = nil
				attempt--
				continue
			}
			if mc.Tool > 0 {
				// Model media tidak di-retry: mengulang job media berturut-turut
				// ke akun yang sama adalah pemicu utama sesi di-revoke.
				break
			}
			// Tidak ada akun lain yang bisa melayani model ini. Berhenti di sini.
			//
			// Mengulang akun yang sama tidak akan berubah hasilnya: penolakan
			// model itu keputusan akses Google untuk (akun, model) tersebut, bukan
			// gangguan sesaat. Terukur: percobaan Argon 150 KB pernah berjalan
			// 673 detik hanya untuk berakhir dengan penolakan yang sama, karena
			// loop mengulang akun yang sudah ditandai. Dengan berhenti di sini
			// klien menerima sebabnya dalam hitungan detik, dan cooldown 5 menit
			// menjaga request berikutnya juga tidak mengulanginya.
			logf("[model] %s: tidak ada akun lain yang berhak, berhenti (disajikan %q)",
				modelLabel(mc), served)
			return attrib(lastErr)
		}
		// Jawaban yang isinya penolakan generik: HTTP 200, nama model benar,
		// tapi bukan jawaban. Ini kelas kegagalan tersendiri — bukan kuota
		// (tidak menyebut "limit resets"), bukan downgrade (nama model cocok).
		// Sebelum ini klien menerimanya sebagai sukses dan mengira modelnya
		// rusak.
		//
		// Percobaan ulang DIBATASI 2, bukan memakai seluruh jatah retry: satu
		// penolakan butuh 30-140 detik, jadi 3 percobaan bisa menembus 420
		// detik dan klien yang timeout lebih dulu tidak pernah menerima apa
		// pun — termasuk kalau percobaan ketiga sebenarnya berhasil.
		if mc.Tool == 0 && cannedRefusal(extractResponseText(string(raw))) {
			lastErr = fmt.Errorf("upstream returned a canned refusal instead of an answer "+
				"(model %s, akun %s); this is transient on Google's side, not a quota or access problem",
				modelLabel(mc), cookieLabel)
			if pickedOK {
				recordProxyResult(picked.ID, false, lastErr.Error())
			}
			// Penolakan generik biasanya menempel pada satu akun pada satu
			// waktu, bukan pada request-nya. Mengulang di akun yang sama sering
			// ditolak lagi; pindah akun dulu - tapi terbatas, lihat
			// maxRefusalAccountSwitches.
			if refusalSwitches < maxRefusalAccountSwitches {
				if switched := switchToAccountSupporting(mc, tried, &acct, &cookieStr,
					&cookieID, &cookieLabel, &sapisid, &xsrfToken); switched {
					refusalSwitches++
					logf("[model] %s ditolak generik di akun sebelumnya, pindah ke akun #%d (%d dari maks %d)",
						modelLabel(mc), cookieID, refusalSwitches, maxRefusalAccountSwitches)
					lastErr = nil
					continue
				}
			}
			if attempt < maxAttempts-1 && attempt < maxRefusalRetries-1 {
				logf("retry %d/%d: jawaban penolakan generik, ulangi", attempt+1, maxAttempts)
				time.Sleep(time.Duration(rtCfg().RetryDelaySec) * time.Second)
				continue
			}
			// Sudah cukup dicoba. JANGAN diteruskan sebagai 200.
			//
			// Dulu di sini penolakan dikembalikan apa adanya dengan alasan "klien
			// tetap dapat sesuatu". Itu keliru dan bertentangan dengan aturan yang
			// sama di jalur anonim/downgrade: kalimat seperti "I encountered an
			// error doing what you asked" BUKAN jawaban. Klien agentik akan
			// memperlakukannya sebagai hasil sah, lalu melanjutkan dengan asumsi
			// yang salah — dan tidak ada cara membedakannya dari jawaban benar.
			// 502 dengan sebab yang jelas membuat klien bisa retry atau pindah
			// model, dan dashboard mencatatnya sebagai gagal, bukan sukses.
			logf("[model] %s: penolakan generik setelah %d percobaan, dilaporkan sebagai gagal",
				modelLabel(mc), attempt+1)
			return attrib(lastErr)
		}
		if pickedOK {
			recordProxyResult(picked.ID, true, "")
		}
		markCookieByStatus(cookieID, 200, "")
		// Sukses = bukti hidup, cabut juga penanda mati yang mungkin tersisa
		// dari kegagalan sebelumnya. markCookieByStatus(200) sudah me-reset
		// fail_count, tapi TIDAK menyentuh status; tanpa baris ini akun yang
		// pernah di-disable tetap tersaring walaupun request-nya sukses.
		reviveAccountAfterSuccess(cookieID)
		result := &StreamResult{
			Emitted:          tracker.emitted,
			EmittedReasoning: rtracker.emitted,
			Raw:              string(raw),
			Reasoning:        extractReasoning(string(raw)),
			UpstreamModel:    extractUpstreamModel(string(raw)),
			ProxyID:          picked.ID,
			ProxyName:        picked.Name,
			AccountID:        cookieID,
			AccountLabel:     cookieLabel,
			TTFBMs:           ttfb,
			TotalMs:          time.Since(t0).Milliseconds(),
		}
		// 媒体模型：生成的产物字节不在这条响应里，要用同一套 cookie / 出口再走一遍
		// hNvQHb + 下载 host 取回。取不到就记 MediaErr，让上层报错而不是返回半成品。
		if mc.Tool == toolImage || mc.Tool == toolMusic || mc.Tool == toolVideo {
			mime := "image/png"
			switch mc.Tool {
			case toolMusic:
				mime = "audio/mpeg"
			case toolVideo:
				mime = "video/mp4"
			}
			// Batas keras: media tidak boleh menggantung tanpa akhir. Kalau
			// upstream lambat atau akun tidak berhak, klien harus dapat error
			// cepat, bukan koneksi yang menggantung berjam-jam.
			mediaTimeout := 120 * time.Second
			if mc.Tool == toolMusic {
				// Harus melebihi total polling musik (30 x 5s = 150s).
				mediaTimeout = 600 * time.Second
			}
			if mc.Tool == toolVideo {
				// Harus lebih besar dari total polling video (48 x 10s = 480s),
				// kalau tidak batas ini yang memotong lebih dulu.
				mediaTimeout = 960 * time.Second
			}
			type mediaResult struct {
				arts []MediaArtifact
				err  error
			}
			done := make(chan mediaResult, 1)
			go func() {
				a, e := fetchMediaArtifacts(
					mc.Tool, string(raw), extractConversationID(string(raw)),
					cookieStr, sapisid, xsrfToken, proxyURL, mime)
				done <- mediaResult{a, e}
			}()
			var arts []MediaArtifact
			var aerr error
			select {
			case r := <-done:
				arts, aerr = r.arts, r.err
			case <-time.After(mediaTimeout):
				aerr = fmt.Errorf("media retrieval timed out after %s (upstream did not deliver the artifact; a free account usually cannot render %s)",
					mediaTimeout, mime)
			}
			if aerr != nil {
				logf("[media] 取回产物失败: %v", aerr)
				result.MediaErr = aerr.Error()
			} else {
				result.Artifacts = arts
				logf("[media] 取回 %d 份产物", len(arts))
			}
		}
		// #19 自动删会话：出完结果把 gemini.google.com 上留下的这条会话删掉，避免
		// 用户账号里堆一堆。只登录态能删（要 XSRF），异步 best-effort，不影响响应。
		//
		// 临时对话（#34）生效时跳过：那条压根没进会话列表，删它是多发一次 RPC 删空气。
		// 这正是临时对话比 create-then-delete 省的那一次出站请求。
		if rtCfg().AutoDeleteConversation && !temporaryChatEligibleFor(mc.Tool) && cookieStr != "" && xsrfToken != "" {
			if cid := extractConversationID(string(raw)); cid != "" {
				go deleteConversation(cid, cookieStr, sapisid, xsrfToken, proxyURL)
			}
		}
		return result, nil
	}
	if lastErr != nil {
		markCookieByStatus(cookieID, lastStatus, lastErr.Error())
	}
	return attrib(lastErr)
}

// upstreamModelRe 匹配响应帧里服务端自报的模型显示名（帧的 [42] 位）。
// 形如 ...,"fbb127bbb056c959",null,null,"3.6 Flash",true,...
var quotaResetRe = regexp.MustCompile(`(?i)(available again[^".\\]{0,70}|resets? (?:in|on|at)[^".\\]{0,50})`)

var upstreamModelRe = regexp.MustCompile(`\\"[0-9a-f]{16}\\",null,null,\\"([^"\\]{1,40})\\"`)

// extractUpstreamModel 取服务端实际使用的模型名，取不到返回空串。
func extractUpstreamModel(raw string) string {
	m := upstreamModelRe.FindAllStringSubmatch(raw, -1)
	if len(m) == 0 {
		return ""
	}
	return m[len(m)-1][1]
}

// mediaQuotaExhausted mendeteksi respons Google yang menyatakan jatah media
// sudah habis (bukan karena akun tidak punya akses). Google mengirim teks
// seperti "I can create more videos as soon as your limit resets" tanpa
// artefak apa pun; tanpa deteksi ini kasusnya salah dikira downgrade.
func mediaQuotaExhausted(raw string) string {
	low := strings.ToLower(raw)
	markers := []string{
		"limit resets", "limit reset", "as soon as your limit",
		"check your usage in settings", "reached your limit",
		"out of videos", "out of images", "out of video",
	}
	hit := false
	for _, m := range markers {
		if strings.Contains(low, m) {
			hit = true
			break
		}
	}
	if !hit {
		return ""
	}
	if m := quotaResetRe.FindString(raw); m != "" {
		return m
	}
	return "Google reported the media quota is used up"
}

// modelLabel mengembalikan nama model yang diminta, untuk pesan error.
// Pencocokan harus menyertakan Thinking: gemini-4-argon dan
// gemini-4-argon-thinking punya HexID/Tool/ThinkLevel yang sama, jadi tanpa
// pembeda itu pesan error bisa menyebut model yang tidak pernah diminta.
func modelLabel(mc ModelConfig) string {
	for name, m := range Models {
		if m.HexID == mc.HexID && m.Tool == mc.Tool &&
			m.ThinkLevel == mc.ThinkLevel && m.Thinking == mc.Thinking {
			return name
		}
	}
	for name, m := range Models {
		if m.HexID == mc.HexID && m.Tool == mc.Tool && m.ThinkLevel == mc.ThinkLevel {
			return name
		}
	}
	return mc.HexID
}

// modelServedMatches memeriksa apakah model yang benar-benar disajikan Google
// sesuai dengan yang diminta. Untuk model tool (video/canvas/image/music),
// nama yang disajikan biasanya nama model chat -> itu tanda downgrade.
//
// Untuk model teks, Google tidak menolak permintaan yang akunnya tidak berhak;
// dia menjawab 200 dengan model yang lebih rendah (Argon -> 3.5 Flash-Lite).
// Tanpa pemeriksaan ini klien menerima "sukses" dengan model yang salah dan
// tidak ada satu pun sinyal yang memberitahunya.
func modelServedMatches(mc ModelConfig, served string) bool {
	low := strings.ToLower(served)
	switch mc.Tool {
	case toolVideo:
		return strings.Contains(low, "veo") || strings.Contains(low, "video")
	case toolImage:
		return strings.Contains(low, "nano") || strings.Contains(low, "image") ||
			strings.Contains(low, "banana")
	case toolMusic:
		// Musik BUKAN kasus seperti canvas: saat lagunya benar-benar dibuat,
		// Google melaporkan "Lyria 3.5" (terverifikasi di tabel requests).
		// Penolakan "3.6 Flash" berarti akun itu memang tidak punya akses saat
		// itu, jadi gate ini berguna -- biarkan gagal cepat lalu pindah akun,
		// bukan menggantung sampai polling habis.
		return strings.Contains(low, "lyria") || strings.Contains(low, "music")
	case toolCanvas:
		// Canvas sengaja memakai model chat (hexFlash36) dan mengembalikan HTML
		// inline — Google memang melaporkan "3.6/3.8 Flash", bukan "canvas".
		// Menilai dari label model membuat canvas SELALU ditolak. Satu-satunya
		// penanda yang sah untuk canvas adalah isi HTML-nya, yang diperiksa
		// terpisah di pipeline media.
		return true
	}
	// Model teks: cocokkan kelasnya, bukan string persisnya — Google menambahkan
	// sufiks tingkat penalaran ("4 Argon High", "3.8 Flash High") dan
	// menghilangkan spasi pada sebagian label.
	norm := strings.ReplaceAll(low, " ", "")
	switch mc.HexID {
	case hexArgon4:
		return strings.Contains(norm, "argon")
	case hexFlash38:
		// 3.8 dan 3.7 berbagi hex yang sama; keduanya diterima.
		return strings.Contains(norm, "3.8flash") || strings.Contains(norm, "3.7flash")
	case hexPro31:
		return strings.Contains(norm, "3.1pro")
	case hexFlash36:
		// 3.6 diminta, tapi Google menaikkan ke 3.8 di akun yang punya rollout
		// lebih baru. Itu bukan penurunan kelas: 3.8 lebih tinggi dari 3.6 dan
		// konteksnya tidak mengecil, jadi diterima — nama model yang benar-benar
		// dipakai tetap tercatat di kolom upstream_model.
		//
		// Yang tetap ditolak adalah penurunan nyata (3.5 Flash-Lite), karena di
		// situ konteks dan kemampuannya benar-benar berkurang.
		return strings.Contains(norm, "3.6flash") ||
			strings.Contains(norm, "3.8flash") || strings.Contains(norm, "3.7flash")
	case hexFlashLite:
		return strings.Contains(norm, "flash-lite") || strings.Contains(norm, "flashlite")
	}
	return true
}

// switchToAccountSupporting memindahkan request yang sedang berjalan ke akun
// lain yang menurut probe benar-benar mendukung model ini.
//
// Dipakai dua tempat: saat kuota media habis, dan saat Google menyajikan model
// yang berbeda (downgrade). Keduanya berarti "akun ini bukan akun yang tepat",
// bukan "request ini salah". Mengembalikan false kalau tidak ada akun lain,
// sehingga pemanggil bisa berhenti dengan pesan yang jelas.
func switchToAccountSupporting(mc ModelConfig, tried map[int64]bool,
	acct **CookieAccount, cookieStr *string, cookieID *int64,
	cookieLabel *string, sapisid *string, xsrfToken *string) bool {
	if *cookieID > 0 {
		tried[*cookieID] = true
	}
	next, ok := pickCookieAccountFor(mc, tried)
	if !ok {
		return false
	}
	*acct = next
	*cookieStr = next.Cookie
	*cookieID = next.ID
	*cookieLabel = next.Label
	*sapisid = extractSAPISID(next.Cookie)
	// XSRF harus diambil ulang untuk akun baru. Kalau dikosongkan, hNvQHb
	// (riwayat percakapan, dipakai untuk mengambil ulang link produk media)
	// dijawab HTTP 400 "xsrf" — dan itu mematikan satu-satunya jalur
	// penyelamatan saat link CDN dari StreamGenerate ditolak.
	*xsrfToken = ""
	if tok, err := getXSRF(next.Cookie, proxyURLByID(next.ProxyID)); err == nil {
		*xsrfToken = tok
	}
	return true
}

// maxRefusalRetries membatasi berapa kali jawaban "penolakan generik" diulang.
// Satu penolakan butuh 30-140 detik, jadi 3 percobaan bisa melewati 420 detik
// dan klien timeout sebelum menerima apa pun. Dua percobaan cukup: kalau dua
// kali tetap ditolak, mengulang lagi hampir selalu sia-sia.
const maxRefusalRetries = 2

// cannedRefusalMarkers adalah kalimat penolakan generik Google. Respons seperti
// ini datang dengan HTTP 200 dan nama model yang benar, jadi tanpa deteksi
// khusus klien menerima "sukses" yang isinya bukan jawaban.
//
// Ini BUKAN kuota habis (bedanya: kuota menyebut "limit resets") dan bukan
// downgrade (nama modelnya sesuai). Penyebabnya beban/penolakan sesaat di sisi
// Google; satu percobaan ulang biasanya berhasil.
var cannedRefusalMarkers = []string{
	"having a hard time fulfilling your request",
	"something went wrong. please try your request again",
	"encountered an error doing what you asked",
	"seem to be encountering an error",
	"i'm having a hard time",
}

// cannedRefusal mendeteksi jawaban yang sebenarnya penolakan generik.
func cannedRefusal(text string) bool {
	low := strings.ToLower(strings.TrimSpace(text))
	if len(low) > 400 {
		// Jawaban asli bisa panjang; penolakan selalu pendek.
		return false
	}
	for _, m := range cannedRefusalMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// buildModelHeader 拼 x-goog-ext-525001261-jspb，形状逐槽取自抓包：
//
//	[1,null,null,null,"<hex>",null,null,0,[4,5,6,8],null,null,1,null,null,<mode>,<think>,"<uuid>"]
//	下标                4                8                          14      15       16
//
// 下标 14 跟 inner[79] 同值、下标 15 跟 inner[80] 同值 —— 模型和思考模式在 header 和
// payload 里各存一份。**服务端认的是 header**：只填 inner[80]=2 而 header 留最小形式，
// 三个模型实测思考链全是 0 字符；这跟模型选择本身"header 压过 inner[79]"是同一个规律。
//
// 下标 16 是**另一个** uuid，跟 inner[59] 不是一个值 —— 跟 inner[59] 同值的是
// x-goog-ext-525005358-jspb。两份抓包都是这个规律，别图省事复用同一个。
//
// uuid 留空时退回最小形式（匿名路径不需要这些槽位，少发一截更省事）。
func buildModelHeader(hexID string, mode, think, thinkLevel int, uuid string) string {
	// Argon WAJIB pakai format generik ber-uuid, sama seperti model lain.
	// Uji langsung ke akun berbayar (nova-ff2), empat format dibandingkan:
	//   generik + uuid + mode8 + think1 -> "4 Argon", jawaban lengkap
	//   caps [4,5,6,8,16,19,20] + level -> "4 Argon", isi: "Sorry, something went wrong"
	//   caps + level2                   -> "4 Argon High", isi: error
	// Format caps memang membuat Google MELAPORKAN "4 Argon", tapi generasinya
	// gagal — jebakan yang sempat menipu: served benar, isi rusak.
	// Jadi ThinkLevel hanya diterjemahkan ke think (1=plain, 2=high), bukan
	// ke bentuk header yang berbeda.
	// Perhatikan: yang menentukan extended thinking hanya tingkat High.
	// Perbandingan lama `thinkLevel >= thinkLevelHigh` salah karena angka level
	// BUKAN urutan naik: low=1, high=2, medium=4. Akibatnya medium (4) ikut
	// dianggap >= high (2), dikirim sebagai extended thinking, dan Google
	// menjawab 3.5 Flash-Lite -> deteksi downgrade -> 502. Jadi bandingkan
	// dengan nilai persisnya, bukan dengan relasi "lebih besar".
	if hexID == hexArgon4 && thinkLevel == thinkLevelHigh {
		think = thinkingExtended
	}
	if uuid == "" {
		return fmt.Sprintf(`[1,null,null,null,"%s"]`, hexID)
	}
	return fmt.Sprintf(
		`[1,null,null,null,"%s",null,null,0,[4,5,6,8],null,null,1,null,null,%d,%d,"%s"]`,
		hexID, mode, think, uuid)
}

// buildGeminiHeaders 准备 StreamGenerate 必需的应用层 header。
// hexID 决定服务端用哪个模型；留空则服务端一律回落到 3.5 Flash-Lite。
func buildGeminiHeaders(cookieStr, sapisid, hexID string) map[string]string {
	h := map[string]string{
		"Accept":          "*/*",
		"Accept-Language": "en-US,en;q=0.9",
		"Content-Type":    "application/x-www-form-urlencoded;charset=UTF-8",
		"Origin":          "https://gemini.google.com",
		"Referer":         "https://gemini.google.com/app",
		"X-Same-Domain":   "1",
		"X-Goog-AuthUser": "0",
		// 这两个浏览器每次都发，值是固定的。
		"x-goog-ext-73010989-jspb": "[0]",
		"x-goog-ext-73010990-jspb": "[0,0,0]",
	}
	if hexID != "" {
		h["x-goog-ext-525001261-jspb"] = buildModelHeader(hexID, 0, 0, 0, "")
	}
	if cookieStr != "" {
		h["Cookie"] = stripAuthUserMarker(cookieStr)
	}
	// Akun ke-2 dan seterusnya dalam satu profil dibedakan lewat header ini.
	if n := authUserForCookie(cookieStr); n > 0 {
		h["X-Goog-AuthUser"] = strconv.FormatInt(n, 10)
	}
	if sapisid != "" {
		h["Authorization"] = makeSAPISIDHash(sapisid)
	}
	return h
}

// doGeminiRequest 发一次请求到 endpoint。proxyURL 非空走 stdlib（支持 socks5/http），
// 空走 tls-client（chrome146 真指纹）。返回 (HTTP status, body bytes, err)。
// 返回值多了 setCookie：服务端几乎每个响应都在刷新 SIDCC / __Secure-1PSIDCC /
// __Secure-3PSIDCC，浏览器收下再带回去。一直发旧值的客户端会被判定为过期会话，
// 实测号活一两小时就失效 —— 所以这些必须收下来并写回账号。
func doGeminiRequest(endpoint, body string, headers map[string]string, proxyURL string,
	onLine func(string), tool int, ctxs ...context.Context) (int, []byte, int64, []string, error) {
	sendAt := time.Now()
	ctx := optCtx(ctxs)
	if proxyURL != "" {
		// 走 stdlib 的 http.ProxyURL，已知能过 socks5/socks5h。
		req, err := http.NewRequest("POST", endpoint, strings.NewReader(body))
		if err != nil {
			return 0, nil, 0, nil, err
		}
		if ctx != nil {
			req = req.WithContext(ctx)
		}
		applyChromeHeaders(req)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		client := getStdlibClient(proxyURL)
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, 0, nil, err
		}
		defer resp.Body.Close()
		raw, ttfb, err := readBody(resp.Body, onLine, sendAt, tool)
		if err != nil {
			return resp.StatusCode, nil, ttfb, resp.Header.Values("Set-Cookie"), err
		}
		return resp.StatusCode, raw, ttfb, resp.Header.Values("Set-Cookie"), nil
	}

	// 直连 → tls-client，保留 chrome146 TLS/HTTP2 真指纹
	req, err := fhttp.NewRequest("POST", endpoint, strings.NewReader(body))
	if err != nil {
		return 0, nil, 0, nil, err
	}
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := tlsClientFor(tlsProfileFrom(ctx))
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, 0, nil, err
	}
	defer resp.Body.Close()
	raw, ttfb, err := readBody(resp.Body, onLine, sendAt, tool)
	if err != nil {
		return resp.StatusCode, nil, ttfb, resp.Header.Values("Set-Cookie"), err
	}
	return resp.StatusCode, raw, ttfb, resp.Header.Values("Set-Cookie"), nil
}

// readBody 读完整个响应体并原样返回；onLine 非 nil 时每读到一行就回调一次，
// 让上层能在上游还没写完时就往客户端转发。
//
// 始终走逐行扫描（而不是 onLine==nil 时图省事用 io.ReadAll），因为要拿到
// **第一行到达的时刻**当 TTFB。用 ReadAll 的话读完才返回，测出来的"首字节
// 耗时"实际是完整耗时，跟总耗时永远一样。
//
// start 必须是**请求发出前**的时刻，由调用方传入。放在本函数里取 time.Now()
// 是不对的：那时 client.Do 已经返回、响应头甚至部分 body 都到了，测出来恒为 0。
// isStreamTerminator menjawab apakah satu baris adalah frame penutup respons
// batchexecute: [["e",<code>,null,null,<bytes>]].
//
// Kenapa penting: Google mengirim frame ini sebagai penanda "selesai", tapi
// koneksinya TIDAK ditutup sesudahnya. Tanpa deteksi ini pembaca menggantung
// sampai timeout total (180s+) padahal isi jawaban sudah lengkap sejak detik
// pertama — persis gejala "TTFB 1s tapi total 200s".
func isStreamTerminator(line string) bool {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, `[["e",`) {
		return false
	}
	// Pastikan bentuknya memang frame penutup, bukan teks biasa yang kebetulan
	// mirip: harus ada penanda akhir array.
	return strings.HasSuffix(s, "]]")
}

// isPreambleLine menjawab apakah baris ini bagian pembungkus protokol, bukan isi.
//
// batchexecute mengirim: `)]}'` (penanda anti-XSSI), lalu angka panjang tiap
// potongan (`177`, `36`, `5520`), baru frame JSON. Kalau TTFB dihitung dari
// baris pertama, angkanya selalu ~1-2 detik dan menyesatkan: yang terukur
// kedatangan pembungkus, bukan jawaban. Akibatnya request yang butuh 358 detik
// tampak "TTFB 1.8s" dan orang menyimpulkan stream-nya menggantung padahal
// Google memang belum mengirim isi.
func isPreambleLine(line string) bool {
	s := strings.TrimSpace(line)
	if s == "" || s == ")]}'" {
		return true
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// isHeartbeatLine menjawab apakah satu baris adalah frame kendali Google yang
// tidak membawa isi jawaban.
//
// Selama model berpikir, Google mengirim `[{"37":[n]}]` tiap ~60 detik. Baris ini
// dulu ikut me-reset penjaga stall, sehingga penjaga itu tak pernah jenuh — dan
// stream yang jawabannya sudah selesai tetap terbuka 200-621 detik sampai
// akhirnya ditutup Google. Ini satu-satunya sebab "TTFB 250ms tapi total 621s"
// untuk jawaban selebar 2 karakter.
func isHeartbeatLine(line string) bool {
	s := strings.TrimSpace(line)
	return strings.HasPrefix(s, `[{"37"`) || strings.HasPrefix(s, `[{"38"`)
}

// contentIdleAfter adalah berapa lama setelah baris berisi terakhir sebelum
// stream ditutup, kalau ternyata hanya heartbeat yang datang sesudahnya.
//
// Kuncinya: cutoff ini DIHITUNG dari isi terakhir, bukan dari heartbeat. Jadi
// model yang berpikir lama tetap aman (deltanya terus datang), tapi model yang
// sudah menjawab dan hanya mengirim heartbeat tidak akan menggantung.
func contentIdleAfter() time.Duration {
	sec := rtCfg().ContentIdleSec
	if sec <= 0 {
		sec = 25
	}
	return time.Duration(sec) * time.Second
}

// stallAfter() adalah berapa lama tanpa byte baru sebelum upstream dianggap macet.
//
// Harus di atas interval heartbeat Google (terukur 60 detik: frame
// [{"37":[n]}] tiap ~60s selama model berpikir). Kalau di bawah itu, stream
// yang sehat ikut diputus dan kita kehilangan jawaban yang sebenarnya jalan.
//
// Efeknya: request yang benar-benar macet berhenti di ~2 menit, bukan menunggu
// batas total 720 detik. Klien menerima error yang bisa ditindaklanjuti jauh
// lebih cepat, dan akun berikutnya bisa langsung dicoba.
// stallAfterFor() adalah berapa lama tanpa byte baru sebelum upstream dianggap macet,
// disesuaikan per jenis model.
func stallAfterFor(tool int) time.Duration {
	sec := rtCfg().RequestTimeout
	if sec < 60 {
		sec = 60
	}
	// Khusus video (Veo): proses generasi di server Google memakan waktu 3-8 menit
	// di mana Google menahan koneksi tanpa mengirim data byte baru terus-menerus.
	// Batas idle standar (180s) memutus koneksi sebelum render selesai ("idle 3m0s").
	if tool == toolVideo {
		if sec < 600 {
			sec = 600 // 10 menit khusus video
		}
	}
	return time.Duration(sec) * time.Second
}

func stallAfter() time.Duration {
	return stallAfterFor(0)
}

// errUpstreamStall menandai pembacaan yang dihentikan karena upstream berhenti
// mengirim data. Dibedakan dari timeout total supaya pemanggil tahu ini bukan
// soal waktu habis, melainkan upstream berhenti bekerja.
type errUpstreamStall struct{ Idle time.Duration }

func (e *errUpstreamStall) Error() string {
	return fmt.Sprintf("upstream stopped sending data (idle %s)", e.Idle.Round(time.Second))
}

func readBody(r io.ReadCloser, onLine func(string), start time.Time, tool int) ([]byte, int64, error) {
	var buf bytes.Buffer
	sc := bufio.NewScanner(io.TeeReader(r, &buf))
	// 单帧可能很大（实测见过 40 万字节的响应），默认 64KB 上限不够。
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())

	// Terakhir kali baris yang MEMBWA ISI tiba. Heartbeat `[{"37":[n]}]` dan
	// frame kendali lain TIDAK menghitung: kalau ikut dihitung, idle timer
	// tak pernah jenuh (heartbeat tiap ~60s < stallAfter), stream terus
	// terbuka berjam-jam setelah jawaban utuh, dan satu permintaan butuh
	// 209-621 detik hanya untuk membalas "OK" (2 karakter).
	var lastContent atomic.Int64
	lastContent.Store(time.Now().UnixNano())

	// Pengawas: kalau tidak ada baris baru selama stallAfter(), tutup body.
	// Menutup body adalah satu-satunya cara membangunkan sc.Scan() yang sedang
	// memblokir di read; errornya lalu terbaca sebagai stall.
	stopWatch := make(chan struct{})
	var stalled atomic.Bool
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stopWatch:
				return
			case <-t.C:
				idle := time.Duration(time.Now().UnixNano() - lastActivity.Load())
				if idle > stallAfterFor(tool) {
					stalled.Store(true)
					_ = r.Close()
					return
				}
			}
		}
	}()

	// Penjaga jawaban-selesai: kalau jawaban sudah mengalir dan SEJAK ITU hanya
	// heartbeat/kendali yang datang selama contentIdleAfter(), tutup stream.
	// 3.1 Pro dan 3.8 Flash memegang koneksi terbuka berjam-jam setelah jawaban
	// 2 karakter selesai, sambil Google mengirim heartbeat tiap ~60 detik.
	stopIdle := make(chan struct{})
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stopIdle:
				return
			case <-t.C:
				idle := time.Duration(time.Now().UnixNano() - lastContent.Load())
				if idle > contentIdleAfter() {
					stalled.Store(true)
					_ = r.Close()
					return
				}
			}
		}
	}()

	var ttfb int64 = -1
	for sc.Scan() {
		line := sc.Text()
		// Heartbeat dan frame kendali tidak dihitung aktivitas.
		if isHeartbeatLine(line) {
			continue
		}
		lastActivity.Store(time.Now().UnixNano())
		// TTFB = saat frame isi pertama tiba, bukan saat pembungkus tiba.
		if ttfb < 0 && !isPreambleLine(line) {
			ttfb = time.Since(start).Milliseconds()
		}
		if onLine != nil {
			onLine(line)
		}
		// Baris ini menghasilkan isi? kalau ya, itu aktivitas jawaban.
		if !isPreambleLine(line) && !isStreamTerminator(line) {
			lastContent.Store(time.Now().UnixNano())
		}
		// Frame penutup: jawaban sudah utuh, berhenti membaca. Koneksi tidak
		// akan ditutup upstream, jadi menunggu = menggantung sampai timeout.
		if isStreamTerminator(line) {
			break
		}
	}
	close(stopIdle)
	close(stopWatch)

	if ttfb < 0 {
		ttfb = time.Since(start).Milliseconds()
	}
	if err := sc.Err(); err != nil {
		if stalled.Load() {
			return buf.Bytes(), ttfb, &errUpstreamStall{Idle: stallAfter()}
		}
		return buf.Bytes(), ttfb, err
	}
	return buf.Bytes(), ttfb, nil
}

// reasoningInLine 从单个 wrb.fr 行里取出思考链。
//
// 位置是 inner[4][0][37][0][0]，比正文（inner[4][0][1][0]）深两层。
// 只有 3.1 Pro 会产出，3.6 Flash / Flash-Lite 恒为空。
//
// 时序（实测一次过河谜题）：思考链在头 5 帧里累积到 660 字符，此时正文还是空；
// 从第 6 帧起正文开始增长而思考链冻结不再变。两者都是**累积全文**不是增量，
// 所以流式侧可以直接复用 deltaTracker 的前缀 diff。
func reasoningInLine(line string) string {
	if !strings.Contains(line, `"wrb.fr"`) || len(line) < 200 {
		return ""
	}
	var arr []interface{}
	if err := json.Unmarshal([]byte(line), &arr); err != nil || len(arr) == 0 {
		return ""
	}
	first, ok := arr[0].([]interface{})
	if !ok || len(first) < 3 {
		return ""
	}
	innerStr, ok := first[2].(string)
	if !ok || len(innerStr) < 50 {
		return ""
	}
	var inner []interface{}
	if err := json.Unmarshal([]byte(innerStr), &inner); err != nil || len(inner) <= 4 {
		return ""
	}
	parts, ok := inner[4].([]interface{})
	if !ok || len(parts) == 0 {
		return ""
	}
	p0, ok := parts[0].([]interface{})
	if !ok || len(p0) <= 37 {
		return ""
	}
	lvl1, ok := p0[37].([]interface{})
	if !ok || len(lvl1) == 0 {
		return ""
	}
	lvl2, ok := lvl1[0].([]interface{})
	if !ok || len(lvl2) == 0 {
		return ""
	}
	s, _ := lvl2[0].(string)
	return s
}

// extractReasoning 取整个响应里最长的那段思考链。
// 取最长而不是最后一个：思考链冻结后，后续帧该位置可能缺失或被截短。
//
// 必须跟 extractResponseText 一样过 cleanGeminiText：流式侧 deltaTracker 推的是
// 清洗过的文本，这里不清洗的话两者前缀对不上，收尾补发时会把整段思考链重发一遍
// （实测块顺序变成 R→C→R）。
func extractReasoning(raw string) string {
	best := ""
	for _, line := range strings.Split(raw, "\n") {
		if r := reasoningInLine(line); len(r) > len(best) {
			best = r
		}
	}
	return cleanGeminiText(best)
}

// textsInLine 从单个 wrb.fr 行里取出候选回复文本。
// 上游每帧带的是**累积全文**而不是增量，所以流式转发时要自己做前缀 diff。
func textsInLine(line string) []string {
	if !strings.Contains(line, `"wrb.fr"`) || len(line) < 200 {
		return nil
	}
	var arr []interface{}
	if err := json.Unmarshal([]byte(line), &arr); err != nil || len(arr) == 0 {
		return nil
	}
	first, ok := arr[0].([]interface{})
	if !ok || len(first) < 3 {
		return nil
	}
	innerStr, ok := first[2].(string)
	if !ok || len(innerStr) < 50 {
		return nil
	}
	var inner []interface{}
	if err := json.Unmarshal([]byte(innerStr), &inner); err != nil || len(inner) <= 4 {
		return nil
	}
	parts, ok := inner[4].([]interface{})
	if !ok {
		return nil
	}
	var texts []string
	for _, p := range parts {
		pl, ok := p.([]interface{})
		if !ok || len(pl) < 2 {
			continue
		}
		tl, ok := pl[1].([]interface{})
		if !ok {
			continue
		}
		for _, t := range tl {
			if s, ok := t.(string); ok && s != "" {
				texts = append(texts, s)
			}
		}
	}
	return texts
}

// hasContentFrame 判断响应里到底有没有内容帧。
//
// 故意不复用 extractResponseText：那个会先 cleanGeminiText 掉代码产物，一个纯代码
// 产物的回复在它眼里是空的，但那明明是上游正常出了内容。这里只问"有没有帧"，
// 判断的是链路成没成功，不是内容合不合用。
func hasContentFrame(raw string) bool {
	for _, line := range strings.Split(raw, "\n") {
		if len(textsInLine(line)) > 0 || reasoningInLine(line) != "" {
			return true
		}
	}
	return false
}

// extractResponseText parses StreamGenerate's wrb.fr stream and returns the
// last non-empty text chunk (matches Python extract_response_text behavior).
func extractResponseText(raw string) string {
	var texts []string
	for _, line := range strings.Split(raw, "\n") {
		texts = append(texts, textsInLine(line)...)
	}
	for i := len(texts) - 1; i >= 0; i-- {
		if strings.TrimSpace(texts[i]) != "" {
			cleaned := cleanGeminiText(texts[i])
			if cleaned == "" {
				// 整条回复都是代码执行产物（问数学/算式时模型直接跑代码），清洗后被剥空。
				// 别返回空——那样 callGemini 会当"无内容帧"报 502，用户看到的是调用失败。
				// 回退：只把 ?code_reference/stdout 标记去掉、保留代码和结果，当普通代码块给。
				cleaned = strings.TrimSpace(codeMarkerRe.ReplaceAllString(texts[i], "```$1"))
			}
			return cleaned
		}
	}
	return ""
}

var codeArtifactRe = regexp.MustCompile("(?s)```(?:python|javascript|text)\\?code_(?:reference|stdout)&code_event_index=\\d+\\n.*?```\\n?")

// codeMarkerRe 只匹配代码产物的**开围栏标记**（```python?code_reference&code_event_index=N），
// 用于清洗后为空时的回退：把标记降级成普通 ```python，保留代码/结果不整条清空。
var codeMarkerRe = regexp.MustCompile("```(python|javascript|text)\\?code_(?:reference|stdout)&code_event_index=\\d+")

func cleanGeminiText(text string) string {
	return strings.TrimSpace(codeArtifactRe.ReplaceAllString(text, ""))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ProbeResult 是 admin 测试接口返回的连通性诊断结果。
type ProbeResult struct {
	OK           bool   `json:"ok"`
	Status       string `json:"status"` // "success" / "blocked_sorry" / "rate_limited" / "upstream_error" / "network_error"
	HTTPCode     int    `json:"http_code"`
	TotalMs      int64  `json:"total_ms"`
	ProxyID      int64  `json:"proxy_id"`
	ProxyName    string `json:"proxy_name"`
	UseDirect    bool   `json:"use_direct"`
	ResponseText string `json:"response_text"` // 截断到 200 字符
	UpstreamSnip string `json:"upstream_snip"` // 上游原始响应前 300 字符
	Diagnostic   string `json:"diagnostic"`    // 中文诊断说明
	Impersonate  string `json:"impersonate"`
}

// probeGemini 直接调 Gemini StreamGenerate（绕过限流），返回详细诊断。
// 不写 db、不消耗限流 slot。
func probeGemini(prompt, proxyURL string) ProbeResult {
	res := ProbeResult{Impersonate: rtCfg().Impersonate}

	inner := make([]interface{}, innerSlots)
	inner[0] = []interface{}{prompt, 0, nil, nil, nil, nil, 0}
	inner[1] = []interface{}{"en"}
	inner[2] = []interface{}{"", "", "", nil, nil, nil, nil, nil, nil, ""}
	inner[6] = []interface{}{0}
	inner[7] = 1
	inner[10] = 1
	inner[11] = 0
	inner[17] = []interface{}{[]interface{}{0}}
	inner[18] = 0
	inner[27] = 1
	inner[30] = []interface{}{4}
	// 抓包里浏览器三种场景（有 cookie / 无 cookie / 扩展思考）全是 [1]。
	// 我们原来写 [2]，是早期抄来的值、协议层已被证伪。含义仍未知，
	// 匿名两个值都能通，但没有理由继续偏离浏览器。
	inner[41] = []interface{}{1}
	inner[53] = 0
	inner[59] = uuid.NewString()
	inner[61] = []interface{}{}
	inner[68] = 1
	probeModel := Models["gemini-3.6-flash"]
	inner[79] = probeModel.Mode

	innerJSON, _ := json.Marshal(inner)
	outer := []interface{}{nil, string(innerJSON)}
	outerJSON, _ := json.Marshal(outer)
	form := url.Values{}
	form.Set("f.req", string(outerJSON))
	body := form.Encode()

	reqid := time.Now().Unix() % 1000000
	endpoint := fmt.Sprintf(
		"https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate?bl=%s&hl=en&_reqid=%d&rt=c",
		currentBL(proxyURL), reqid,
	)

	// probe 是旁路探测，不回写 cookie 健康度：它的失败原因跟 cookie 无关。
	// 但 at 必须带——否则挂了 cookie 之后连通性探测会一直报 400，假报故障。
	cookieStr, sapisid := loadCookie()
	endpoint = geminiURLWithAuthUser(endpoint, cookieStr)
	if tok, e := getXSRF(cookieStr, proxyURL); e == nil && tok != "" {
		form.Set("at", tok)
		body = form.Encode()
	}
	headers := buildGeminiHeaders(cookieStr, sapisid, probeModel.HexID)

	// 复用主流程同款 client 选择规则:有代理走 stdlib，没代理走 tls-client。
	// 但 probe 需要看 302 的 Location header,所以这里直接发不用 doGeminiRequest。
	var statusCode int
	var raw []byte
	var locHeader string
	var err error

	if proxyURL != "" {
		req, e := http.NewRequest("POST", endpoint, strings.NewReader(body))
		if e != nil {
			res.Status = "network_error"
			res.Diagnostic = "构建请求失败: " + e.Error()
			return res
		}
		applyChromeHeaders(req)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		client := getStdlibClient(proxyURL)
		resp, e := client.Do(req)
		if e != nil {
			res.Status = "network_error"
			res.Diagnostic = "网络层错误（DNS/TCP/TLS 失败）: " + e.Error()
			return res
		}
		defer resp.Body.Close()
		statusCode = resp.StatusCode
		locHeader = resp.Header.Get("Location")
		raw, err = io.ReadAll(resp.Body)
	} else {
		req, e := fhttp.NewRequest("POST", endpoint, strings.NewReader(body))
		if e != nil {
			res.Status = "network_error"
			res.Diagnostic = "构建请求失败: " + e.Error()
			return res
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		client := getTLSClient()
		resp, e := client.Do(req)
		if e != nil {
			res.Status = "network_error"
			res.Diagnostic = "网络层错误（DNS/TCP/TLS 失败）: " + e.Error()
			return res
		}
		defer resp.Body.Close()
		statusCode = resp.StatusCode
		locHeader = resp.Header.Get("Location")
		raw, err = io.ReadAll(resp.Body)
	}
	_ = err
	res.HTTPCode = statusCode
	res.UpstreamSnip = truncate(string(raw), 300)

	switch {
	case statusCode == 302:
		res.Status = "blocked_sorry"
		res.Diagnostic = "IP 被 Google 风控（重定向到 sorry/index）。" +
			"通常 6-24 小时解除，或换 VPN/代理 IP 立即恢复。Location: " + truncate(locHeader, 200)
		return res
	case statusCode == 429:
		res.Status = "rate_limited"
		res.Diagnostic = "Google 直接返回 429 限流。同样是 IP 嫌疑，但风控分支不同（朴素 SDK 路径）。"
		return res
	case statusCode != 200:
		res.Status = "upstream_error"
		res.Diagnostic = fmt.Sprintf("上游返回非 200 (HTTP %d)，可能是协议变更或临时故障。", statusCode)
		return res
	}

	text := extractResponseText(string(raw))
	if text == "" {
		res.Status = "upstream_error"
		res.Diagnostic = "上游 200 但只回了结束帧、没有内容帧，通常是请求被服务端拒绝" +
			"（例如带了不被接受的会话 id 或工具开关），不是帧格式变更。"
		return res
	}

	res.OK = true
	res.Status = "success"
	res.ResponseText = truncate(text, 200)
	res.Diagnostic = "调用成功。延迟 / 内容见上面字段。"
	return res
}
