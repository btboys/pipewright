package build

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// 本文件锁死「克隆失败必须说清楚原因」这条契约:用户看到的日志/failure_log 里要有可排查的
// 分类原因,而不是一句「鉴权/网络/ref 不存在或被 SSRF 拒绝」让人猜。

func TestCloneFailureText(t *testing.T) {
	if got := CloneFailureText(""); !strings.Contains(got, "鉴权/网络/ref") {
		t.Fatalf("空 detail 应回退到通用文案, got %q", got)
	}
	got := CloneFailureText("鉴权失败:远端要求认证")
	if !strings.HasPrefix(got, "源码克隆失败:") || !strings.Contains(got, "远端要求认证") {
		t.Fatalf("detail 应拼进文案, got %q", got)
	}
}

// TestClonerBlocksSSRFWithReason 覆盖 SSRF 收口:被拒时既保持 ErrRepoBlocked 的 errors.Is 语义,
// 又说清是 scheme 不支持还是命中了回环/链路本地地址。
func TestClonerBlocksSSRFWithReason(t *testing.T) {
	c := NewCloner() // 生产严格收口
	cases := []struct{ name, url, wantDetail string }{
		{"scheme-file", "file:///tmp/repo", "仅支持 http/https"},
		{"scheme-ssh", "ssh://git@github.com/o/r.git", "仅支持 http/https"},
		{"scheme-empty", "/tmp/repo", "仅支持 http/https"},
		{"loopback-v4", "http://127.0.0.1/o/r.git", "回环"},
		{"metadata", "http://169.254.169.254/latest/meta-data", "链路本地"},
		{"loopback-v6", "http://[::1]/o/r.git", "回环"},
		{"missing-host", "https:///o/r.git", "缺少主机名"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			_, err := c.Clone(context.Background(), tc.url, "", "", "", "", dir)
			if !errors.Is(err, ErrRepoBlocked) {
				t.Fatalf("want ErrRepoBlocked, got %v", err)
			}
			var ce *CloneError
			if !errors.As(err, &ce) {
				t.Fatalf("want *CloneError, got %T", err)
			}
			if ce.Reason != ReasonRepoBlocked {
				t.Fatalf("want reason %q, got %q", ReasonRepoBlocked, ce.Reason)
			}
			if !strings.Contains(err.Error(), tc.wantDetail) {
				t.Fatalf("detail 应含 %q, got %q", tc.wantDetail, err.Error())
			}
		})
	}
}

// TestClonerEmptyRepoURLReasons 覆盖「项目/节点没配仓库地址」这条最常见的配置错误。
func TestClonerEmptyRepoURLReasons(t *testing.T) {
	_, err := NewCloner().Clone(context.Background(), "   ", "", "", "", "", t.TempDir())
	if !errors.Is(err, ErrCloneFailed) {
		t.Fatalf("want ErrCloneFailed, got %v", err)
	}
	var ce *CloneError
	if !errors.As(err, &ce) || ce.Reason != ReasonEmptyRepoURL {
		t.Fatalf("want reason %q, got %+v", ReasonEmptyRepoURL, err)
	}
	if !strings.Contains(err.Error(), "未配置仓库地址") {
		t.Fatalf("detail 应点明未配置仓库地址, got %q", err.Error())
	}
}

// TestClonerClassifiesNetworkFailures 无需外网即可覆盖 DNS 失败与连接被拒两条网络分支。
func TestClonerClassifiesNetworkFailures(t *testing.T) {
	t.Run("dns", func(t *testing.T) {
		if testing.Short() {
			t.Skip("需要 DNS 解析失败路径")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := NewCloner().Clone(ctx, "https://no-such-host-pipewright.invalid/o/r.git", "", "", "", "", t.TempDir())
		assertReason(t, err, ReasonDNSFailed, "域名解析失败")
	})
	t.Run("conn-refused", func(t *testing.T) {
		// 起一个监听后立刻关闭的端口,拿到一个必然拒绝连接的地址。
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL + "/o/r.git"
		srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// allowInsecure:回环地址在生产会被 SSRF 收口先行拦下,这里要验的是「拨号失败」分类本身。
		_, err := (&Cloner{allowInsecure: true}).Clone(ctx, url, "", "", "", "", t.TempDir())
		if err == nil {
			t.Fatal("want error")
		}
		var ce *CloneError
		if !errors.As(err, &ce) {
			t.Fatalf("want *CloneError, got %T (%v)", err, err)
		}
		// 端口关闭后可能是 connection refused 或超时(取决于内核/复用),两者都是有效分类。
		if ce.Reason != ReasonConnRefused && ce.Reason != ReasonTimeout {
			t.Fatalf("want conn_refused/timeout, got %q (%s)", ce.Reason, ce.Detail)
		}
	})
}

// TestClonerClassifiesAuthFailure 用本地假 Git 服务回 401,确定性覆盖鉴权失败分类
// (生产里对应「凭据过期/用户名与令牌不匹配」)。
func TestClonerClassifiesAuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := (&Cloner{allowInsecure: true}).Clone(ctx, srv.URL+"/o/r.git", "user", "bad-token", "", "", t.TempDir())
	assertReason(t, err, ReasonAuthFailed, "鉴权失败")
	if strings.Contains(err.Error(), "bad-token") {
		t.Fatalf("失败文本绝不能带回令牌: %q", err.Error())
	}
	// 日志里必须给出可执行的下一步,而不是让用户猜。
	text := CloneFailureText(cloneFailureDetail(err))
	if !strings.Contains(text, "凭据") {
		t.Fatalf("失败文案应给出凭据相关的下一步, got %q", text)
	}
}

