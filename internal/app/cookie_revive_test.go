package app

import (
	"testing"
	"time"
)

// Regresi untuk bug "akun sehat tampil cookie expired selamanya".
//
// Yang diuji adalah INVARIAN, bukan implementasi: satu kegagalan tidak boleh
// mematikan akun, dan bukti hidup dari jalur mana pun harus mencabut penanda
// mati. Ketiganya dulu rusak bersama-sama sehingga pool habis pelan-pelan.
func TestRotateFailureDoesNotDisableHealthyAccount(t *testing.T) {
	withTestDB(t)

	id := insertTestAccount(t, "rotate-healthy")

	// Akun baru saja sukses: masih di dalam recentOKWindow.
	_, err := getDB().Exec(`UPDATE accounts SET last_ok_at=?, fail_count=0 WHERE id=?`,
		time.Now().Unix(), id)
	if err != nil {
		t.Fatalf("seed last_ok_at: %v", err)
	}

	// Simulasikan jalur "rotate gagal + /app tanpa SNlM0e": naikkan fail_count
	// sekali, jangan sentuh status.
	if _, err := getDB().Exec(
		`UPDATE accounts SET fail_count=fail_count+1,
		 last_error='no SNlM0e in page (cookie incomplete or rotate in progress)'
		 WHERE id=?`, id); err != nil {
		t.Fatalf("bump fail_count: %v", err)
	}

	status, fails := accountStatusAndFails(t, id)
	if status != "enabled" {
		t.Fatalf("satu kegagalan rotate mematikan akun sehat: status=%q (want enabled)", status)
	}
	if fails != 1 {
		t.Errorf("fail_count=%d want 1", fails)
	}
}

// Akun yang belum pernah sukses tetap boleh mati — tapi hanya setelah ambang
// beruntun, bukan pada kegagalan pertama.
func TestAutoDisableOnlyAfterThreshold(t *testing.T) {
	withTestDB(t)

	id := insertTestAccount(t, "never-ok")
	// Tidak ada last_ok_at -> akun dianggap belum pernah sukses.
	for i := 1; i <= maxCookieAuthFailures; i++ {
		if _, err := getDB().Exec(
			`UPDATE accounts SET fail_count=fail_count+1 WHERE id=?`, id); err != nil {
			t.Fatalf("bump: %v", err)
		}
		autoDisableIfDead(id)
		status, _ := accountStatusAndFails(t, id)
		want := "enabled"
		if i >= maxCookieAuthFailures {
			want = "disabled"
		}
		if status != want {
			t.Fatalf("percobaan ke-%d: status=%q want %q", i, status, want)
		}
	}
}

// reviveAccountAfterSuccess harus mencabut status disabled DAN mereset
// fail_count, karena keduanya dipakai sebagai filter pemilihan akun.
func TestReviveAccountAfterSuccess(t *testing.T) {
	withTestDB(t)

	id := insertTestAccount(t, "revive-me")
	if _, err := getDB().Exec(
		`UPDATE accounts SET status='disabled', fail_count=24,
		 last_error='no SNlM0e in page (cookie expired or not signed in)' WHERE id=?`,
		id); err != nil {
		t.Fatalf("seed: %v", err)
	}

	reviveAccountAfterSuccess(id)

	status, fails := accountStatusAndFails(t, id)
	if status != "enabled" {
		t.Errorf("status=%q want enabled", status)
	}
	if fails != 0 {
		t.Errorf("fail_count=%d want 0", fails)
	}

	// Idempoten: memanggil lagi tidak boleh mengubah apa pun yang berarti.
	reviveAccountAfterSuccess(id)
	if status2, fails2 := accountStatusAndFails(t, id); status2 != "enabled" || fails2 != 0 {
		t.Errorf("panggilan kedua mengubah state: status=%q fails=%d", status2, fails2)
	}

	// id<=0 (anonim) harus no-op, bukan query dengan id kosong.
	reviveAccountAfterSuccess(0)
}

