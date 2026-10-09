package trigger

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"maps"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btboys/pipewright/internal/vault"
)

// fakeRunCreator 记录被请求创建的运行(不触 run 包,避免领域互引)。
type fakeRunCreator struct {
	mu    sync.Mutex
	calls []RunRequest
	id    string
}

func (f *fakeRunCreator) CreateWebhookRun(_ context.Context, _ string, in RunRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, in)
	if f.id == "" {
		f.id = "run-1"
	}
	return f.id, nil
}

func (f *fakeRunCreator) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// hdrs 把明文头表包成 HeaderLookup(键大小写不敏感,模拟 net/http.Header.Get)。
func hdrs(kv map[string]string) HeaderLookup {
	lower := make(map[string]string, len(kv))
	for k, v := range kv {
		lower[strings.ToLower(k)] = v
	}
	return func(name string) string { return lower[strings.ToLower(name)] }
}

// giteeHeaders 构造 Gitee 风格头查找(空 timestamp / deliveryID 不设头)。
func giteeHeaders(event, tokenHdr, timestamp, deliveryID string) HeaderLookup {
	h := map[string]string{HeaderGiteeEvent: event, HeaderGiteeToken: tokenHdr}
	if timestamp != "" {
		h[HeaderGiteeTimestamp] = timestamp
	}
	if deliveryID != "" {
		h[HeaderGiteeDelivery] = deliveryID
	}
	return hdrs(h)
}