func TestCloneFailureDetailUsesHint(t *testing.T) {
	err := &CloneError{Reason: ReasonRefNotFound, Detail: "分支/commit 在远端不存在", Hint: "请核对触发分支/commit 是否已推送到远端", err: ErrCloneFailed}
	got := cloneFailureDetail(err)
	if !strings.Contains(got, "分支/commit 在远端不存在") || !strings.Contains(got, "请核对触发分支") {
		t.Fatalf("detail 应「原因 → 下一步」, got %q", got)
	}
	if text := CloneFailureText(got); !strings.HasPrefix(text, "源码克隆失败:") || !strings.Contains(text, "请核对触发分支") {
		t.Fatalf("完整文案应含下一步, got %q", text)
	}
}

// TestCloneFailureDetailNeverLeaksCredentials 锁死脱敏:底层文本里出现的内嵌凭据 URL
// 不得出现在日志/failure_log 文本里。
func TestCloneFailureDetailNeverLeaksCredentials(t *testing.T) {
	raw := errors.New(`Get "https://user:sup3r-secret@git.example.com/o/r.git/info/refs": dial tcp: connection refused`)
	got := cloneFailureDetail(raw)
	if strings.Contains(got, "sup3r-secret") || strings.Contains(got, "git.example.com") {
		t.Fatalf("detail 泄漏了 URL/凭据: %q", got)
	}
	if !strings.Contains(got, "<url>") {
		t.Fatalf("URL 应被占位替换, got %q", got)
	}
	// 分类错误走 Detail(+ Hint),不再二次处理。
	if got := cloneFailureDetail(&CloneError{Reason: ReasonRefNotFound, Detail: "分支不存在", err: ErrCloneFailed}); got != "分支不存在" {
		t.Fatalf("CloneError 应直接用分类 Detail, got %q", got)
	}
}

// TestClonerRefNotFoundAgainstLocalFixture 用本地夹具仓库确定性覆盖「分支不存在」分类
// (allowInsecure 放行 file://,不触网)。
func TestClonerRefNotFoundAgainstLocalFixture(t *testing.T) {
	src := newLocalFixtureRepo(t)
	c := &Cloner{allowInsecure: true}
	_, err := c.Clone(context.Background(), "file://"+src, "", "", "no-such-branch-xyz", "", t.TempDir())
	if err == nil {
		t.Fatal("want error for missing branch")
	}
	if !errors.Is(err, ErrCloneFailed) {
		t.Fatalf("want ErrCloneFailed, got %v", err)
	}
	var ce *CloneError
	if !errors.As(err, &ce) {
		t.Fatalf("want *CloneError, got %T (%v)", err, err)
	}
	if ce.Reason != ReasonRefNotFound {
		t.Fatalf("want %q, got %q (%s)", ReasonRefNotFound, ce.Reason, ce.Detail)
	}
	// 分支名大小写敏感:远端回显的 ref 必须原样保留,方便用户对照。
	if !strings.Contains(ce.Detail, "no-such-branch-xyz") {
		t.Fatalf("detail 应原样保留分支名, got %q", ce.Detail)
	}
}

// TestScrubURLsAndFirstQuoted 锁死底层文本的脱敏与 ref 提取:URL(可能内嵌 userinfo)必须被占位,
// 引号片段原样保留。
func TestScrubURLsAndFirstQuoted(t *testing.T) {
	raw := `couldn't find remote ref "refs/heads/Release-1.2" for https://u:tok3n@git.example.com/o/r.git`
	if got := scrubURLs(raw); strings.Contains(got, "tok3n") || strings.Contains(got, "git.example.com") {
		t.Fatalf("scrubURLs 泄漏凭据/主机: %q", got)
	}
	if got := firstQuoted(raw); got != "refs/heads/Release-1.2" {
		t.Fatalf("firstQuoted 应原样返回引号内 ref, got %q", got)
	}
	// 无引号时回退整段,但仍去 URL。
	if got := firstQuoted(`boom https://u:p@h/x.git`); strings.Contains(got, "u:p@h") {
		t.Fatalf("无引号回退仍需脱敏, got %q", got)
	}
}

func assertReason(t *testing.T, err error, want CloneReason, wantDetail string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error with reason %q, got nil", want)
	}
	var ce *CloneError
	if !errors.As(err, &ce) {
		t.Fatalf("want *CloneError, got %T (%v)", err, err)
	}
	if ce.Reason != want {
		t.Fatalf("want reason %q, got %q (%s)", want, ce.Reason, ce.Detail)
	}
	if wantDetail != "" && !strings.Contains(ce.Detail, wantDetail) {
		t.Fatalf("detail 应含 %q, got %q", wantDetail, ce.Detail)
	}
}

// newLocalFixtureRepo 造一个本地单分支夹具仓库(main 上一个 commit),返回其绝对路径。
// 供「分支不存在」分类用:file:// 经 allowInsecure 放行,全程不触网。
func newLocalFixtureRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("fixture"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := wt.Add("README.md"); err != nil {
		t.Fatalf("add: %v", err)
	}
	sig := &object.Signature{Name: "t", Email: "t@x", When: time.Now()}
	if _, err := wt.Commit("c1", &gogit.CommitOptions{Author: sig}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return dir
}
