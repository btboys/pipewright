package notify

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// ── 企业微信群机器人 ──────────────────────────────────────────────────────────

// 企微成功:返回 {errcode:0} → Test 成功;POST 体须是 {msgtype:markdown,markdown:{content}}。
func TestWecomSendSuccess(t *testing.T) {
	var (
		mu      sync.Mutex
		gotBody []byte
		gotCT   string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = body
		gotCT = r.Header.Get("Content-Type")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer srv.Close()

	s, _, _ := newSvc(t, srv.Client())
	ch, err := s.Create(ctx(), CreateInput{
		Name: "ops-wecom", Type: TypeWecom, Enabled: true,
		Config: Config{URL: srv.URL + "/cgi-bin/webhook/send?key=abc"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	res, err := s.Test(ctx(), ch.ID)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if !res.OK {
		t.Fatalf("企微 test 应成功: %+v", res)
	}

	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(gotCT, "application/json") {
		t.Fatalf("Content-Type 应为 json: %q", gotCT)
	}
	var sent wecomMarkdownBody
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("POST 体应为合法 JSON: %s", gotBody)
	}
	if sent.MsgType != "markdown" {
		t.Fatalf("msgtype 应为 markdown,得 %q", sent.MsgType)
	}
	if !strings.Contains(sent.Markdown.Content, "# Pipewright 测试通知") {
		t.Fatalf("markdown content 应含标题: %q", sent.Markdown.Content)
	}
	if !strings.Contains(sent.Markdown.Content, "测试通知") {
		t.Fatalf("markdown content 应含正文: %q", sent.Markdown.Content)
	}
	// 测试载荷字段(source/kind)应渲染入正文。
	if !strings.Contains(sent.Markdown.Content, "Pipewright") {
		t.Fatalf("markdown content 应含字段值: %q", sent.Markdown.Content)
	}
}

// 关键回归守卫:企微参数错误也回 HTTP 200,但 body 内 errcode!=0 → 必须判失败。
func TestWecomRejectedHTTP200ButErrcodeNonZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK) // HTTP 200
		_, _ = w.Write([]byte(`{"errcode":93000,"errmsg":"invalid webhook url"}`))
	}))
	defer srv.Close()

	s, _, _ := newSvc(t, srv.Client())
	ch, _ := s.Create(ctx(), CreateInput{
		Name: "bad", Type: TypeWecom, Enabled: true,
		Config: Config{URL: srv.URL + "/hook"},
	})
	res, err := s.Test(ctx(), ch.ID)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if res.OK {
		t.Fatalf("企微 errcode!=0 应判失败(不可因 HTTP 200 误报成功)")
	}
	if !strings.Contains(res.Error, "93000") || !strings.Contains(res.Error, "invalid webhook url") {
		t.Fatalf("错误应人读含 errcode 与 errmsg: %q", res.Error)
	}
}

// 企微 webhook 地址必填。
func TestWecomRequiresURL(t *testing.T) {
	s, _, _ := newSvc(t, http.DefaultClient)
	if _, err := s.Create(ctx(), CreateInput{Name: "x", Type: TypeWecom, Enabled: true}); err != ErrInvalidConfig {
		t.Fatalf("企微缺 URL 应 ErrInvalidConfig,得: %v", err)
	}
}

// 企微地址同样过 SSRF 收口(拒云元数据)。
func TestWecomSSRFBlocked(t *testing.T) {
	s, _, _ := newSvc(t, http.DefaultClient)
	_, err := s.Create(ctx(), CreateInput{
		Name: "evil", Type: TypeWecom, Enabled: true,
		Config: Config{URL: "http://169.254.169.254/hook"},
	})
	if err != ErrSSRFBlocked {
		t.Fatalf("企微 169.254 应被 SSRF 拒绝,得: %v", err)
	}
}

// ── 钉钉自定义群机器人 ────────────────────────────────────────────────────────

