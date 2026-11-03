package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// sseWriter 把 chat.completion.chunk 按 OpenAI 规范的顺序写出去：
// 先 delta{role} → 若干 delta{content} → delta{} + finish_reason → 可选 usage → [DONE]。
//
// header 是懒发的：第一次真要写 chunk 时才 WriteHeader(200)。这样上游在出第一个
// 字之前就失败的话，还能退回正常的 502 JSON，而不是留给客户端半个流。
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	id      string
	created int64
	model   string
	started bool

	// mu melindungi penulisan ke ResponseWriter: heartbeat dikirim dari
	// goroutine terpisah, jadi dua penulis tidak boleh bertabrakan.
	mu     sync.Mutex
	hbStop chan struct{}
	hbOnce sync.Once

	// held menahan potongan awal stream. Penolakan generik Google ("I seem to
	// be encountering an error...") hanya puluhan karakter dan datang sebagai
	// jawaban biasa di jalur stream; tanpa ditahan, teks itu terkirim sebagai
	// konten dan klien mengira model menjawab begitu.
	held strings.Builder
}

// heartbeatInterval: jeda antar komentar keepalive.
//
// Kenapa ada: antara pembukaan stream dan potongan isi pertama bisa lewat
// >100 detik (model lambat / thinking panjang). Proxy di tengah (9router,
// cloudflared, nginx) memutus koneksi yang diam; hasilnya klien melihat 503
// padahal upstream masih bekerja. Komentar SSE (`: keepalive`) diabaikan
// klien OpenAI - tidak muncul sebagai konten, hanya menjaga koneksi hidup.
const heartbeatInterval = 15 * time.Second

// writeRaw menulis langsung ke klien dengan kunci, lalu flush.
func (s *sseWriter) writeRaw(format string, args ...interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.w, format, args...)
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

func (s *sseWriter) startHeartbeat() {
	s.hbOnce.Do(func() {
		s.hbStop = make(chan struct{})
		go func() {
			t := time.NewTicker(heartbeatInterval)
			defer t.Stop()
			for {
				select {
				case <-s.hbStop:
					return
				case <-t.C:
					s.writeRaw(": keepalive\n\n")
				}
			}
		}()
	})
}

func (s *sseWriter) stopHeartbeat() {
	if s.hbStop != nil {
		close(s.hbStop)
		s.hbStop = nil
	}
}

func newSSEWriter(w http.ResponseWriter, id string, created int64, model string) *sseWriter {
	f, _ := w.(http.Flusher)
	return &sseWriter{w: w, flusher: f, id: id, created: created, model: model}
}

func (s *sseWriter) Started() bool { return s.started }

// start 发送 SSE 响应头和首个 delta{"role":"assistant"} chunk。幂等。
func (s *sseWriter) start() {
	if s.started {
		return
	}
	s.started = true
	s.w.Header().Set("Content-Type", "text/event-stream")
	s.w.Header().Set("Cache-Control", "no-cache")
	s.w.Header().Set("Connection", "keep-alive")
	// 关掉反代（nginx 等）对 SSE 的整体缓冲：不发这个头，客户端直连能看到流式，
	// 但一旦经过反代就会被攒成一坨一次性下发（Dify 那种接法就是这么变非流式的）。
	s.w.Header().Set("X-Accel-Buffering", "no")
	s.w.Header().Set("Access-Control-Allow-Origin", "*")
	s.w.WriteHeader(200)
	s.chunk(map[string]interface{}{"role": "assistant"}, nil, nil)
	s.startHeartbeat()
}

