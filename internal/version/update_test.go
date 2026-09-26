package version

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v1.2.3", "v1.2.3", 0},
		{"1.2.3", "v1.2.3", 0}, // 前导 v 可有可无
		{"v1.2.4", "v1.2.3", 1},
		{"v1.2.3", "v1.2.4", -1},
		{"v1.3.0", "v1.2.9", 1},
		{"v2.0.0", "v1.9.9", 1},
		{"v1.2", "v1.2.0", 0},        // 缺位补 0
		{"v1.0.0", "v1.0.0-rc.1", 1}, // 正式版高于预发布
		{"v1.0.0-rc.1", "v1.0.0", -1},
		{"v1.0.0-rc.2", "v1.0.0-rc.1", 1},
		{"v1.0.0-rc.1", "v1.0.0-rc.1", 0},
		{"v1.0.0-beta", "v1.0.0-rc", -1}, // 字典序 beta < rc
		{"v1.0.0+build.5", "v1.0.0", 0},  // build 元数据不参与比较
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestIsComparable(t *testing.T) {
	for v, want := range map[string]bool{
		"v1.2.3":      true,
		"1.2.3":       true,
		"v1.0.0-rc.1": true,
		"dev":         false,
		"none":        false,
		"":            false,
		"latest":      false,
	} {
		if got := isComparable(v); got != want {
			t.Errorf("isComparable(%q)=%v want %v", v, got, want)
		}
	}
}

// newStubChecker 指向本地 stub server,并钉死当前版本与时钟。
func newStubChecker(t *testing.T, current string, h http.HandlerFunc) (*Checker, func()) {
	t.Helper()
	srv := httptest.NewServer(h)
	prevBase, prevHTML, prevVer := apiBase, htmlBase, Version
	apiBase = srv.URL
	htmlBase = srv.URL // 同一 stub 兼当网页站,避免重定向兜底打到真实 github.com
	Version = current
	cleanup := func() {
		apiBase = prevBase
		htmlBase = prevHTML
		Version = prevVer
		srv.Close()
	}
	c := &Checker{repo: "owner/repo", client: srv.Client()}
	return c, cleanup
}

func TestCheck_UpdateAvailable(t *testing.T) {
	c, done := newStubChecker(t, "v1.0.0", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v1.2.0","html_url":"https://example/releases/v1.2.0","published_at":"2026-06-06T00:00:00Z","body":"notes"}`))
	})
	defer done()

	info := c.Check(context.Background())
	if info.CheckError != "" {
		t.Fatalf("unexpected checkError: %s", info.CheckError)
	}
	if !info.UpdateAvailable {
		t.Errorf("expected updateAvailable=true (v1.0.0 → v1.2.0)")
	}
	if info.Current != "v1.0.0" || info.Latest != "v1.2.0" {
		t.Errorf("current=%q latest=%q", info.Current, info.Latest)
	}
	if info.ReleaseURL == "" || info.Notes != "notes" {
		t.Errorf("missing release fields: %+v", info)
	}
}

func TestCheck_AlreadyLatest(t *testing.T) {
	c, done := newStubChecker(t, "v1.2.0", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v1.2.0"}`))
	})
	defer done()
	if info := c.Check(context.Background()); info.UpdateAvailable {
		t.Errorf("expected no update when current==latest")
	}
}

func TestCheck_DevVersionNeverPrompts(t *testing.T) {
	c, done := newStubChecker(t, "dev", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9"}`))
	})
	defer done()
	info := c.Check(context.Background())
	if info.UpdateAvailable {
		t.Errorf("dev build must not report an update")
	}
	if info.Latest != "v9.9.9" {
		t.Errorf("latest should still be reported: %q", info.Latest)
	}
}

