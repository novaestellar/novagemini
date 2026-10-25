package app

// /api/media + /gallery —— 产物画廊。
//
// 异步媒体任务把成品写到 data/media(见 media_store.go),这里给出两个只读接口:
//   GET /api/media  → 递归列出 data/media 下的文件(最新在前,最多 500 条)
//   GET /gallery    → 302 到面板的 #gallery 页
//
// 画廊原来是这里一张自包含的深色页面:配色、字体、布局都是另一套,跟面板并列着
// 看就像两个产品。现在只有一份实现 —— 由 admin_ui/index.html 的 gallery 页渲染,
// 这个路径只负责把人送过去,带上 #gallery 让面板直接落在那一页。

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// mediaListEntry 是 /api/media 列表里的一项。
type mediaListEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"` // 相对 mediaDir,正斜杠
	URL   string `json:"url"`  // /files/<rel>
	Bytes int64  `json:"bytes"`
	Mtime int64  `json:"mtime"`
	Mime  string `json:"mime"`
}

// mediaListCap 是 /api/media 返回条数的上限,防止目录很大时一次吐太多。
const mediaListCap = 500

// handleMediaList 处理 GET /api/media:递归遍历 mediaDir,最新在前。
func handleMediaList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	root, err := filepath.Abs(mediaDir())
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "media dir unavailable"})
		return
	}
	entries := make([]mediaListEntry, 0, 64)
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // 跳过读不到的项目,不中断整次遍历
		}
		if d.IsDir() {
			return nil
		}
		st, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		entries = append(entries, mediaListEntry{
			Name:  d.Name(),
			Path:  relSlash,
			URL:   "/files/" + relSlash,
			Bytes: st.Size(),
			Mtime: st.ModTime().Unix(),
			Mime:  mimeFromExt(d.Name()),
		})
		return nil
	})
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Mtime != entries[j].Mtime {
			return entries[i].Mtime > entries[j].Mtime
		}
		return entries[i].Path > entries[j].Path
	})
	if len(entries) > mediaListCap {
		entries = entries[:mediaListCap]
	}
	writeJSON(w, 200, entries)
}

// mimeFromExt menurunkan MIME dari ekstensi file (kebalikan artifactExt).
func mimeFromExt(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	case ".ogg":
		return "audio/ogg"
	case ".mp4":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".html", ".htm":
		return "text/html"
	default:
		return "application/octet-stream"
	}
}

// handleGallery 把旧的独立画廊页 302 到面板里的 #gallery。
//
// 目标直接写带斜杠的 /admin/#gallery:面板对 /admin 还会再 301 一次,而那次跳转
// 能不能保住 fragment 取决于浏览器(服务端说什么都不算数)。落到终态的 /admin/
// 就只有一跳,fragment 是自己带过去的,不依赖任何继承规则。绝对路径跟
// /api/media、/files/ 一样按站点根挂载。
func handleGallery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Location", "/admin/#gallery")
	w.WriteHeader(http.StatusFound)
}