func (s *sseWriter) chunk(delta map[string]interface{}, finish interface{}, usage map[string]int) {
	c := map[string]interface{}{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []map[string]interface{}{{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
	if usage != nil {
		c["usage"] = usage
	}
	b, _ := json.Marshal(c)
	s.writeRaw("data: %s\n\n", b)
}

// SendContent 发一段正文增量，供 streamGenerate 的 onDelta 回调直接使用。
// SendReasoning 推思考链增量。上游先把思考链推完才开始推正文，所以客户端会
// 先看到「思考过程」再看到答案，跟网页端的观感一致。
func (s *sseWriter) SendReasoning(delta string) {
	if delta == "" {
		return
	}
	s.start()
	s.chunk(map[string]interface{}{"reasoning_content": delta}, nil, nil)
}

// heldContentLimit: berapa karakter awal yang ditahan sebelum dikirim.
//
// Cukup untuk membedakan penolakan generik (puluhan karakter) dari jawaban
// sungguhan, tanpa menunda jawaban panjang secara berarti: begitu 160 karakter
// terkumpul atau ada baris baru, buffer langsung dikirim.
const heldContentLimit = 160

func (s *sseWriter) SendContent(delta string) {
	if delta == "" {
		return
	}
	s.start()
	s.held.WriteString(delta)
	if s.held.Len() >= heldContentLimit || strings.Contains(s.held.String(), "\n") {
		s.flushHeld()
	}
}

// flushHeld mengirim isi buffer tahanan sebagai satu chunk.
func (s *sseWriter) flushHeld() {
	if s.held.Len() == 0 {
		return
	}
	txt := s.held.String()
	s.held.Reset()
	s.chunk(map[string]interface{}{"content": txt}, nil, nil)
}

// Settle dipanggil tepat sebelum stream ditutup. Kalau seluruh teks ternyata
// penolakan generik, kembalikan error supaya pemanggil mengirim chunk error
// alih-alih menyajikan penolakan Google sebagai jawaban model.
func (s *sseWriter) Settle(fullText string) error {
	if cannedRefusal(fullText) {
		return fmt.Errorf("upstream returned a canned refusal instead of an answer; " +
			"this is transient on Google's side, not a quota or access problem")
	}
	s.flushHeld()
	return nil
}

func (s *sseWriter) SendToolCalls(tcs []ToolCall) {
	s.start()
	// 流式 delta 里每个 tool_call 必须带 index，客户端靠它把分片的 tool_call 拼起来
	// （OpenAI 流式规范要求）。ToolCall 结构本身没有 index，这里按顺序补上。
	out := make([]map[string]interface{}, len(tcs))
	for i, tc := range tcs {
		out[i] = map[string]interface{}{
			"index": i,
			"id":    tc.ID,
			"type":  tc.Type,
			"function": map[string]interface{}{
				"name":      tc.Function.Name,
				"arguments": tc.Function.Arguments,
			},
		}
	}
	s.chunk(map[string]interface{}{"tool_calls": out}, nil, nil)
}

// Finish 收尾：空 delta + finish_reason，可选 usage chunk，最后 [DONE]。
func (s *sseWriter) Finish(reason string, usage map[string]int) {
	s.start()
	s.stopHeartbeat()
	s.chunk(map[string]interface{}{}, reason, nil)
	if usage != nil {
		// OpenAI 的 usage chunk：choices 为空数组
		c := map[string]interface{}{
			"id": s.id, "object": "chat.completion.chunk",
			"created": s.created, "model": s.model,
			"choices": []map[string]interface{}{},
			"usage":   usage,
		}
		b, _ := json.Marshal(c)
		s.writeRaw("data: %s\n\n", b)
	}
	s.writeRaw("data: [DONE]\n\n")
}

// Fail 在已经开流之后出错时用：发一个带 error 的 chunk 再收尾。
// 此时 HTTP 状态码已经是 200，改不了了。
func (s *sseWriter) Fail(err error) {
	s.start()
	s.stopHeartbeat()
	c := map[string]interface{}{
		"id": s.id, "object": "chat.completion.chunk",
		"created": s.created, "model": s.model,
		"choices": []map[string]interface{}{},
		"error":   map[string]string{"message": err.Error(), "type": "upstream_error"},
	}
	b, _ := json.Marshal(c)
	s.writeRaw("data: %s\n\n", b)
	s.writeRaw("data: [DONE]\n\n")
}