// 钉钉成功(无加签):返回 {errcode:0} → Test 成功;POST 体须是 {msgtype:markdown,markdown:{title,text}};
// 未配密钥不应带 timestamp/sign query。
func TestDingtalkSendSuccessNoSign(t *testing.T) {
	var (
		mu       sync.Mutex
		gotBody  []byte
		gotQuery string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = body
		gotQuery = r.URL.RawQuery
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer srv.Close()

	s, _, _ := newSvc(t, srv.Client())
	ch, err := s.Create(ctx(), CreateInput{
		Name: "ops-ding", Type: TypeDingtalk, Enabled: true,
		Config: Config{URL: srv.URL + "/robot/send?access_token=abc"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	res, err := s.Test(ctx(), ch.ID)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if !res.OK {
		t.Fatalf("钉钉 test 应成功: %+v", res)
	}

	mu.Lock()
	defer mu.Unlock()
	var sent dingtalkMarkdownBody
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("POST 体应为合法 JSON: %s", gotBody)
	}
	if sent.MsgType != "markdown" {
		t.Fatalf("msgtype 应为 markdown,得 %q", sent.MsgType)
	}
	if sent.Markdown.Title != "Pipewright 测试通知" {
		t.Fatalf("钉钉 markdown.title 应为测试标题,得 %q", sent.Markdown.Title)
	}
	if !strings.Contains(sent.Markdown.Text, "# Pipewright 测试通知") {
		t.Fatalf("钉钉 markdown.text 应含标题: %q", sent.Markdown.Text)
	}
	// 未配密钥:不应携带 timestamp/sign(原 query access_token 保留)。
	if strings.Contains(gotQuery, "timestamp=") || strings.Contains(gotQuery, "sign=") {
		t.Fatalf("未配密钥不应带 timestamp/sign,query=%q", gotQuery)
	}
	if !strings.Contains(gotQuery, "access_token=abc") {
		t.Fatalf("原有 query 应保留,query=%q", gotQuery)
	}
}

// 关键回归守卫:钉钉参数/签名错误也回 HTTP 200,但 body 内 errcode!=0 → 必须判失败。
func TestDingtalkRejectedHTTP200ButErrcodeNonZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK) // HTTP 200
		_, _ = w.Write([]byte(`{"errcode":310000,"errmsg":"sign not match"}`))
	}))
	defer srv.Close()

	s, _, _ := newSvc(t, srv.Client())
	ch, _ := s.Create(ctx(), CreateInput{
		Name: "bad", Type: TypeDingtalk, Enabled: true,
		Config: Config{URL: srv.URL + "/hook"},
	})
	res, err := s.Test(ctx(), ch.ID)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if res.OK {
		t.Fatalf("钉钉 errcode!=0 应判失败(不可因 HTTP 200 误报成功)")
	}
	if !strings.Contains(res.Error, "310000") || !strings.Contains(res.Error, "sign not match") {
		t.Fatalf("错误应人读含 errcode 与 errmsg: %q", res.Error)
	}
}