// repairAccountStatuses hanya menyentuh akun yang ditandai mati oleh jalur
// otomatis ("no SNlM0e"), bukan yang di-disable manual oleh operator.
func TestRepairAccountStatuses(t *testing.T) {
	withTestDB(t)

	now := time.Now().Unix()
	autoID := insertTestAccount(t, "auto-disabled")
	staleID := insertTestAccount(t, "dead-three-days")
	manualID := insertTestAccount(t, "manual-off")
	healthyID := insertTestAccount(t, "healthy")

	// Disabled oleh bug rotate, TAPI baru saja sukses -> harus pulih.
	if _, err := getDB().Exec(
		`UPDATE accounts SET status='disabled', fail_count=24, last_ok_at=?,
		 last_error='no SNlM0e in page (cookie expired or not signed in)' WHERE id=?`,
		now-600, autoID); err != nil {
		t.Fatalf("seed auto: %v", err)
	}
	// Penanda yang sama tapi sukses terakhir 3 hari lalu -> cookie memang
	// hangus, jangan diaktifkan ulang (kalau tidak, tiap boot membayar XSRF
	// round-trip untuk akun mati).
	if _, err := getDB().Exec(
		`UPDATE accounts SET status='disabled', fail_count=425, last_ok_at=?,
		 last_error='no SNlM0e in page (cookie expired or not signed in)' WHERE id=?`,
		now-3*24*3600, staleID); err != nil {
		t.Fatalf("seed stale: %v", err)
	}
	// Disabled manual: operator mematikannya, last_error bisa apa saja.
	if _, err := getDB().Exec(
		`UPDATE accounts SET status='disabled', note='dimatikan manual',
		 last_error='dihentikan operator' WHERE id=?`, manualID); err != nil {
		t.Fatalf("seed manual: %v", err)
	}

	repairAccountStatuses()

	if st, fails := accountStatusAndFails(t, autoID); st != "enabled" || fails != 0 {
		t.Errorf("akun yang baru sukses tidak dipulihkan: status=%q fails=%d", st, fails)
	}
	if st, _ := accountStatusAndFails(t, staleID); st != "disabled" {
		t.Errorf("akun mati 3 hari ikut diaktifkan: status=%q", st)
	}
	if st, _ := accountStatusAndFails(t, manualID); st != "disabled" {
		t.Errorf("akun yang di-disable manual ikut diaktifkan: status=%q", st)
	}
	if st, _ := accountStatusAndFails(t, healthyID); st != "enabled" {
		t.Errorf("akun sehat berubah status: %q", st)
	}
}

// Revive harus memperbarui last_ok_at: nilai itu dipakai sebagai patokan
// "baru sukses" oleh rotate.go, jadi kalau dibiarkan basi akun sehat bisa
// langsung dianggap mati lagi pada kegagalan berikutnya.
func TestReviveAccountUpdatesLastOK(t *testing.T) {
	withTestDB(t)

	id := insertTestAccount(t, "stale-lastok")
	old := time.Now().Add(-72 * time.Hour).Unix()
	if _, err := getDB().Exec(
		`UPDATE accounts SET status='disabled', fail_count=25, last_ok_at=?, last_error=? WHERE id=?`,
		old, "no SNlM0e in page (cookie expired or not signed in)", id); err != nil {
		t.Fatalf("seed: %v", err)
	}

	reviveAccountAfterSuccess(id)

	var lastOK int64
	if err := getDB().QueryRow(`SELECT last_ok_at FROM accounts WHERE id=?`, id).Scan(&lastOK); err != nil {
		t.Fatalf("baca last_ok_at: %v", err)
	}
	if lastOK <= old {
		t.Errorf("last_ok_at tidak diperbarui: %d (want > %d)", lastOK, old)
	}
	if time.Since(time.Unix(lastOK, 0)) > time.Minute {
		t.Errorf("last_ok_at terlalu lama: %v", time.Unix(lastOK, 0))
	}
}

