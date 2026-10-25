package app

import (
	"net/http"
)

// handleAdminProbe — GET  : kembalikan hasil probe terakhir (tanpa memanggil Google)
//                   POST : jalankan probe baru untuk semua akun, lalu kembalikan
//
// Probe memakai RPC ESY5D yang hanya membaca status akun; tidak mengirim job
// generate apa pun sehingga tidak memakai kuota media.
func handleAdminProbe(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		snap := probeSnapshot()
		items := make([]map[string]interface{}, 0, len(snap))
		for _, c := range snap {
			items = append(items, capabilityJSON(c))
		}
		writeJSON(w, 200, map[string]interface{}{
			"items": items,
			"count": len(items),
		})

	case http.MethodPost:
		// Probe bisa lambat (satu request ESY5D per akun), jadi kirim hasil
		// per akun begitu selesai. Handler tetap sinkron supaya tombol di
		// dashboard bisa menunggu satu respons penuh.
		snap := probeAllAccounts()
		items := make([]map[string]interface{}, 0, len(snap))
		for _, c := range snap {
			items = append(items, capabilityJSON(c))
		}
		writeJSON(w, 200, map[string]interface{}{
			"items": items,
			"count": len(items),
		})

	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}