// 配了加签密钥 → URL query 带 timestamp + sign,且 sign 与钉钉规范算法一致。
func TestDingtalkSignsWhenSecretConfigured(t *testing.T) {
	var (
		mu        sync.Mutex
		gotTS     string
		gotSign   string
		gotAccess string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotTS = r.URL.Query().Get("timestamp")
		gotSign = r.URL.Query().Get("sign")
		gotAccess = r.URL.Query().Get("access_token")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer srv.Close()

	const secret = "SECabc123signsecret"
	s, _, _ := newSvc(t, srv.Client())
	ch, err := s.Create(ctx(), CreateInput{
		Name: "signed-ding", Type: TypeDingtalk, Enabled: true,
		Config: Config{URL: srv.URL + "/robot/send?access_token=tok"}, Secret: secret,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	res, err := s.Test(ctx(), ch.ID)
	if err != nil || !res.OK {
		t.Fatalf("带加签应成功: %+v err=%v", res, err)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotTS == "" || gotSign == "" {
		t.Fatalf("配密钥应带 timestamp+sign query: ts=%q sign=%q", gotTS, gotSign)
	}
	if gotAccess != "tok" {
		t.Fatalf("原有 access_token query 应保留,得 %q", gotAccess)
	}
	// 服务端从 URL 解码得到的 sign 必须等于按相同 timestamp + secret 重算的规范签名。
	want := dingtalkSign(gotTS, secret)
	if gotSign != want {
		t.Fatalf("sign 与钉钉规范算法不一致: got %q want %q", gotSign, want)
	}
}

// 钉钉缺密钥不影响保存(密钥可选);加签密钥 write-only —— HasSecret 反映存在但不回明文。
func TestDingtalkSecretWriteOnly(t *testing.T) {
	s, _, _ := newSvc(t, http.DefaultClient)
	ch, err := s.Create(ctx(), CreateInput{
		Name: "ding", Type: TypeDingtalk, Enabled: true,
		Config: Config{URL: "https://oapi.dingtalk.com/robot/send?access_token=tok"},
		Secret: "topsecret",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !ch.HasSecret {
		t.Fatalf("配了密钥 HasSecret 应为 true")
	}
	got, err := s.Get(ctx(), ch.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.HasSecret {
		t.Fatalf("Get 视图 HasSecret 应为 true")
	}
	// 视图结构里绝无密钥明文字段(Config 不含 secret;仅 HasSecret 布尔)。
	blob, _ := json.Marshal(got)
	if strings.Contains(string(blob), "topsecret") {
		t.Fatalf("渠道视图绝不应含密钥明文: %s", blob)
	}
}

// 钉钉规范签名:key=secret、message="{ts}\n{secret}"(与飞书的 key=stringToSign 相反)。
func TestDingtalkSignAlgorithm(t *testing.T) {
	// 固定输入断言确定性输出(防算法被误改回飞书式)。
	got := dingtalkSign("1600000000000", "thesecret")
	// 与飞书签名(key/message 互换)对比应不同 —— 守卫不可混淆两套算法。
	feishu, _ := feishuSign("1600000000000", "thesecret")
	if got == feishu {
		t.Fatalf("钉钉签名不应等于飞书签名(key/message 互换): %q", got)
	}
	if got == "" {
		t.Fatalf("签名不应为空")
	}
}

// 通用 markdown 渲染:标题一级标题 + 正文 + 字段按业务顺序逐行(中文标签)。
func TestRenderMarkdownBody(t *testing.T) {
	md := renderMarkdownBody(Payload{
		Title:  "部署成功",
		Body:   "aireboot @ staging",
		Fields: map[string]string{"status": "success", "branch": "main", "event": "deploy_succeeded"},
		Lang:   "zh-CN",
	}, flavorWecom)
	if !strings.HasPrefix(md, "# 部署成功") {
		t.Fatalf("应以一级标题起: %q", md)
	}
	if !strings.Contains(md, "aireboot @ staging") {
		t.Fatalf("应含正文: %q", md)
	}
	for _, want := range []string{"**分支**:main", "**状态**:success", "**事件**:deploy_succeeded"} {
		if !strings.Contains(md, want) {
			t.Fatalf("应含字段行 %q,得:\n%s", want, md)
		}
	}
	// 业务顺序:分支在状态前、状态在事件前。
	if strings.Index(md, "**分支**") > strings.Index(md, "**状态**") {
		t.Fatalf("业务顺序:分支应在状态前:\n%s", md)
	}
	if strings.Index(md, "**状态**") > strings.Index(md, "**事件**") {
		t.Fatalf("业务顺序:状态应在事件前:\n%s", md)
	}
	// 空载荷:仍产默认标题,不 panic。
	if !strings.HasPrefix(renderMarkdownBody(Payload{}, flavorWecom), "# Pipewright 通知") {
		t.Fatalf("空载荷应产默认标题")
	}
}

// 流水线卡片(运行终态事件):企微 flavor 输出彩色标题 + 流水线名链接 + 卡片字段行 + 提交说明引用行;
// 空值行省略。对齐 Flow 流水线消息通知格式。
func TestRenderPipelineCardWecom(t *testing.T) {
	md := renderPipelineCardForTest(t, flavorWecom)
	for _, want := range []string{
		`# <font color="info">Pipewright 流水线消息通知</font>`,
		"**流水线**:[财务V5自动构建_saas](https://pw.example.com/runs/r1)",
		"**流水线环境**:正式环境",
		"**触发信息**:流水线定时自动触发",
		"**流水线阶段**:部署",
		"**流水线任务**:纷析云部署",
		"**运行状态**:✅构建成功",
		"> chore(agent): 端点生成器改为显式任务并支持仓库级排除清单",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("企微卡片应含 %q,得:\n%s", want, md)
		}
	}
	// 空值行省略:执行人/actor 为空不应出现该行。
	if strings.Contains(md, "**执行人**") {
		t.Fatalf("空值行应省略(执行人为空):\n%s", md)
	}
}

// 钉钉 flavor:标题不带企微专有彩色标签(钉钉不支持 <font color>),其余结构一致。
func TestRenderPipelineCardDingtalk(t *testing.T) {
	md := renderPipelineCardForTest(t, flavorDingtalk)
	if !strings.HasPrefix(md, "# Pipewright 流水线消息通知") {
		t.Fatalf("钉钉卡片标题应为纯文本一级标题: %q", md)
	}
	if strings.Contains(md, "<font color=") {
		t.Fatalf("钉钉不应输出企微彩色标签:\n%s", md)
	}
	if !strings.Contains(md, "**运行状态**:✅构建成功") {
		t.Fatalf("钉钉卡片应含运行状态行:\n%s", md)
	}
}

// renderPipelineCardForTest 构造一张终态成功卡片载荷(带 fieldPipeline 键)并渲染。
func renderPipelineCardForTest(t *testing.T, flavor markdownFlavor) string {
	t.Helper()
	p := Payload{
		Lang: "zh-CN",
		Fields: map[string]string{
			"event":         "build_succeeded",
			"pipeline":      "财务V5自动构建_saas",
			"pipelineUrl":   "https://pw.example.com/runs/r1",
			"environment":   "正式环境",
			"triggerType":   "schedule",
			"stage":         "部署",
			"task":          "纷析云部署",
			"commitMessage": "chore(agent): 端点生成器改为显式任务并支持仓库级排除清单",
			// actor 故意缺省:空值行应被省略。
		},
	}
	return renderMarkdownBody(p, flavor)
}

// 触发信息文案映射:schedule/webhook/manual/chain → 本地化人读文案;未知值原样透出。
func TestTriggerInfoLabel(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"schedule", "流水线定时自动触发"},
		{"webhook", "代码推送触发"},
		{"manual", "手动触发"},
		{"chain", "上游运行串联触发"},
		{"mystery", "mystery"},
		{"", ""},
	}
	for _, c := range cases {
		if got := triggerInfoLabel(c.in, "zh-CN"); got != c.want {
			t.Fatalf("triggerInfoLabel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := triggerInfoLabel("schedule", "en"); got != "Scheduled pipeline trigger" {
		t.Fatalf("en schedule = %q", got)
	}
}

// 无 pipeline 键的载荷(自定义模板/审批/异常/测试通知)保持旧版字段列表渲染,行为不变。
func TestRenderMarkdownBodyLegacyUnchanged(t *testing.T) {
	md := renderMarkdownBody(Payload{
		Title: "需要审批",
		Fields: map[string]string{
			"event": "approval_required", "project": "demo", "status": "waiting_approval",
		},
		Lang: "zh-CN",
	}, flavorWecom)
	if strings.Contains(md, "流水线消息通知") {
		t.Fatalf("无 pipeline 键不应走卡片渲染:\n%s", md)
	}
	if !strings.HasPrefix(md, "# 需要审批") {
		t.Fatalf("应保持旧版标题渲染:\n%s", md)
	}
}