// sync menimpa cookie akun yang sudah ada: status pulih, fail_count nol,
// id/label tetap. Ini yang membuat pola "browser pemilik sesi" bisa jalan
// tanpa membuat akun baru setiap kali.
func TestAccountSetCookieRevivesInPlace(t *testing.T) {
	withTestDB(t)

	id := insertTestAccount(t, "sync-target")
	if _, err := getDB().Exec(
		`UPDATE accounts SET status='disabled', fail_count=37,
		 last_error='no SNlM0e in page (cookie incomplete or rotate in progress)' WHERE id=?`,
		id); err != nil {
		t.Fatalf("seed: %v", err)
	}

	fresh := "SID=new; HSID=new; SSID=new; APISID=new; SAPISID=freshSAPISID0123; __Secure-1PSID=newpsid"
	if err := accountSetCookie(id, fresh); err != nil {
		t.Fatalf("accountSetCookie: %v", err)
	}

	var status, lastErr, cookie string
	var fails int64
	if err := getDB().QueryRow(
		`SELECT status, fail_count, last_error, cookie FROM accounts WHERE id=?`, id).
		Scan(&status, &fails, &lastErr, &cookie); err != nil {
		t.Fatalf("baca: %v", err)
	}
	if status != "enabled" || fails != 0 || lastErr != "" {
		t.Errorf("status=%q fails=%d err=%q, want enabled/0/kosong", status, fails, lastErr)
	}
	if extractSAPISID(cookie) != "freshSAPISID0123" {
		t.Errorf("cookie tidak terganti: %q", cookie)
	}

	// Cookie tanpa SAPISID ditolak, dan penolakan tidak boleh mengubah apa pun.
	before := cookie
	if err := accountSetCookie(id, "SID=only; NID=x"); err == nil {
		t.Error("cookie tanpa SAPISID seharusnya ditolak")
	}
	if err := getDB().QueryRow(`SELECT cookie FROM accounts WHERE id=?`, id).Scan(&cookie); err != nil {
		t.Fatalf("baca ulang: %v", err)
	}
	if cookie != before {
		t.Error("cookie berubah padahal input ditolak")
	}
}

// rotate_disabled harus terbaca lewat accountList — rotateAllAccounts memakai
// nilai itu untuk melewati akun yang sesinya dipegang browser.
func TestRotateDisabledFlagRoundTrip(t *testing.T) {
	withTestDB(t)

	id := insertTestAccount(t, "browser-owned")
	if err := accountSetRotateDisabled(id, true); err != nil {
		t.Fatalf("set true: %v", err)
	}
	var got int64 = -1
	for _, a := range accountList() {
		if a.ID == id {
			got = a.RotateDisabled
		}
	}
	if got != 1 {
		t.Fatalf("RotateDisabled=%d want 1", got)
	}

	if err := accountSetRotateDisabled(id, false); err != nil {
		t.Fatalf("set false: %v", err)
	}
	for _, a := range accountList() {
		if a.ID == id && a.RotateDisabled != 0 {
			t.Fatalf("RotateDisabled=%d want 0", a.RotateDisabled)
		}
	}
}

// Kegagalan di jalur keluar (proxy mati, DNS, timeout) tidak boleh menaikkan
// fail_count. Dulu WARP berhenti listen dan akun sehat dihukum tiga kali lalu
// auto-disable — persis gejala "cookie expired".
func TestTransportErrorDoesNotPenalizeAccount(t *testing.T) {
	withTestDB(t)

	id := insertTestAccount(t, "proxy-down")
	markAccountResult(id, false, `Get "https://gemini.google.com/app": proxyconnect tcp: `+
		`dial tcp 127.0.0.1:40000: connectex: No connection could be made because the `+
		`target machine actively refused it.`)
	if st, fails := accountStatusAndFails(t, id); fails != 0 || st != "enabled" {
		t.Fatalf("kegagalan transport dihitung sebagai salah cookie: status=%q fails=%d", st, fails)
	}

	// Kegagalan yang benar-benar soal sesi tetap dihukum.
	markAccountResult(id, false, "no SNlM0e in page (cookie expired or not signed in)")
	if _, fails := accountStatusAndFails(t, id); fails != 1 {
		t.Fatalf("kegagalan sesi tidak tercatat: fails=%d", fails)
	}

	for _, s := range []string{
		"dial tcp 1.2.3.4:443: i/o timeout",
		"socks connect tcp 127.0.0.1:52001->1.2.3.4:443: connection refused",
		`lookup gemini.google.com: no such host`,
	} {
		if !isTransportError(s) {
			t.Errorf("tidak dikenali sebagai transport: %q", s)
		}
	}
	if isTransportError("no SNlM0e in page") {
		t.Error("kegagalan sesi salah dikenali sebagai transport")
	}
}

