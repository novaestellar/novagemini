package app

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client keys: kunci terpisah untuk tiap klien, dengan kuota RPM/RPH sendiri.
//
// Kenapa: sebelumnya hanya ada SATU kunci global (api_key). Berbagi kunci itu
// ke beberapa klien berarti tidak bisa mencabut satu klien tanpa memutus semua,
// dan tidak ada batas per klien. Tabel ini memberi satu baris per klien:
// nama, kunci, kuota, status, dan kapan terakhir dipakai.

const clientKeyPrefix = "sk-gm-"

type ClientKey struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Key       string `json:"key,omitempty"` // hanya dikembalikan sekali saat dibuat
	Masked    string `json:"masked"`
	RPM       int    `json:"rpm"`
	RPH       int    `json:"rph"`
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"created_at"`
	LastUsed  int64  `json:"last_used_at"`
}

func newClientKey() string {
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	return clientKeyPrefix + hex.EncodeToString(b)
}

func maskClientKey(k string) string {
	if len(k) <= 16 {
		return k
	}
	return k[:10] + "…" + k[len(k)-4:]
}

func clientKeyList() []ClientKey {
	rows, err := getDB().Query(`SELECT id, name, key, rpm, rph, enabled, created_at, last_used_at
		FROM client_keys ORDER BY id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]ClientKey, 0, 8)
	for rows.Next() {
		var k ClientKey
		var enabled int
		if err := rows.Scan(&k.ID, &k.Name, &k.Key, &k.RPM, &k.RPH, &enabled, &k.CreatedAt, &k.LastUsed); err != nil {
			continue
		}
		k.Enabled = enabled != 0
		k.Masked = maskClientKey(k.Key)
		k.Key = "" // jangan pernah kirim kunci penuh lewat daftar
		out = append(out, k)
	}
	return out
}

func clientKeyCreate(name string, rpm, rph int) (ClientKey, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "client"
	}
	key := newClientKey()
	now := time.Now().Unix()
	res, err := getDB().Exec(`INSERT INTO client_keys (name, key, rpm, rph, enabled, created_at, last_used_at)
		VALUES (?, ?, ?, ?, 1, ?, 0)`, name, key, rpm, rph, now)
	if err != nil {
		return ClientKey{}, err
	}
	id, _ := res.LastInsertId()
	return ClientKey{
		ID: id, Name: name, Key: key, Masked: maskClientKey(key),
		RPM: rpm, RPH: rph, Enabled: true, CreatedAt: now,
	}, nil
}

func clientKeySet(id int64, name *string, rpm, rph *int, enabled *bool) error {
	sets := []string{}
	args := []interface{}{}
	if name != nil && strings.TrimSpace(*name) != "" {
		sets = append(sets, "name = ?")
		args = append(args, strings.TrimSpace(*name))
	}
	if rpm != nil {
		sets = append(sets, "rpm = ?")
		args = append(args, *rpm)
	}
	if rph != nil {
		sets = append(sets, "rph = ?")
		args = append(args, *rph)
	}
	if enabled != nil {
		v := 0
		if *enabled {
			v = 1
		}
		sets = append(sets, "enabled = ?")
		args = append(args, v)
	}
	if len(sets) == 0 {
		return nil
	}
	args = append(args, id)
	_, err := getDB().Exec("UPDATE client_keys SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	return err
}

func clientKeyRotate(id int64) (string, error) {
	key := newClientKey()
	_, err := getDB().Exec("UPDATE client_keys SET key = ? WHERE id = ?", key, id)
	return key, err
}

func clientKeyDelete(id int64) error {
	_, err := getDB().Exec("DELETE FROM client_keys WHERE id = ?", id)
	return err
}

// clientKeyLookup mencari kunci klien dengan perbandingan waktu-konstan.
func clientKeyLookup(raw string) (*ClientKey, bool) {
	if raw == "" {
		return nil, false
	}
	rows, err := getDB().Query(`SELECT id, name, key, rpm, rph, enabled, created_at, last_used_at FROM client_keys`)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	var found *ClientKey
	for rows.Next() {
		var k ClientKey
		var enabled int
		if err := rows.Scan(&k.ID, &k.Name, &k.Key, &k.RPM, &k.RPH, &enabled, &k.CreatedAt, &k.LastUsed); err != nil {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(k.Key), []byte(raw)) == 1 {
			k.Enabled = enabled != 0
			cp := k
			found = &cp
		}
	}
	if found == nil {
		return nil, false
	}
	return found, true
}

func clientKeyTouch(id int64) {
	_, _ = getDB().Exec("UPDATE client_keys SET last_used_at = ? WHERE id = ?", time.Now().Unix(), id)
}

// Kuota per kunci: jendela menit dan jam, disimpan di memori.
type keyWindow struct {
	minute int64
	mc     int
	hour   int64
	hc     int
}

var (
	ckMu       sync.Mutex
	ckCounters = map[int64]*keyWindow{}
)

func clientKeyAllow(k *ClientKey) bool {
	if k.RPM <= 0 && k.RPH <= 0 {
		return true
	}
	now := time.Now()
	curMin := now.Unix() / 60
	curHour := now.Unix() / 3600
	ckMu.Lock()
	defer ckMu.Unlock()
	w := ckCounters[k.ID]
	if w == nil {
		w = &keyWindow{minute: curMin, hour: curHour}
		ckCounters[k.ID] = w
	}
	if w.minute != curMin {
		w.minute, w.mc = curMin, 0
	}
	if w.hour != curHour {
		w.hour, w.hc = curHour, 0
	}
	if k.RPM > 0 && w.mc >= k.RPM {
		return false
	}
	if k.RPH > 0 && w.hc >= k.RPH {
		return false
	}
	w.mc++
	w.hc++
	return true
}

// ── handler admin ─────────────────────────────────────────────────────────

func handleAdminClientKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		writeJSON(w, 200, map[string]interface{}{"items": clientKeyList()})
	case "POST":
		var p struct {
			Name string `json:"name"`
			RPM  int    `json:"rpm"`
			RPH  int    `json:"rph"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &p); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json"})
			return
		}
		if p.RPM < 0 || p.RPH < 0 {
			writeJSON(w, 400, map[string]string{"error": "quota must be >= 0"})
			return
		}
		k, err := clientKeyCreate(p.Name, p.RPM, p.RPH)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, k)
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

func handleAdminClientKeyItem(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimPrefix(r.URL.Path, "/admin/api/client-keys/")
	if strings.HasSuffix(raw, "/rotate") {
		id, _ := strconv.ParseInt(strings.TrimSuffix(raw, "/rotate"), 10, 64)
		if id <= 0 {
			writeJSON(w, 400, map[string]string{"error": "bad id"})
			return
		}
		key, err := clientKeyRotate(id)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]string{"key": key})
		return
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, 400, map[string]string{"error": "bad id"})
		return
	}
	switch r.Method {
	case "PATCH":
		var p struct {
			Name    *string `json:"name"`
			RPM     *int    `json:"rpm"`
			RPH     *int    `json:"rph"`
			Enabled *bool   `json:"enabled"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &p); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json"})
			return
		}
		if err := clientKeySet(id, p.Name, p.RPM, p.RPH, p.Enabled); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	case "DELETE":
		if err := clientKeyDelete(id); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}