func TestCheck_NoReleasesSoftFails(t *testing.T) {
	c, done := newStubChecker(t, "v1.0.0", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	defer done()
	info := c.Check(context.Background())
	if info.CheckError != "" {
		t.Errorf("404 (no releases) should be soft, got error %q", info.CheckError)
	}
	if info.UpdateAvailable || info.Latest != "" {
		t.Errorf("no releases → no update, no latest")
	}
}

func TestCheck_RateLimitedReportsError(t *testing.T) {
	c, done := newStubChecker(t, "v1.0.0", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	defer done()
	if info := c.Check(context.Background()); info.CheckError == "" {
		t.Errorf("403 should surface a checkError")
	}
}

// API 限流(403)时,退回网页站 /releases/latest 的 302 重定向解析出最新 tag,
// 并从 releases.atom(非限流)补出 release 说明。
func TestCheck_RateLimitFallsBackToRedirect(t *testing.T) {
	const atom = `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <entry>
    <id>tag:github.com,2008:Repository/123/v1.2.0</id>
    <title>Pipewright v1.2.0</title>
    <content type="html">&lt;h2&gt;Changelog&lt;/h2&gt;&lt;ul&gt;&lt;li&gt;feat: 新增 SSH 密码凭据 (#100)&lt;/li&gt;&lt;/ul&gt;</content>
  </entry>
</feed>`
	c, done := newStubChecker(t, "v1.0.0", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/repos/"):
			w.WriteHeader(http.StatusForbidden) // API 限流
		case strings.HasSuffix(r.URL.Path, "/releases.atom"):
			_, _ = w.Write([]byte(atom))
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			w.Header().Set("Location", "/owner/repo/releases/tag/v1.2.0")
			w.WriteHeader(http.StatusFound) // 网页站 302 → tag
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	defer done()

	info := c.Check(context.Background())
	if info.CheckError != "" {
		t.Fatalf("重定向兜底应清空 checkError,实得 %q", info.CheckError)
	}
	if info.Latest != "v1.2.0" || !info.UpdateAvailable {
		t.Errorf("应经重定向得到 latest=v1.2.0 updateAvailable=true,实得 %+v", info)
	}
	if !strings.Contains(info.Notes, "SSH 密码凭据") || !strings.Contains(info.Notes, "• ") {
		t.Errorf("应从 atom 补出 release 说明(去标签转纯文本 + li 转项目符号),实得 %q", info.Notes)
	}
}

func TestHtmlToText(t *testing.T) {
	got := htmlToText(`<h2>标题</h2><ul><li>第一条 <a href="x">#1</a></li><li>第二条</li></ul><hr><p>结尾 &amp; 收尾</p>`)
	for _, want := range []string{"标题", "• 第一条 #1", "• 第二条", "结尾 & 收尾"} {
		if !strings.Contains(got, want) {
			t.Errorf("htmlToText 缺 %q,实得:\n%s", want, got)
		}
	}
	if strings.Contains(got, "<") || strings.Contains(got, "href") {
		t.Errorf("htmlToText 未去净标签: %q", got)
	}
}

// 每次 Check 都必须真查 GitHub:这两个调用方(「检查更新」按钮、自更新)都是用户主动动作,
// 吃到过期结果会让人以为刚发的版没生效,自更新更会照着旧版本号替换自己的二进制。
func TestCheck_AlwaysQueriesUpstream(t *testing.T) {
	var hits int
	c, done := newStubChecker(t, "v1.0.0", func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"tag_name":"v1.1.0"}`))
	})
	defer done()
	_ = c.Check(context.Background())
	_ = c.Check(context.Background())
	if hits != 2 {
		t.Errorf("expected 2 upstream hits (no caching), got %d", hits)
	}
}

// 新版本发布后立刻再查必须立刻看到 —— 这是用户报告「发完版还显示旧版本」的那条路径。
func TestCheck_SeesNewReleaseImmediately(t *testing.T) {
	var tag = "v1.1.0"
	c, done := newStubChecker(t, "v1.0.0", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"` + tag + `"}`))
	})
	defer done()

	if got := c.Check(context.Background()).Latest; got != "v1.1.0" {
		t.Fatalf("首次应看到 v1.1.0, got %q", got)
	}
	tag = "v1.2.0" // 模拟上游刚发布新版
	info := c.Check(context.Background())
	if info.Latest != "v1.2.0" {
		t.Fatalf("上游发布新版后应立即看到 v1.2.0, got %q", info.Latest)
	}
	if !info.UpdateAvailable {
		t.Fatal("应判定有更新")
	}
}