func TestXSRFTokenFromError(t *testing.T) {
	// Bentuk asli: [["er",null,...,400,...,[{"48448350":["xsrf","<token>",...]}]]]
	raw := `[["er",null,null,null,null,null,null,null,null,null,[["di",62],["af.httprm",61,"-123",7]],` +
		`null,null,null,null,null,[[{"48448350":["xsrf","AOExLx9vQ2m3kPzR7tYw",null,null,null,` +
		`"AfmAbe9xYzQ"]}]],400,null,null,null,"generic"]]`
	if got := xsrfTokenFromError(raw); got != "AOExLx9vQ2m3kPzR7tYw" {
		t.Errorf("token=%q want %q", got, "AOExLx9vQ2m3kPzR7tYw")
	}

	for _, bad := range []string{
		"",
		`[["er",null,400]]`,
		`{"xsrf":"short"}`,           // terlalu pendek
		`{"xsrf":"has space in it"}`, // mengandung spasi
		`{"xsrf":""}`,                // kosong
	} {
		if got := xsrfTokenFromError(bad); got != "" {
			t.Errorf("respons %q seharusnya tidak menghasilkan token, dapat %q", bad, got)
		}
	}
}

// cacheXSRFFromError menyimpan token supaya request berikutnya tidak perlu
// fetch /app, tapi TIDAK boleh mengisi pushID/pctx dengan nilai kosong —
// upload file bergantung pada nilai dari halaman.
func TestCacheXSRFFromErrorKeepsUploadFields(t *testing.T) {
	cookie := "SID=abc; __Secure-1PSID=def"
	invalidateXSRF(cookie)
	t.Cleanup(func() { invalidateXSRF(cookie) })

	// Siapkan entri dengan nilai upload (seolah dari fetchAppTokens).
	key := cookieKey(cookie)
	xsrfMu.Lock()
	xsrfCache[key] = xsrfEntry{token: "old", pushID: "PUSH-1", pctx: "PCTX-1", fetched: time.Now()}
	xsrfMu.Unlock()

	raw := `[["er",[{"1":["xsrf","NEWTOKEN123456",null]}]]]`
	if got := cacheXSRFFromError(cookie, raw); got != "NEWTOKEN123456" {
		t.Fatalf("token=%q", got)
	}

	xsrfMu.Lock()
	e := xsrfCache[key]
	xsrfMu.Unlock()
	if e.token != "NEWTOKEN123456" {
		t.Errorf("token tidak terganti: %q", e.token)
	}
	if e.pushID != "PUSH-1" || e.pctx != "PCTX-1" {
		t.Errorf("nilai upload terhapus: pushID=%q pctx=%q (want PUSH-1/PCTX-1)", e.pushID, e.pctx)
	}

	// Respons tanpa token tidak boleh mengosongkan cache.
	if got := cacheXSRFFromError(cookie, `[["er",null,400]]`); got != "" {
		t.Errorf("token=%q want kosong", got)
	}
	xsrfMu.Lock()
	e2 := xsrfCache[key]
	xsrfMu.Unlock()
	if e2.token != "NEWTOKEN123456" {
		t.Errorf("cache terhapus oleh respons tanpa token: %q", e2.token)
	}
}
