package app

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain 把 DB 指向临时目录再跑测试。
//
// hasCookie() 现在只看 cookie 池，也就是要查 DB；不接管 cfg.DBPath 的话
// getDB() 会去开真实的 ./data/gemini.db，测试就会读写生产数据。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gw2a-test-")
	if err != nil {
		panic(err)
	}
	cfg.DBPath = filepath.Join(dir, "test.db")
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// withPoolCookie 往 cookie 池塞一条测试账号，用完自动删掉。
// 取代原来的 cookieRuntime.Store —— 那条单 cookie 路径已经取消。
func withPoolCookie(t *testing.T) {
	t.Helper()
	id, err := accountAdd("test", "SAPISID=dummy; SID=x", "")
	if err != nil {
		t.Fatalf("插测试 cookie 失败: %v", err)
	}
	t.Cleanup(func() { _ = accountDelete(id) })
}

// withTestDB menjalankan f dengan DB yang bersih lalu memulihkan isinya.
//
// DB tes dipakai bersama seluruh paket (TestMain menetapkan path sekali), jadi
// tes yang butuh "tidak ada akun lain" harus mengosongkan dan mengembalikan.
// Dipakai oleh tes siklus hidup akun (auto-disable / revive), yang hasilnya
// bergantung pada jumlah baris di tabel accounts.
func withTestDB(t *testing.T) {
	t.Helper()
	db := getDB()

	// Simpan isi lama supaya tes lain tidak terganggu.
	type row struct {
		id                                                  int64
		label, cookie, status, note, lastError              string
		createdAt, lastUsedAt, lastOkAt, failCount, proxyID int64
	}
	var saved []row
	rows, err := db.Query(`SELECT id,label,cookie,status,note,created_at,last_used_at,
		last_ok_at,fail_count,proxy_id,last_error FROM accounts`)
	if err == nil {
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.label, &r.cookie, &r.status, &r.note,
				&r.createdAt, &r.lastUsedAt, &r.lastOkAt, &r.failCount, &r.proxyID,
				&r.lastError); err == nil {
				saved = append(saved, r)
			}
		}
		rows.Close()
	}
	if _, err := db.Exec(`DELETE FROM accounts`); err != nil {
		t.Fatalf("kosongkan accounts: %v", err)
	}

	t.Cleanup(func() {
		_, _ = getDB().Exec(`DELETE FROM accounts`)
		for _, r := range saved {
			_, _ = getDB().Exec(`INSERT INTO accounts
				(id,label,cookie,status,note,created_at,last_used_at,last_ok_at,fail_count,proxy_id,last_error)
				VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
				r.id, r.label, r.cookie, r.status, r.note, r.createdAt, r.lastUsedAt,
				r.lastOkAt, r.failCount, r.proxyID, r.lastError)
		}
	})
}

// insertTestAccount menambah satu akun uji dan mengembalikan id-nya.
func insertTestAccount(t *testing.T, label string) int64 {
	t.Helper()
	id, err := accountAdd(label, "SAPISID=dummy; SID=x", "")
	if err != nil {
		t.Fatalf("accountAdd: %v", err)
	}
	return id
}

// accountStatusAndFails membaca status + fail_count langsung dari DB.
func accountStatusAndFails(t *testing.T, id int64) (string, int64) {
	t.Helper()
	var status string
	var fails int64
	if err := getDB().QueryRow(`SELECT status, fail_count FROM accounts WHERE id=?`, id).
		Scan(&status, &fails); err != nil {
		t.Fatalf("baca akun #%d: %v", id, err)
	}
	return status, fails
}