// newReceiver 装配 Receiver + 已配 push 事件 + 分支映射 main→prod 的项目;返回 token、明文密钥。
func newReceiver(t *testing.T) (*Receiver, *fakeRunCreator, string, string) {
	t.Helper()
	db, _ := testDB(t)
	v := vault.New(db, testMasterKey())
	svc := New(db, v)
	projID := seedProject(t, db)

	ctx := context.Background()
	cfg, err := svc.Get(ctx, projID)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	token := cfg.WebhookToken

	reset, err := svc.ResetSecret(ctx, projID)
	if err != nil {
		t.Fatalf("reset secret: %v", err)
	}
	secret := reset.Secret

	if _, err := svc.Save(ctx, projID, SaveInput{
		Events: Events{Push: true},
		BranchMappings: []BranchMapping{
			{BranchPattern: "main", Environment: "prod", TargetServerIDs: []string{"srv-1", "srv-2"}},
			{BranchPattern: "release/*", Environment: "staging", TargetServerIDs: []string{"srv-3"}},
		},
		UnmatchedPolicy: PolicyIgnore,
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}

	fake := &fakeRunCreator{}
	rc := NewReceiver(db, v, fake)
	return rc, fake, token, secret
}

func pushBody(branch, commit string) []byte {
	return []byte(`{"ref":"refs/heads/` + branch + `","after":"` + commit + `"}`)
}

func TestWebhookPasswordModeCreatesRun(t *testing.T) {
	rc, fake, token, secret := newReceiver(t)
	res, err := rc.Handle(context.Background(), Delivery{
		Token:   token,
		Header:  giteeHeaders(eventPush, secret, "", "d-1"),
		RawBody: pushBody("main", "abc123"),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !res.Accepted || res.RunID == "" {
		t.Fatalf("expected accepted run, got %+v", res)
	}
	if fake.count() != 1 {
		t.Fatalf("expected 1 run created, got %d", fake.count())
	}
	got := fake.calls[0]
	if got.Branch != "main" || got.Commit != "abc123" {
		t.Fatalf("unexpected run req: %+v", got)
	}
	if got.ResolvedEnvironment != "prod" {
		t.Fatalf("expected resolved env prod, got %q", got.ResolvedEnvironment)
	}
	if len(got.ResolvedTargetServerIDs) != 2 {
		t.Fatalf("expected 2 target servers, got %v", got.ResolvedTargetServerIDs)
	}
	if got.Actor != "gitee" {
		t.Fatalf("expected actor gitee, got %q", got.Actor)
	}
}

func TestWebhookSignatureModeCreatesRun(t *testing.T) {
	rc, fake, token, secret := newReceiver(t)
	// 新鲜 timestamp(秒 epoch),落在防重放窗口内。
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	res, err := rc.Handle(context.Background(), Delivery{
		Token:   token,
		Header:  giteeHeaders(eventPush, sig, ts, "d-sig"),
		RawBody: pushBody("main", "deadbeef"),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !res.Accepted {
		t.Fatalf("expected accepted, got %+v", res)
	}
	if fake.count() != 1 {
		t.Fatalf("expected 1 run, got %d", fake.count())
	}
}

// TestWebhookSignatureReplayRejected 验证防重放:旧 timestamp(超窗)即便签名正确也 401。
func TestWebhookSignatureReplayRejected(t *testing.T) {
	rc, fake, token, secret := newReceiver(t)
	ts := "1700000000" // 2023,远超 5min 窗口
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	_, err := rc.Handle(context.Background(), Delivery{
		Token:   token,
		Header:  giteeHeaders(eventPush, sig, ts, "d-replay"),
		RawBody: pushBody("main", "deadbeef"),
	})
	if err != ErrUnauthorized {
		t.Fatalf("旧 timestamp 应 401(防重放), got %v", err)
	}
	if fake.count() != 0 {
		t.Fatalf("重放不应创建运行, got %d", fake.count())
	}
}

// TestWebhookSignatureMissingTimestampRejected 验证签名模式缺失 timestamp → 401。
func TestWebhookSignatureMissingTimestampRejected(t *testing.T) {
	rc, _, token, secret := newReceiver(t)
	// 用空 timestamp 计算的签名(攻击者可构造),但缺失 timestamp 头本身应被拒。
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("\n" + secret))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	_, err := rc.Handle(context.Background(), Delivery{
		Token:   token,
		Header:  giteeHeaders(eventPush, sig, "", "d-no-ts"),
		RawBody: pushBody("main", "deadbeef"),
	})
	if err != ErrUnauthorized {
		t.Fatalf("签名模式缺 timestamp 应 401, got %v", err)
	}
}

// TestWebhookTimestampMillisAccepted 验证毫秒 epoch 也被正确解析为新鲜。
func TestWebhookTimestampMillisAccepted(t *testing.T) {
	rc, fake, token, secret := newReceiver(t)
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10) // 13 位毫秒
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	res, err := rc.Handle(context.Background(), Delivery{
		Token:   token,
		Header:  giteeHeaders(eventPush, sig, ts, "d-ms"),
		RawBody: pushBody("main", "deadbeef"),
	})
	if err != nil {
		t.Fatalf("Handle(ms ts): %v", err)
	}
	if !res.Accepted || fake.count() != 1 {
		t.Fatalf("毫秒 timestamp 应被接受, res=%+v count=%d", res, fake.count())
	}
}

func TestWebhookWrongSecretUnauthorized(t *testing.T) {
	rc, fake, token, _ := newReceiver(t)
	_, err := rc.Handle(context.Background(), Delivery{
		Token:   token,
		Header:  giteeHeaders(eventPush, "whsec_wrong", "", "d-bad"),
		RawBody: pushBody("main", "x"),
	})
	if err != ErrUnauthorized {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
	if fake.count() != 0 {
		t.Fatalf("expected no run on bad secret, got %d", fake.count())
	}
}

func TestWebhookEventNotSubscribed(t *testing.T) {
	rc, fake, token, secret := newReceiver(t)
	// Tag 未勾选(仅 push 开)。
	res, err := rc.Handle(context.Background(), Delivery{
		Token:   token,
		Header:  giteeHeaders(eventTag, secret, "", "d-tag"),
		RawBody: []byte(`{"ref":"refs/tags/v1","after":"t1"}`),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.Accepted || res.Ignored != IgnoredEventNotSubscribed {
		t.Fatalf("expected event_not_subscribed, got %+v", res)
	}
	if fake.count() != 0 {
		t.Fatalf("expected no run, got %d", fake.count())
	}
}

func TestWebhookNoBranchMatchIgnored(t *testing.T) {
	rc, fake, token, secret := newReceiver(t)
	// UnmatchedPolicy=ignore;dev 不匹配任何映射。
	res, err := rc.Handle(context.Background(), Delivery{
		Token:   token,
		Header:  giteeHeaders(eventPush, secret, "", "d-dev"),
		RawBody: pushBody("dev", "c1"),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.Accepted || res.Ignored != IgnoredUnmatchedIgnored {
		t.Fatalf("expected unmatched_ignored, got %+v", res)
	}
	if fake.count() != 0 {
		t.Fatalf("expected no run, got %d", fake.count())
	}
}

func TestWebhookWildcardBranchMatch(t *testing.T) {
	rc, fake, token, secret := newReceiver(t)
	res, err := rc.Handle(context.Background(), Delivery{
		Token:   token,
		Header:  giteeHeaders(eventPush, secret, "", "d-rel"),
		RawBody: pushBody("release/1.2", "r12"),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !res.Accepted {
		t.Fatalf("expected accepted for release/1.2, got %+v", res)
	}
	if fake.calls[0].ResolvedEnvironment != "staging" {
		t.Fatalf("expected staging env, got %q", fake.calls[0].ResolvedEnvironment)
	}
}

func TestWebhookDuplicateDeliveryIgnored(t *testing.T) {
	rc, fake, token, secret := newReceiver(t)
	d := Delivery{
		Token:   token,
		Header:  giteeHeaders(eventPush, secret, "", "dup-1"),
		RawBody: pushBody("main", "samecommit"),
	}
	first, err := rc.Handle(context.Background(), d)
	if err != nil || !first.Accepted {
		t.Fatalf("first delivery should be accepted: %+v err=%v", first, err)
	}
	second, err := rc.Handle(context.Background(), d)
	if err != nil {
		t.Fatalf("second Handle: %v", err)
	}
	if second.Accepted || second.Ignored != IgnoredDuplicate {
		t.Fatalf("expected duplicate ignored, got %+v", second)
	}
	if fake.count() != 1 {
		t.Fatalf("expected exactly 1 run despite duplicate, got %d", fake.count())
	}
}

// TestDerivedKeyNoColonCollision 验证派生去重键对含 ':' 的分支名不碰撞
// (长度前缀+hash 消歧:不同分段不能产生相同键)。
func TestDerivedKeyNoColonCollision(t *testing.T) {
	// 旧实现 "derived:event:branch:commit" 下,这两组会拼成相同串。
	a := derivedKey("push", "a:b", "c")
	b := derivedKey("push", "a", "b:c")
	if a == b {
		t.Fatalf("含冒号分支名派生键碰撞: %q == %q", a, b)
	}
	// 空 payload(空 branch/commit)与非空也应区分。
	if derivedKey("push", "", "") == derivedKey("push", "x", "") {
		t.Fatalf("空 payload 与非空派生键碰撞")
	}
}

// TestWebhookUnmatchedRecordIdempotent 验证未匹配 record 路径走同一 dedup claim:
// 同 delivery 重复投递不无界灌库(第二次判 duplicate),只入一行。
func TestWebhookUnmatchedRecordIdempotent(t *testing.T) {
	db, _ := testDB(t)
	v := vault.New(db, testMasterKey())
	svc := New(db, v)
	projID := seedProject(t, db)
	ctx := context.Background()
	cfg, err := svc.Get(ctx, projID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	reset, _ := svc.ResetSecret(ctx, projID)
	// Push 订阅,但无任何分支映射 + policy=record(分支必不匹配 → unmatched_recorded)。
	if _, err := svc.Save(ctx, projID, SaveInput{
		Events:          Events{Push: true},
		BranchMappings:  []BranchMapping{},
		UnmatchedPolicy: PolicyRecord,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	rc := NewReceiver(db, v, &fakeRunCreator{})
	d := Delivery{
		Token:   cfg.WebhookToken,
		Header:  giteeHeaders(eventPush, reset.Secret, "", "rec-1"),
		RawBody: pushBody("feature/x", "c1"),
	}
	r1, err := rc.Handle(ctx, d)
	if err != nil || r1.Ignored != IgnoredUnmatchedRecorded {
		t.Fatalf("first: %+v err=%v, want unmatched_recorded", r1, err)
	}
	r2, err := rc.Handle(ctx, d)
	if err != nil || r2.Ignored != IgnoredDuplicate {
		t.Fatalf("second(same delivery): %+v err=%v, want duplicate", r2, err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(1) FROM webhook_deliveries WHERE project_id = ?`, projID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("同 delivery 应只入 1 行(防无界灌库), got %d", n)
	}
}

// newTagReleaseReceiver 装配 Receiver + 已配 tag+release 事件 + 分支映射 v*→prod 的项目;
// 返回 token、明文密钥(供 tag/release 上下文与订阅用例复用)。
func newTagReleaseReceiver(t *testing.T) (*Receiver, *fakeRunCreator, string, string) {
	t.Helper()
	db, _ := testDB(t)
	v := vault.New(db, testMasterKey())
	svc := New(db, v)
	projID := seedProject(t, db)

	ctx := context.Background()
	cfg, err := svc.Get(ctx, projID)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	token := cfg.WebhookToken

	reset, err := svc.ResetSecret(ctx, projID)
	if err != nil {
		t.Fatalf("reset secret: %v", err)
	}
	secret := reset.Secret

	if _, err := svc.Save(ctx, projID, SaveInput{
		Events: Events{Tag: true, Release: true},
		BranchMappings: []BranchMapping{
			{BranchPattern: "v*", Environment: "prod", TargetServerIDs: []string{"srv-1"}},
		},
		UnmatchedPolicy: PolicyIgnore,
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}

	fake := &fakeRunCreator{}
	rc := NewReceiver(db, v, fake)
	return rc, fake, token, secret
}

// TestWebhookTagPushCarriesTagRefAndCommit 验证 Tag Push Hook 命中后:
// 运行携带标签名(refs/tags/v1.2.3 → v1.2.3,落入 Branch)与 commit,并经 v* 映射到 prod。
func TestWebhookTagPushCarriesTagRefAndCommit(t *testing.T) {
	rc, fake, token, secret := newTagReleaseReceiver(t)
	res, err := rc.Handle(context.Background(), Delivery{
		Token:   token,
		Header:  giteeHeaders(eventTag, secret, "", "d-tagpush"),
		RawBody: []byte(`{"ref":"refs/tags/v1.2.3","after":"tagsha123"}`),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !res.Accepted || res.RunID == "" {
		t.Fatalf("expected accepted run for tag push, got %+v", res)
	}
	if fake.count() != 1 {
		t.Fatalf("expected 1 run, got %d", fake.count())
	}
	got := fake.calls[0]
	if got.Branch != "v1.2.3" {
		t.Fatalf("expected tag name v1.2.3 in Branch, got %q", got.Branch)
	}
	if got.Commit != "tagsha123" {
		t.Fatalf("expected commit tagsha123, got %q", got.Commit)
	}
	if got.ResolvedEnvironment != "prod" {
		t.Fatalf("expected env prod via v* mapping, got %q", got.ResolvedEnvironment)
	}
}

// TestWebhookReleaseHookCreatesRunWithTagName 验证 Release Hook 命中后:
// 运行携带 release.tag_name(落入 Branch),commit 回退到 target_commitish,经 v* 映射到 prod。
func TestWebhookReleaseHookCreatesRunWithTagName(t *testing.T) {
	rc, fake, token, secret := newTagReleaseReceiver(t)
	res, err := rc.Handle(context.Background(), Delivery{
		Token:   token,
		Header:  giteeHeaders(eventRelease, secret, "", "d-release"),
		RawBody: []byte(`{"action":"published","release":{"tag_name":"v2.0.0","target_commitish":"main"}}`),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !res.Accepted || res.RunID == "" {
		t.Fatalf("expected accepted run for release, got %+v", res)
	}
	if fake.count() != 1 {
		t.Fatalf("expected 1 run, got %d", fake.count())
	}
	got := fake.calls[0]
	if got.Branch != "v2.0.0" {
		t.Fatalf("expected release tag v2.0.0 in Branch, got %q", got.Branch)
	}
	if got.Commit != "main" {
		t.Fatalf("expected commit fallback to target_commitish 'main', got %q", got.Commit)
	}
	if got.ResolvedEnvironment != "prod" {
		t.Fatalf("expected env prod via v* mapping, got %q", got.ResolvedEnvironment)
	}
}

// TestWebhookReleaseNotSubscribed 验证未勾选 Release 时,Release Hook 投递被忽略(不创建运行)。
func TestWebhookReleaseNotSubscribed(t *testing.T) {
	// newReceiver 只开 Push(Release=false)。
	rc, fake, token, secret := newReceiver(t)
	res, err := rc.Handle(context.Background(), Delivery{
		Token:   token,
		Header:  giteeHeaders(eventRelease, secret, "", "d-rel-nosub"),
		RawBody: []byte(`{"release":{"tag_name":"v9","target_commitish":"main"}}`),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.Accepted || res.Ignored != IgnoredEventNotSubscribed {
		t.Fatalf("expected event_not_subscribed, got %+v", res)
	}
	if fake.count() != 0 {
		t.Fatalf("expected no run, got %d", fake.count())
	}
}

func TestWebhookTokenNotFound(t *testing.T) {
	rc, _, _, secret := newReceiver(t)
	_, err := rc.Handle(context.Background(), Delivery{
		Token:   "nonexistent",
		Header:  giteeHeaders(eventPush, secret, "", ""),
		RawBody: pushBody("main", "x"),
	})
	if err != ErrTokenNotFound {
		t.Fatalf("expected ErrTokenNotFound, got %v", err)
	}
}

func TestWebhookVaultUnconfiguredRejects(t *testing.T) {
	db, _ := testDB(t)
	v := vault.New(db, testMasterKey())
	svc := New(db, v)
	projID := seedProject(t, db)
	cfg, err := svc.Get(context.Background(), projID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// 用未配置 vault 的 Receiver(master key 缺失)→ 验签不可用,明确拒绝。
	rc := NewReceiver(db, vault.New(db, nil), &fakeRunCreator{})
	_, err = rc.Handle(context.Background(), Delivery{
		Token:   cfg.WebhookToken,
		Header:  giteeHeaders(eventPush, "anything", "", ""),
		RawBody: pushBody("main", "x"),
	})
	if err != ErrUnauthorized {
		t.Fatalf("expected ErrUnauthorized when vault unconfigured, got %v", err)
	}
}

// TestNormalizeTokenHeader 验证自定义 token 头名校验:
// 空/空白 = 不启用(回落内置链);合法头名原样保留;非法字符/超长 → ErrInvalidTokenHeader。
func TestNormalizeTokenHeader(t *testing.T) {
	ok := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"X-Custom-Token", "X-Custom-Token"},
		{"  x-codeup-token  ", "x-codeup-token"},
		{"X_Token.1", "X_Token.1"},
	}
	for _, c := range ok {
		got, err := normalizeTokenHeader(c.in)
		if err != nil || got != c.want {
			t.Errorf("normalizeTokenHeader(%q)=%q,%v want %q", c.in, got, err, c.want)
		}
	}
	bad := []string{"X Token", "X-Token:", "令牌头", strings.Repeat("A", maxTokenHeaderLen+1)}
	for _, in := range bad {
		if _, err := normalizeTokenHeader(in); !errors.Is(err, ErrInvalidTokenHeader) {
			t.Errorf("normalizeTokenHeader(%q) should fail, got %v", in, err)
		}
	}
}

// TestWebhookCodeupHeadersCreatesRun 验证云效 Codeup 头被等价识别:
// 事件/投递 id 走 X-Codeup-*,token 走 X-Codeup-Token(或 GitLab 兼容的 X-Gitlab-Token)。
func TestWebhookCodeupHeadersCreatesRun(t *testing.T) {
	for _, tokenHdrName := range []string{HeaderCodeupToken, HeaderGitlabToken} {
		t.Run(tokenHdrName, func(t *testing.T) {
			rc, fake, token, secret := newReceiver(t)
			res, err := rc.Handle(context.Background(), Delivery{
				Token: token,
				Header: hdrs(map[string]string{
					HeaderCodeupEvent:    eventPush,
					tokenHdrName:         secret,
					HeaderCodeupDelivery: "codeup-d-1",
				}),
				RawBody: pushBody("main", "codeup1"),
			})
			if err != nil || !res.Accepted {
				t.Fatalf("Codeup 头应被接受: res=%+v err=%v", res, err)
			}
			if fake.count() != 1 {
				t.Fatalf("expected 1 run, got %d", fake.count())
			}
			// 来源标签据头判定:Codeup 投递不应被记成 gitee。
			if got := fake.calls[0].Actor; got != actorCodeup {
				t.Fatalf("expected actor %q, got %q", actorCodeup, got)
			}
		})
	}
}

// TestWebhookGitlabEventHeaderCreatesRun 验证 GitLab 头(X-Gitlab-Event + X-Gitlab-Token)
// 也被接受(事件名与载荷同形),来源标签为 gitlab。
func TestWebhookGitlabEventHeaderCreatesRun(t *testing.T) {
	rc, fake, token, secret := newReceiver(t)
	res, err := rc.Handle(context.Background(), Delivery{
		Token: token,
		Header: hdrs(map[string]string{
			HeaderGitlabEvent: eventPush,
			HeaderGitlabToken: secret,
		}),
		RawBody: pushBody("main", "gl1"),
	})
	if err != nil || !res.Accepted {
		t.Fatalf("GitLab 头应被接受: res=%+v err=%v", res, err)
	}
	if fake.count() != 1 {
		t.Fatalf("expected 1 run, got %d", fake.count())
	}
	if got := fake.calls[0].Actor; got != actorGitlab {
		t.Fatalf("expected actor %q, got %q", actorGitlab, got)
	}
}

// TestWebhookCustomTokenHeader 验证自定义 token 头名:
//   - 配置 X-Custom-Token 后,只认该头:该头带正确密钥 → 通过;
//     密钥只出现在内置头(X-Gitee-Token)→ 401(不偷偷回退,避免误配时静默通过)。
func TestWebhookCustomTokenHeader(t *testing.T) {
	db, _ := testDB(t)
	v := vault.New(db, testMasterKey())
	svc := New(db, v)
	projID := seedProject(t, db)
	ctx := context.Background()

	cfg, err := svc.Get(ctx, projID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	reset, err := svc.ResetSecret(ctx, projID)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	secret := reset.Secret
	if _, err := svc.Save(ctx, projID, SaveInput{
		Events:          Events{Push: true},
		BranchMappings:  []BranchMapping{{BranchPattern: "main", Environment: "prod"}},
		UnmatchedPolicy: PolicyIgnore,
		TokenHeader:     "X-Custom-Token",
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	// 保存后回读:自定义头名已持久化。
	got, err := svc.Get(ctx, projID)
	if err != nil {
		t.Fatalf("get after save: %v", err)
	}
	if got.TokenHeader != "X-Custom-Token" {
		t.Fatalf("token header not persisted, got %q", got.TokenHeader)
	}

	fake := &fakeRunCreator{}
	rc := NewReceiver(db, v, fake)
	body := pushBody("main", "custom1")
	base := map[string]string{HeaderGiteeEvent: eventPush, HeaderGiteeDelivery: "custom-d-1"}

	// 只认自定义头:内置头带正确密钥不算数。
	withBuiltin := maps.Clone(base)
	withBuiltin[HeaderGiteeToken] = secret
	if _, err := rc.Handle(ctx, Delivery{Token: cfg.WebhookToken, Header: hdrs(withBuiltin), RawBody: body}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("配了自定义头时不应回退内置头, got err=%v", err)
	}
	if fake.count() != 0 {
		t.Fatalf("expected no run, got %d", fake.count())
	}

	// 自定义头带正确密钥 → 通过。
	withCustom := maps.Clone(base)
	withCustom["X-Custom-Token"] = secret
	res, err := rc.Handle(ctx, Delivery{Token: cfg.WebhookToken, Header: hdrs(withCustom), RawBody: body})
	if err != nil || !res.Accepted {
		t.Fatalf("自定义头应通过: res=%+v err=%v", res, err)
	}
	if fake.count() != 1 {
		t.Fatalf("expected 1 run, got %d", fake.count())
	}
}

// TestActorOf 验证运行来源标签判定(按头识别平台,不参与鉴权)。
func TestActorOf(t *testing.T) {
	cases := []struct {
		name      string
		headers   map[string]string
		tokenName string
		want      string
	}{
		{"gitee", map[string]string{HeaderGiteeEvent: eventPush}, HeaderGiteeToken, actorGitee},
		{"codeup-event", map[string]string{HeaderCodeupEvent: eventPush}, HeaderCodeupToken, actorCodeup},
		{"codeup-token-only", map[string]string{HeaderCodeupToken: "s"}, HeaderCodeupToken, actorCodeup},
		{"gitlab-token", map[string]string{}, HeaderGitlabToken, actorGitlab},
		{"gitlab-event", map[string]string{HeaderGitlabEvent: eventPush}, HeaderGiteeToken, actorGitlab},
		{"custom-header", map[string]string{"X-Custom-Token": "s"}, "X-Custom-Token", actorGitee},
	}
	for _, c := range cases {
		if got := actorOf(Delivery{Header: hdrs(c.headers)}, c.tokenName); got != c.want {
			t.Errorf("%s: actorOf = %q want %q", c.name, got, c.want)
		}
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"main", "main", true},
		{"main", "master", false},
		{"release/*", "release/1.2", true},
		{"release/*", "release/", true},
		{"release/*", "dev", false},
		{"feature/*-hotfix", "feature/x-hotfix", true},
		{"feature/*-hotfix", "feature/x", false},
		{"*", "anything", true},
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.s); got != c.want {
			t.Errorf("globMatch(%q,%q)=%v want %v", c.pattern, c.s, got, c.want)
		}
	}
}

// --- 事件类型判定:头归一化 + 载荷 object_kind 兜底(云效 Codeup 不带事件头) ---

func TestCanonicalEvent(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"Push Hook", eventPush},
		{"push", eventPush},         // GitLab / Codeup 系裸小写
		{"Push", eventPush},         // 大小写无关
		{"Tag Push Hook", eventTag}, //
		{"tag_push", eventTag},      // 下划线形态
		{"Merge Request Hook", eventMergeRequest},
		{"merge_request", eventMergeRequest},
		{"Release Hook", eventRelease},
		{"", ""},
		{"Note Hook", ""}, // 不认识 → 空(不猜)
		{"unknown", ""},
	}
	for _, c := range cases {
		if got := canonicalEvent(c.raw); got != c.want {
			t.Errorf("canonicalEvent(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestEventFromPayload(t *testing.T) {
	cases := []struct {
		name string
		p    parsedPayload
		want string
	}{
		{"分支 push", parsedPayload{ObjectKind: "push", Ref: "refs/heads/main"}, eventPush},
		{"推 tag 也报 push,靠 ref 区分", parsedPayload{ObjectKind: "push", Ref: "refs/tags/v1"}, eventTag},
		{"显式 tag_push", parsedPayload{ObjectKind: "tag_push", Ref: "refs/tags/v1"}, eventTag},
		{"MR", parsedPayload{ObjectKind: "merge_request"}, eventMergeRequest},
		{"release", parsedPayload{ObjectKind: "release"}, eventRelease},
		{"大写容忍", parsedPayload{ObjectKind: "Push", Ref: "refs/heads/x"}, eventPush},
		{"无 object_kind", parsedPayload{Ref: "refs/heads/main"}, ""},
		{"未知 object_kind", parsedPayload{ObjectKind: "note"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := eventFromPayload(c.p); got != c.want {
				t.Fatalf("eventFromPayload = %q, want %q", got, c.want)
			}
		})
	}
}

// codeupPushBody 是云效 Codeup 真实 push 载荷的形态:带 object_kind,**不带**事件头,
// commits[] 里也没有 added/modified/removed(Codeup 不给改动文件清单)。
func codeupPushBody(branch, after string) []byte {
	return []byte(`{"object_kind":"push","before":"882688d","after":"` + after + `",` +
		`"checkout_sha":"` + after + `","ref":"refs/heads/` + branch + `","total_commits_count":1,` +
		`"commits":[{"id":"` + after + `","message":"msg","timestamp":"2026-09-26T16:25:38.000+08:00",` +
		`"url":"https://codeup.aliyun.com/x/y/commit/` + after + `","author":{"name":"liangxy","email":"a@b.c"}}],` +
		`"repository":{"name":"financial-v5","git_http_url":"https://codeup.aliyun.com/fxy/fenxi365/financial-v5.git"}}`)
}

// 云效 Codeup 投递不带事件头:事件类型必须由 body 的 object_kind 判定,否则会被当成
// 「未订阅」而永不触发(这是「配了 webhook 却一直不触发」的原因)。
func TestCodeupPushWithoutEventHeaderCreatesRun(t *testing.T) {
	rc, fake, token, secret := newReceiver(t)
	res, err := rc.Handle(context.Background(), Delivery{
		Token: token,
		// 只带 token 头,事件头/投递 id 头一律没有。
		Header:  hdrs(map[string]string{HeaderCodeupToken: secret}),
		RawBody: codeupPushBody("main", "6022b02"),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !res.Accepted || res.RunID == "" {
		t.Fatalf("object_kind=push 且分支命中时应创建运行, got %+v", res)
	}
	if fake.count() != 1 {
		t.Fatalf("expected 1 run, got %d", fake.count())
	}
	if got := fake.calls[0]; got.Branch != "main" || got.Commit != "6022b02" {
		t.Fatalf("运行入参不符: %+v", got)
	}
}

// 同一形态的载荷、分支不命中时,应以**分支不匹配**忽略 —— 证明事件闸门已经放行,
// 拦下它的不再是「事件未订阅」。
func TestCodeupPushWithoutEventHeaderEventGatePasses(t *testing.T) {
	rc, fake, token, secret := newReceiver(t)
	res, err := rc.Handle(context.Background(), Delivery{
		Token:   token,
		Header:  hdrs(map[string]string{HeaderCodeupToken: secret}),
		RawBody: codeupPushBody("master", "6022b02"),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.Accepted {
		t.Fatalf("分支不在映射里不应创建运行, got %+v", res)
	}
	if res.Ignored != IgnoredUnmatchedIgnored {
		t.Fatalf("应因分支不匹配被忽略(而非事件未订阅), got %q", res.Ignored)
	}
	if fake.count() != 0 {
		t.Fatalf("不应创建运行, got %d", fake.count())
	}
}

// TestWebhookPushCarriesCommitMessage 验证 push 事件把触发提交的说明信息(commits[] 末条首行)
// 解析进 RunRequest,供通知引用展示。
func TestWebhookPushCarriesCommitMessage(t *testing.T) {
	rc, fake, token, secret := newReceiver(t)
	body := []byte(`{"ref":"refs/heads/main","after":"abc123","commits":[` +
		`{"id":"1","message":"first commit\n\nbody 多行说明"},{"id":"2","message":"chore(agent): 端点生成器改为显式任务并支持仓库级排除清单\n\n更多说明"}` +
		`]}`)
	res, err := rc.Handle(context.Background(), Delivery{
		Token:   token,
		Header:  giteeHeaders(eventPush, secret, "", "d-cm"),
		RawBody: body,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !res.Accepted {
		t.Fatalf("expected accepted, got %+v", res)
	}
	if fake.count() != 1 {
		t.Fatalf("expected 1 run, got %d", fake.count())
	}
	if got := fake.calls[0].CommitMessage; got != "chore(agent): 端点生成器改为显式任务并支持仓库级排除清单" {
		t.Fatalf("CommitMessage = %q", got)
	}
}

// TestWebhookPushCommitMessageHeadCommitFallback 验证 commits[] 缺失时回退 head_commit.message
// (GitHub push 载荷常见形状)。
func TestWebhookPushCommitMessageHeadCommitFallback(t *testing.T) {
	rc, fake, token, secret := newReceiver(t)
	body := []byte(`{"ref":"refs/heads/main","after":"abc123","head_commit":{"id":"abc123","message":"fix: 修复部署健康检查"}}`)
	res, err := rc.Handle(context.Background(), Delivery{
		Token:   token,
		Header:  giteeHeaders(eventPush, secret, "", "d-cm2"),
		RawBody: body,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !res.Accepted || fake.count() != 1 {
		t.Fatalf("expected accepted run, got %+v / %d runs", res, fake.count())
	}
	if got := fake.calls[0].CommitMessage; got != "fix: 修复部署健康检查" {
		t.Fatalf("CommitMessage = %q", got)
	}
}
