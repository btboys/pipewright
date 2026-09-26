package proxy

import (
	"context"
	"strings"
	"testing"

	"github.com/btboys/pipewright/internal/storetest"
	"github.com/btboys/pipewright/internal/target"
)

// --- 渲染:acme.sh 模式的显式 tls ------------------------------------------

func TestRenderAcmeShExplicitTLS(t *testing.T) {
	out := renderCaddyfile([]Route{{
		Domain: "app.example.com", UpstreamContainer: "web", UpstreamPort: 8080,
		TLSMode: TLSModeAcmeSh,
	}}, nil, map[string]bool{"app.example.com": true})

	want := "    tls /etc/pipewright/acme/app.example.com/fullchain.pem /etc/pipewright/acme/app.example.com/privkey.pem\n"
	if !strings.Contains(out, want) {
		t.Fatalf("acme.sh 路由应渲染显式 tls 行:\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
	if strings.Contains(out, "dns ") {
		t.Fatalf("acme.sh 路由不应渲染 Caddy 的 DNS-01 tls 块(证书已由 acme.sh 签好):\n%s", out)
	}
	if !strings.Contains(out, "reverse_proxy web:8080\n") {
		t.Fatalf("站点块本体仍应渲染:\n%s", out)
	}
}

// 证书文件尚未落到主机上时不渲染 tls:Caddy 在配置加载期即读 tls 引用的文件,引用不存在的文件会让
// reload 直接失败(不像自动 ACME 那样容忍异步失败)。此时退回自动 ACME 语义,待签发成功后再补 tls。
func TestRenderAcmeShSkipsTLSWhenCertNotReady(t *testing.T) {
	out := renderCaddyfile([]Route{{
		Domain: "app.example.com", UpstreamContainer: "web", UpstreamPort: 8080,
		TLSMode: TLSModeAcmeSh,
	}}, nil, nil)

	if strings.Contains(out, "tls ") {
		t.Fatalf("证书未就位时不应渲染 tls 行:\n%s", out)
	}
	if !strings.Contains(out, "reverse_proxy web:8080\n") {
		t.Fatalf("站点块本体仍应渲染:\n%s", out)
	}
}

// 通配符域名含 `*`,不能直接当目录名;目录名必须文件系统安全,且判就绪时用同一套映射。
func TestRenderAcmeShWildcardDirName(t *testing.T) {
	out := renderCaddyfile([]Route{{
		Domain: "*.example.com", UpstreamContainer: "web", UpstreamPort: 8080,
		TLSMode: TLSModeAcmeSh,
	}}, nil, map[string]bool{"_.example.com": true})

	if !strings.Contains(out, "/etc/pipewright/acme/_.example.com/fullchain.pem") {
		t.Fatalf("通配符域名应映射为文件系统安全目录名:\n%s", out)
	}
}

// auto 模式即使给了 acmeReady 也不受影响(两模式互斥)。
func TestRenderAutoModeIgnoresAcmeReady(t *testing.T) {
	out := renderCaddyfile([]Route{{
		Domain: "app.example.com", UpstreamContainer: "web", UpstreamPort: 8080,
	}}, nil, map[string]bool{"app.example.com": true})

	if strings.Contains(out, "tls ") {
		t.Fatalf("auto 模式不应渲染显式 tls 行:\n%s", out)
	}
}

// --- DNS 提供商 → acme.sh 插件/环境变量 --------------------------------------

func TestAcmeDNSPluginMapping(t *testing.T) {
	cases := []struct {
		name       string
		provider   string
		token      string
		wantPlugin string
		wantEnv    string
		wantErr    bool
	}{
		{"cloudflare 单 token", "cloudflare", "cf-token", "dns_cf", "CF_Token=cf-token", false},
		{"dnspod 两段凭据", "dnspod", "id1,key1", "dns_dp", "DP_Id=id1,DP_Key=key1", false},
		{"tencentcloud 是 dnspod 别名", "tencentcloud", " sid , skey ", "dns_dp", "DP_Id=sid,DP_Key=skey", false},
		{"alidns 两段凭据", "alidns", "ak,sk", "dns_ali", "Ali_Key=ak,Ali_Secret=sk", false},
		{"cloudflare 空 token", "cloudflare", "", "", "", true},
		{"dnspod 缺 secret", "dnspod", "only-id", "", "", true},
		{"未知提供商", "route53", "x", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plugin, env, err := acmeDNSPlugin(c.provider, c.token)
			if c.wantErr {
				if err == nil {
					t.Fatalf("%s/%q 应报错, got plugin=%q env=%v", c.provider, c.token, plugin, env)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if plugin != c.wantPlugin {
				t.Fatalf("插件名不符: got %q want %q", plugin, c.wantPlugin)
			}
			if got := strings.Join(env, ","); got != c.wantEnv {
				t.Fatalf("环境变量不符: got %q want %q", got, c.wantEnv)
			}
		})
	}
}

// --- 签发命令序列 -----------------------------------------------------------

func TestIssueAcmeCertCommandSequence(t *testing.T) {
	ctx := context.Background()
	ft := &fakeTarget{}

	if err := issueAcmeCert(ctx, ft, "srv-1", "app.example.com", []string{"www.example.com"}, "cloudflare", "cf-token"); err != nil {
		t.Fatalf("issueAcmeCert: %v", err)
	}
	if len(ft.execCalls) != 3 {
		t.Fatalf("应恰好 3 次远端调用(mkdir + --issue + --install-cert), got %d:\n%s", len(ft.execCalls), joinCalls(ft.execCalls))
	}

	if got := cmdJoin(ft.execCalls[0]); got != "mkdir -p /etc/pipewright/acme/app.example.com" {
		t.Fatalf("第 1 步应是建证书目录, got %q", got)
	}

	issue := cmdJoin(ft.execCalls[1])
	for _, want := range []string{
		"env CF_Token=cf-token acme.sh --issue",
		"--dns dns_cf",
		"--keylength ec-256",
		"--server letsencrypt",
		"-d app.example.com",
		"-d www.example.com", // 别名一并签进同一张证书
	} {
		if !strings.Contains(issue, want) {
			t.Fatalf("--issue 命令缺 %q:\n%s", want, issue)
		}
	}
	if strings.Contains(issue, "--force") {
		t.Fatalf("不应 --force 重签(会触发 Let's Encrypt 速率限制):\n%s", issue)
	}

	install := cmdJoin(ft.execCalls[2])
	for _, want := range []string{
		"acme.sh --install-cert -d app.example.com --ecc",
		"--key-file /etc/pipewright/acme/app.example.com/privkey.pem",
		"--fullchain-file /etc/pipewright/acme/app.example.com/fullchain.pem",
		"--reloadcmd docker exec pipewright-caddy caddy reload",
	} {
		if !strings.Contains(install, want) {
			t.Fatalf("--install-cert 命令缺 %q:\n%s", want, install)
		}
	}
}

// --- 创建/更新校验:模式白名单 + acme.sh 必须绑 DNS --------------------------

func TestCreateRejectsUnknownTLSMode(t *testing.T) {
	ctx := context.Background()
	ft := &fakeTarget{}
	svc := &service{store: NewStore(storetest.OpenDB(t)), tg: ft, prober: fakeProber{}}

	_, err := svc.Create(ctx, CreateInput{
		ServerID: "srv-1", Domain: "app.example.com", UpstreamContainer: "web", UpstreamPort: 8080,
		TLSMode: "self-signed",
	})
	if err != ErrInvalidTLSMode {
		t.Fatalf("未知证书模式应报 ErrInvalidTLSMode, got %v", err)
	}
	if len(ft.execCalls) != 0 {
		t.Fatalf("非法早拒不应触网, got %d exec calls", len(ft.execCalls))
	}
}

func TestCreateAcmeShRequiresDNSProvider(t *testing.T) {
	ctx := context.Background()
	ft := &fakeTarget{}
	svc := &service{store: NewStore(storetest.OpenDB(t)), tg: ft, prober: fakeProber{}}

	_, err := svc.Create(ctx, CreateInput{
		ServerID: "srv-1", Domain: "app.example.com", UpstreamContainer: "web", UpstreamPort: 8080,
		TLSMode: TLSModeAcmeSh,
	})
	if err != ErrAcmeNeedsDNS {
		t.Fatalf("acme.sh 模式无 DNS 提供商应报 ErrAcmeNeedsDNS, got %v", err)
	}
	if len(ft.execCalls) != 0 {
		t.Fatalf("非法早拒不应触网, got %d exec calls", len(ft.execCalls))
	}
}

// --- 端到端:Create(acme.sh)→ 主机上签发 → Caddyfile 补上显式 tls -----------

// acmeShFakeTarget 模拟一整台主机:反代容器不存在(触发起容器路径),证书就位探测恒为「未就位」
// (由 ensureAcmeCerts 在签发成功后自行标为就位)。
func acmeShFakeTarget() *fakeTarget {
	return &fakeTarget{
		resultFor: func(cmd []string) *target.ExecResult {
			if len(cmd) >= 2 && cmd[0] == "docker" && cmd[1] == "inspect" {
				return &target.ExecResult{ExitCode: 1, Stderr: "No such object"}
			}
			if len(cmd) >= 3 && cmd[0] == "env" && cmd[2] == "sh" {
				return &target.ExecResult{ExitCode: 0} // 尚无 fullchain
			}
			return nil
		},
	}
}

func TestCreateAcmeShSignsCertAndRendersTLS(t *testing.T) {
	ctx := context.Background()
	st := storetest.OpenDB(t)
	ft := acmeShFakeTarget()
	dns := stubDNSResolver{
		creds: map[string]dnsCred{"prov-1": {Type: "cloudflare", Token: "TKN"}},
		types: map[string]string{"prov-1": "cloudflare"},
	}
	svc := &service{store: NewStore(st), tg: ft, prober: fakeProber{}, dns: dns}

	route, err := svc.Create(ctx, CreateInput{
		ServerID: "srv-1", Domain: "app.example.com", UpstreamContainer: "web", UpstreamPort: 8080,
		DNSProviderID: "prov-1", TLSMode: TLSModeAcmeSh,
	})
	if err != nil {
		t.Fatalf("Create(acme.sh): %v", err)
	}
	if route.TLSMode != TLSModeAcmeSh {
		t.Fatalf("应持久化 acme.sh 模式, got %q", route.TLSMode)
	}

	// 主机上确实跑了 acme.sh 签发。
	if !strings.Contains(joinCalls(ft.execCalls), "acme.sh --issue --dns dns_cf") {
		t.Fatalf("应在目标主机上经 acme.sh 签发:\n%s", joinCalls(ft.execCalls))
	}

	// 下发的 Caddyfile 含显式 tls(证书刚签发成功 → 标记为就位 → 补上)。
	content := ft.uploadBytes[caddyfileTmpPath]
	if !strings.Contains(content, "    tls /etc/pipewright/acme/app.example.com/fullchain.pem") {
		t.Fatalf("acme.sh 路由的 Caddyfile 应含显式 tls:\n%s", content)
	}
	// acme.sh 模式不把 DNS token 交给 Caddy(证书已由 acme.sh 签好),token 不该出现在下发的配置里。
	if strings.Contains(content, "TKN") {
		t.Fatalf("acme.sh 模式不应把 DNS token 渲染进 Caddyfile:\n%s", content)
	}
}

// --- IssueCert 语义 --------------------------------------------------------

func TestIssueCertRejectsAutoMode(t *testing.T) {
	ctx := context.Background()
	st := storetest.OpenDB(t)
	s := NewStore(st)
	r := newRoute(CreateInput{ServerID: "srv-1", Domain: "app.example.com", UpstreamContainer: "web", UpstreamPort: 8080})
	if err := s.insert(ctx, r); err != nil {
		t.Fatalf("insert: %v", err)
	}
	svc := &service{store: s, tg: &fakeTarget{}, prober: fakeProber{}}

	if _, err := svc.IssueCert(ctx, r.ID); err != ErrInvalidTLSMode {
		t.Fatalf("auto 模式的证书由 Caddy 自理,应报 ErrInvalidTLSMode, got %v", err)
	}
}

// acme.sh 执行失败(如主机未装 acme.sh)是**状态**而非 API 错误:落 cert_status=failed + 人话原因。
func TestIssueCertFailureRecordsHumanReason(t *testing.T) {
	ctx := context.Background()
	st := storetest.OpenDB(t)
	s := NewStore(st)
	r := newRoute(CreateInput{
		ServerID: "srv-1", Domain: "app.example.com", UpstreamContainer: "web", UpstreamPort: 8080,
		DNSProviderID: "prov-1", TLSMode: TLSModeAcmeSh,
	})
	if err := s.insert(ctx, r); err != nil {
		t.Fatalf("insert: %v", err)
	}
	ft := &fakeTarget{
		resultFor: func(cmd []string) *target.ExecResult {
			// acme.sh 不在 PATH(docker run 里的 shell 语义)→ exit 127。
			if len(cmd) >= 3 && cmd[0] == "env" && cmd[2] == acmeShBin {
				return &target.ExecResult{ExitCode: 127}
			}
			return nil
		},
	}
	dns := stubDNSResolver{
		creds: map[string]dnsCred{"prov-1": {Type: "cloudflare", Token: "TKN"}},
		types: map[string]string{"prov-1": "cloudflare"},
	}
	svc := &service{store: s, tg: ft, prober: fakeProber{}, dns: dns}

	got, err := svc.IssueCert(ctx, r.ID)
	if err != nil {
		t.Fatalf("acme.sh 执行失败不应变成 API 错误: %v", err)
	}
	if got.CertStatus != CertStatusFailed {
		t.Fatalf("应回写 failed, got %q", got.CertStatus)
	}
	if !strings.Contains(got.CertDetail, "acme.sh") {
		t.Fatalf("certDetail 应给人话原因(提示安装 acme.sh), got %q", got.CertDetail)
	}
}

// --- 就位探测 --------------------------------------------------------------

func TestAcmeCertsReadyParsesHostOutput(t *testing.T) {
	ctx := context.Background()
	ft := &fakeTarget{
		resultFor: func(cmd []string) *target.ExecResult {
			return &target.ExecResult{ExitCode: 0, Stdout: "a.example.com\n_.example.com\n"}
		},
	}
	ready := acmeCertsReady(ctx, ft, "srv-1", []string{"a.example.com", "b.example.com", "_.example.com"})
	if !ready["a.example.com"] || !ready["_.example.com"] {
		t.Fatalf("应报告 a/通配符 两组就位, got %v", ready)
	}
	if ready["b.example.com"] {
		t.Fatalf("未在输出里的目录不应判为就位, got %v", ready)
	}
}

// 探测本身失败(SSH 不可达)→ 全部判为未就位(保守,不让 Caddy 引用可能不存在的文件)。
func TestAcmeCertsReadyFalseOnProbeFailure(t *testing.T) {
	ctx := context.Background()
	ft := &fakeTarget{
		resultFor: func(cmd []string) *target.ExecResult {
			return &target.ExecResult{ExitCode: 1, Stderr: "boom"}
		},
	}
	if ready := acmeCertsReady(ctx, ft, "srv-1", []string{"a.example.com"}); len(ready) != 0 {
		t.Fatalf("探测失败应判未就位, got %v", ready)
	}
}

func joinCalls(calls [][]string) string {
	joined := make([]string, len(calls))
	for i, c := range calls {
		joined[i] = cmdJoin(c)
	}
	return strings.Join(joined, "\n")
}

// Update 切换证书模式必须真落库(证书总览的签发入口与后续渲染都读这个值),
// 且 acme.sh 模式不能在没有 DNS 提供商时留存。
func TestUpdatePersistsTLSMode(t *testing.T) {
	ctx := context.Background()
	st := storetest.OpenDB(t)
	s := NewStore(st)
	r := newRoute(CreateInput{ServerID: "srv-1", Domain: "app.example.com", UpstreamContainer: "web", UpstreamPort: 8080})
	if err := s.insert(ctx, r); err != nil {
		t.Fatalf("insert: %v", err)
	}
	dns := stubDNSResolver{
		creds: map[string]dnsCred{"prov-1": {Type: "cloudflare", Token: "TKN"}},
		types: map[string]string{"prov-1": "cloudflare"},
	}
	svc := &service{store: s, tg: &fakeTarget{}, prober: fakeProber{}, dns: dns}

	got, err := svc.Update(ctx, r.ID, UpdateInput{TLSMode: TLSModeAcmeSh, Config: RouteConfig{DNSProviderID: "prov-1"}})
	if err != nil {
		t.Fatalf("切到 acme.sh: %v", err)
	}
	if got.TLSMode != TLSModeAcmeSh {
		t.Fatalf("tls_mode 应落库为 acme.sh, got %q", got.TLSMode)
	}

	// 空 tlsMode = 保持不变。
	got, err = svc.Update(ctx, r.ID, UpdateInput{Config: RouteConfig{DNSProviderID: "prov-1"}})
	if err != nil {
		t.Fatalf("空 tlsMode 应保持不变: %v", err)
	}
	if got.TLSMode != TLSModeAcmeSh {
		t.Fatalf("空 tlsMode 不应改模式, got %q", got.TLSMode)
	}

	// 切回 auto。
	got, err = svc.Update(ctx, r.ID, UpdateInput{TLSMode: TLSModeAuto})
	if err != nil {
		t.Fatalf("切回 auto: %v", err)
	}
	if got.TLSMode != TLSModeAuto {
		t.Fatalf("应落库为 auto, got %q", got.TLSMode)
	}

	// acme.sh 模式摘掉 DNS 提供商 → 拒。
	if _, err := svc.Update(ctx, r.ID, UpdateInput{TLSMode: TLSModeAcmeSh}); err != ErrAcmeNeedsDNS {
		t.Fatalf("acme.sh 无 DNS 提供商应报 ErrAcmeNeedsDNS, got %v", err)
	}
	// 非法模式 → 拒。
	if _, err := svc.Update(ctx, r.ID, UpdateInput{TLSMode: "nope"}); err != ErrInvalidTLSMode {
		t.Fatalf("非法模式应报 ErrInvalidTLSMode, got %v", err)
	}
}

// --- 挂载门控:纯 auto 主机不该因这次改动被迫重建 Caddy ----------------------

// existingCaddyTarget 模拟「主机上已有一个发布好 80/443 的 Caddy 容器,但没挂证书目录」。
func existingCaddyTarget() *fakeTarget {
	return &fakeTarget{
		resultFor: func(cmd []string) *target.ExecResult {
			j := cmdJoin(cmd)
			switch {
			case strings.Contains(j, "docker inspect") && strings.Contains(j, "Mounts"):
				return &target.ExecResult{ExitCode: 0, Stdout: "/data /config "} // 未挂证书目录
			case strings.Contains(j, "docker inspect") && strings.Contains(j, "PortBindings"):
				return &target.ExecResult{ExitCode: 0, Stdout: "80/tcp=80 443/tcp=443 "}
			case strings.Contains(j, "docker inspect"):
				return &target.ExecResult{ExitCode: 0, Stdout: "true|caddy:2\n"}
			}
			return nil
		},
	}
}

func hasRmCall(calls [][]string) bool {
	for _, c := range calls {
		if len(c) >= 3 && c[0] == "docker" && c[1] == "rm" {
			return true
		}
	}
	return false
}

// 只有 auto 路由的主机:端口已覆盖 + 不需要证书挂载 → 幂等返回,绝不重建容器。
func TestEnsureCaddyKeepsContainerForAutoOnlyHost(t *testing.T) {
	ctx := context.Background()
	st := storetest.OpenDB(t)
	ft := existingCaddyTarget()
	svc := &service{store: NewStore(st), tg: ft, prober: fakeProber{}}

	if _, err := svc.Create(ctx, CreateInput{
		ServerID: "srv-1", Domain: "app.example.com", UpstreamContainer: "web", UpstreamPort: 8080,
	}); err != nil {
		t.Fatalf("Create(auto): %v", err)
	}
	if hasRmCall(ft.execCalls) {
		t.Fatalf("纯 auto 主机不应重建 Caddy 容器:\n%s", joinCalls(ft.execCalls))
	}
	if strings.Contains(joinCalls(ft.execCalls), "Mounts") {
		t.Fatalf("不需要证书挂载时不必探测挂载:\n%s", joinCalls(ft.execCalls))
	}
}

// 有 acme.sh 路由的主机:缺证书挂载 → 重建一次容器(补上 -v)。
func TestEnsureCaddyRebuildsForAcmeRouteHost(t *testing.T) {
	ctx := context.Background()
	st := storetest.OpenDB(t)
	s := NewStore(st)
	r := newRoute(CreateInput{
		ServerID: "srv-1", Domain: "app.example.com", UpstreamContainer: "web", UpstreamPort: 8080,
		TLSMode: TLSModeAcmeSh, DNSProviderID: "prov-1",
	})
	if err := s.insert(ctx, r); err != nil {
		t.Fatalf("insert: %v", err)
	}
	ft := existingCaddyTarget()
	svc := &service{store: s, tg: ft, prober: fakeProber{}}

	if err := svc.ensureAndApply(ctx, "srv-1", ""); err != nil {
		t.Fatalf("ensureAndApply: %v", err)
	}
	if !hasRmCall(ft.execCalls) {
		t.Fatalf("有 acme.sh 路由且容器缺证书挂载 → 应重建容器:\n%s", joinCalls(ft.execCalls))
	}
	run := joinCalls(ft.execCalls)
	if !strings.Contains(run, "-v /etc/pipewright/acme:/etc/pipewright/acme:ro") {
		t.Fatalf("重建的容器应挂上证书目录:\n%s", run)
	}
}
