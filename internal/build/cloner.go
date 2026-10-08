package build

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/btboys/pipewright/internal/gitauth"
)

// cloneTimeout 是单次克隆的硬超时(防黑洞 IP / 慢 DNS 把构建 goroutine 挂死)。
const cloneTimeout = 5 * time.Minute

// 克隆领域错误(不泄漏 URL 密钥/底层细节)。
var (
	// ErrRepoBlocked 表示仓库地址被 SSRF 收口拒绝(云元数据/链路本地/回环等)。
	ErrRepoBlocked = errors.New("build: repo url blocked by ssrf guard")
	// ErrCloneFailed 表示克隆/检出失败(鉴权/网络/ref 不存在等;不泄漏底层文本)。
	ErrCloneFailed = errors.New("build: clone failed")
)

// CloneError 是克隆失败的可读化错误:保留 ErrCloneFailed/ErrRepoBlocked 的 errors.Is 语义,
// 同时携带一条**已分类、已脱敏**的原因(Detail),供运行日志与 failure_log 展示。
//
// 为什么需要它:旧实现把 go-git 的底层错误整个丢弃、只回一句"源码克隆失败(鉴权/网络/ref
// 不存在或被 SSRF 拒绝)",把「到底是哪一种」——鉴权被拒、分支/commit 不存在、DNS 解析不了、
// 连接被拒、超时、还是地址被 SSRF 收口拒绝——全推给用户猜,是运维上的死胡同。
//
// 安全:Detail 只由本包构造,绝不透传底层 error 的原文(可能含 URL 与其内嵌密钥);凭据
// 只经 BasicAuth.Password 传递,本文件无任何路径把它写进字符串。
type CloneError struct {
	// Reason 是机器可辨的失败类别。
	Reason CloneReason
	// Detail 是给运维看的中文原因(可为空;绝不含 token/密码)。
	Detail string
	// Hint 是可执行的下一步(该去检查哪个字段/哪份配置),随原因一起展示。
	Hint string
	// err 是归类的哨兵错误(ErrCloneFailed / ErrRepoBlocked),供 errors.Is 匹配。
	err error
}

func (e *CloneError) Error() string {
	if e == nil {
		return ""
	}
	if e.Detail != "" {
		return e.Detail
	}
	return e.err.Error()
}

// Unwrap 暴露归类哨兵:errors.Is(err, ErrCloneFailed) / ErrRepoBlocked 保持成立。
func (e *CloneError) Unwrap() error { return e.err }

// CloneFailureText 组装「源码克隆失败」的完整展示文本(日志行 / failure_log 共用一份口径)。
// 返回的文本绝不含 token 与 URL 内嵌凭据;repoURL 只在调用方另行记录。
func CloneFailureText(detail string) string {
	const head = "源码克隆失败"
	if strings.TrimSpace(detail) == "" {
		return head + "(鉴权/网络/ref 不存在或被 SSRF 拒绝)"
	}
	return head + ":" + detail
}

// cloneFailureDetail 从克隆错误里取「人话原因 + 下一步」:本包产出的 *CloneError 用其分类
// Detail/Hint;其它来源(repocache 直连回退、测试桩)回退为已去 URL 的底层文本,
// 保证日志里永远有可排查信息,而不是一句万能猜测。
func cloneFailureDetail(err error) string {
	if err == nil {
		return ""
	}
	var ce *CloneError
	if errors.As(err, &ce) {
		switch {
		case ce.Detail != "" && ce.Hint != "":
			return ce.Detail + " → " + ce.Hint
		case ce.Detail != "":
			return ce.Detail
		case ce.Hint != "":
			return ce.Hint
		}
	}
	return scrubURLs(err.Error())
}

// CloneReason 是克隆失败的机器可辨类别。前端/诊断若要按类别分支,认这些常量而非错误文本。
type CloneReason string

const (
	// ReasonRepoBlocked:仓库地址被 SSRF 收口拒绝(非 http/https、云元数据/链路本地/回环、缺 host)。
	ReasonRepoBlocked CloneReason = "repo_blocked"
	// ReasonEmptyRepoURL:项目/节点上没配仓库地址。
	ReasonEmptyRepoURL CloneReason = "empty_repo_url"
	// ReasonAuthFailed:鉴权被拒(凭据缺失/错误,或私有仓库未授权)。
	ReasonAuthFailed CloneReason = "auth_failed"
	// ReasonRepoNotFound:远端没有这个仓库(或无权看到,平台常以 404 表达)。
	ReasonRepoNotFound CloneReason = "repo_not_found"
	// ReasonRefNotFound:分支/tag/commit 在远端不存在(浅克隆下也可能是 commit 不在历史里)。
	ReasonRefNotFound CloneReason = "ref_not_found"
	// ReasonDNSFailed:域名解析失败。
	ReasonDNSFailed CloneReason = "dns_failed"
	// ReasonConnRefused:连接被拒(端口不通/服务未监听)。
	ReasonConnRefused CloneReason = "conn_refused"
	// ReasonTimeout:克隆超时(黑洞 IP/慢 DNS/网络不通)。
	ReasonTimeout CloneReason = "timeout"
	// ReasonTLSFailed:TLS 证书校验/握手失败。
	ReasonTLSFailed CloneReason = "tls_failed"
	// ReasonCheckoutFailed:远端已连通,但检出到指定 commit 失败(commit 不在克隆历史里)。
	ReasonCheckoutFailed CloneReason = "checkout_failed"
	// ReasonUnknown:未能归类(此时 Detail 保留已脱敏的底层文本,供进一步排查)。
	ReasonUnknown CloneReason = "unknown"
)

// classifyCloneErr 把 go-git 的底层错误归成类别 + 中文原因,并返回给 errors.Is 的哨兵。
//
// 判定优先用哨兵错误(transport.ErrAuthenticationRequired 等),仅在无法用哨兵归类时才回退到
// 文本嗅探 —— 且只嗅探**协议/网络库自身**的关键字(no such host / connection refused / timeout /
// x509),不对 HTTP 状态码做子串猜测。远端回显的 ref 名按**原大小写**保留(分支名大小写敏感,
// 保留原文才方便与远端对照),它来自本次克隆请求的 branch/commit 参数,不是机密。
func classifyCloneErr(err error) *CloneError {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, transport.ErrAuthenticationRequired):
		return &CloneError{Reason: ReasonAuthFailed, Detail: "鉴权失败:远端要求认证", Hint: "请检查项目/本阶段「拉取源码」节点绑定的访问凭据是否有效,以及该账号是否有此仓库权限", err: ErrCloneFailed}
	case errors.Is(err, transport.ErrAuthorizationFailed), errors.Is(err, transport.ErrInvalidAuthMethod):
		return &CloneError{Reason: ReasonAuthFailed, Detail: "鉴权失败:凭据被远端拒绝(令牌过期/权限不足/用户名与令牌不匹配)", Hint: "Gitee 等平台要求用户名与账号严格配对,请核对凭据里的 Git 用户名", err: ErrCloneFailed}
	case errors.Is(err, transport.ErrRepositoryNotFound):
		return &CloneError{Reason: ReasonRepoNotFound, Detail: "仓库不存在或当前凭据无权访问", Hint: "请核对仓库地址拼写;私有仓库需在项目或「拉取源码」节点上绑定可用凭据", err: ErrCloneFailed}
	}

	msg := strings.ToLower(err.Error())
	var netErr net.Error
	switch {
	case isRemoteRefMissing(err, msg):
		return &CloneError{Reason: ReasonRefNotFound, Detail: "分支/commit 在远端不存在(远端报:" + firstQuoted(err.Error()) + ")", Hint: "请核对触发分支/commit 是否已推送到远端", err: ErrCloneFailed}
	case strings.Contains(msg, "no such host"):
		return &CloneError{Reason: ReasonDNSFailed, Detail: "域名解析失败:无法解析仓库主机名", Hint: "请核对仓库地址拼写,以及运行 pipewright 的主机能否解析该域名", err: ErrCloneFailed}
	case strings.Contains(msg, "connection refused"):
		return &CloneError{Reason: ReasonConnRefused, Detail: "连接被拒:目标端口未监听或被防火墙拒绝", Hint: "请确认 Git 服务可达(自建 Git 注意内网与端口策略)", err: ErrCloneFailed}
	case strings.Contains(msg, "certificate") || strings.Contains(msg, "x509") || strings.Contains(msg, "tls:"):
		return &CloneError{Reason: ReasonTLSFailed, Detail: "TLS 校验失败:证书不受信或握手被中间设备中断", Hint: "请为自签证书配置受信 CA,或改用 http(仅限内网)", err: ErrCloneFailed}
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(msg, "timeout"), strings.Contains(msg, "timed out"):
		return &CloneError{Reason: ReasonTimeout, Detail: "克隆超时(网络不可达或远端无响应)", Hint: "请检查运行 pipewright 的主机到 Git 服务的出网连通性", err: ErrCloneFailed}
	case errors.As(err, &netErr) && netErr.Timeout():
		return &CloneError{Reason: ReasonTimeout, Detail: "克隆超时(网络不可达或远端无响应)", Hint: "请检查运行 pipewright 的主机到 Git 服务的出网连通性", err: ErrCloneFailed}
	default:
		// 兜底:带上已脱敏的底层摘要(去掉 URL 形态的片段),否则用户又只能猜。
		return &CloneError{Reason: ReasonUnknown, Detail: "克隆失败:" + scrubURLs(err.Error()), Hint: "请对照上面的底层报错排查仓库地址/凭据/网络", err: ErrCloneFailed}
	}
}

// isRemoteRefMissing 判定「远端没有该 ref」。go-git 对 SingleBranch 浅克隆的缺失分支返回
// 形如 `couldn't find remote ref "refs/heads/x"` 的 transport 错误。
func isRemoteRefMissing(err error, lowMsg string) bool {
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return true
	}
	return strings.Contains(lowMsg, "couldn't find remote ref") ||
		strings.Contains(lowMsg, "reference not found") ||
		strings.Contains(lowMsg, "unknown revision")
}

// firstQuoted 取文本里首个双引号片段(原大小写),用于把远端回显的 ref 名原样带进失败原因;
// 没有引号片段时回退为整段(仍经 scrubURLs 去 URL)。结果绝不含 URL,避免细节外泄。
func firstQuoted(s string) string {
	if i := strings.IndexByte(s, '"'); i >= 0 {
		if j := strings.IndexByte(s[i+1:], '"'); j >= 0 {
			return scrubURLs(s[i+1 : i+1+j])
		}
	}
	return scrubURLs(s)
}

// scrubURLs 把文本里 http(s):// 形态的片段替换为占位符:URL 里可能内嵌 userinfo(用户名+令牌),
// 兜底文本绝不允许把它带进日志/failure_log。
func scrubURLs(s string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "://")
		if i < 0 {
			b.WriteString(s)
			break
		}
		// 回删 scheme 段(字母/数字/+-. )
		start := i
		for start > 0 {
			ch := s[start-1]
			if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '+' || ch == '-' || ch == '.' {
				start--
				continue
			}
			break
		}
		b.WriteString(s[:start])
		b.WriteString("<url>")
		// 跳过 URL 主体,直到空白/引号/右括号
		j := i + 3
		for j < len(s) {
			ch := s[j]
			if ch == ' ' || ch == '\t' || ch == '\n' || ch == '"' || ch == '\'' || ch == ')' || ch == ']' {
				break
			}
			j++
		}
		s = s[j:]
	}
	return b.String()
}

// Cloner 把项目仓库在指定 commit/branch 上克隆到**磁盘临时工作区**(Docker build 需 `.` 上下文落盘)。
//
// 与 2-5/3-6 的内存克隆(memfs)不同:构建上下文须是真实目录树供容器 CLI 读取。工作区由调用方
// (Builder)经 MkdirTemp 建、defer RemoveAll 销(宿主零污染,FR-5)。SSRF 收口复用全平台同款策略:
// 仅 http/https;拒云元数据/链路本地/回环;私网放行(自托管内网 Git 友好)。
type Cloner struct {
	// allowInsecure 仅供测试:为 true 时跳过 SSRF scheme/host 校验(放行 file:// 本地夹具)。
	// 生产路径绝不设置(NewCloner 默认 false)。
	allowInsecure bool
}

// NewCloner 构造生产 Cloner(严格 SSRF 收口)。
func NewCloner() *Cloner { return &Cloner{} }

// CloneResolved 是克隆结果:实际检出的 commit 短 sha(供产物 tag/引用),空则未解析出。
type CloneResolved struct {
	CommitShort string
}

// Clone 把 repoURL 在 ref(commit sha 优先,否则分支名;皆空则默认分支)上克隆到 destDir。
// token 经 BasicAuth.Password 传入(绝不进 URL/日志/错误)。失败统一映射干净错误。
//
// 策略:先克隆默认/指定分支(Depth:1 浅克隆省带宽),若指定了 commit 则再 checkout 到该 commit
//
//	(commit 不在浅克隆历史里时回退为不限深克隆重试一次,best-effort)。
func (c *Cloner) Clone(ctx context.Context, repoURL, username, token, branch, commit, destDir string) (*CloneResolved, error) {
	repoURL = strings.TrimSpace(repoURL)
	if repoURL == "" {
		return nil, &CloneError{Reason: ReasonEmptyRepoURL, Detail: "未配置仓库地址", Hint: "请在项目或本阶段「拉取源码」节点上填写 Git 仓库地址", err: ErrCloneFailed}
	}
	if !c.allowInsecure {
		if detail := repoURLSSRFDetail(repoURL); detail != "" {
			return nil, &CloneError{Reason: ReasonRepoBlocked, Detail: detail, Hint: "仓库地址只支持 http/https,且不能指向本机/云元数据地址", err: ErrRepoBlocked}
		}
	}

	cctx, cancel := context.WithTimeout(ctx, cloneTimeout)
	defer cancel()

	auth := gitauth.BasicAuth(repoURL, username, token)
	commit = strings.TrimSpace(commit)
	branch = strings.TrimSpace(branch)

	opts := &gogit.CloneOptions{
		URL:  repoURL,
		Auth: auth,
		Tags: gogit.NoTags,
	}
	// 指定了具体 commit 时不浅克隆(浅克隆默认分支可能不含该 commit);否则浅克隆指定/默认分支。
	if commit == "" {
		opts.Depth = 1
		opts.SingleBranch = true
		if branch != "" {
			opts.ReferenceName = plumbing.NewBranchReferenceName(branch)
		}
	}

	repo, err := gogit.PlainCloneContext(cctx, destDir, false, opts)
	if err != nil {
		// 浅克隆 + 指定分支失败时不再重试(可能是鉴权/不可达);归类后映射干净错误。
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return nil, &CloneError{Reason: ReasonTimeout, Detail: "克隆超时(5 分钟未完成):网络不可达或远端无响应", Hint: "请检查出网连通性;大仓库持续超时可考虑启用本地镜像缓存", err: ErrCloneFailed}
		}
		return nil, classifyCloneErr(err)
	}

	resolved := &CloneResolved{}
	if commit != "" {
		wt, werr := repo.Worktree()
		if werr != nil {
			return nil, &CloneError{Reason: ReasonUnknown, Detail: "克隆成功但打开工作区失败", Hint: "请重试;持续失败请检查临时目录权限", err: ErrCloneFailed}
		}
		hash := plumbing.NewHash(commit)
		if cerr := wt.Checkout(&gogit.CheckoutOptions{Hash: hash, Force: true}); cerr != nil {
			return nil, &CloneError{Reason: ReasonCheckoutFailed, Detail: "检出失败:commit " + shortSHA(commit) + " 不在此仓库的克隆历史中(或不是有效 sha)", Hint: "请确认该 commit 已推送到本仓库", err: ErrCloneFailed}
		}
		resolved.CommitShort = shortSHA(commit)
	} else if head, herr := repo.Head(); herr == nil {
		resolved.CommitShort = shortSHA(head.Hash().String())
	}
	return resolved, nil
}

// IsRepoURLAllowed 是 validRepoURL 的导出包装(供 repocache 等复用同款 SSRF 收口,不重复实现)。
func IsRepoURLAllowed(repoURL string) bool { return validRepoURL(repoURL) }

// validRepoURL 对仓库地址做 SSRF 收口(生产路径),复用全平台同款策略:
// 仅 http/https;拒云元数据/链路本地/回环;私网放行(自托管内网 Git 友好)。
func validRepoURL(repoURL string) bool { return repoURLSSRFDetail(repoURL) == "" }

// repoURLSSRFDetail 是 SSRF 收口的「人话版」:地址合规返回空串,被拒返回具体原因
// (供克隆失败日志区分「地址被拒」与「网络/鉴权失败」)。判定与 validRepoURL 完全同源,
// 只是把「为什么拒」讲清楚:scheme 不是 http/https、缺 host、或解析到的 IP 落在禁止区。
func repoURLSSRFDetail(repoURL string) string {
	raw := strings.TrimSpace(repoURL)
	u, err := url.Parse(raw)
	if err != nil {
		return "仓库地址无法解析(SSRF 收口拒绝)"
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		scheme := u.Scheme
		if scheme == "" {
			scheme = "(空)"
		}
		return "仓库地址被 SSRF 收口拒绝:仅支持 http/https,当前 scheme=" + scheme
	}
	host := u.Hostname()
	if host == "" {
		return "仓库地址被 SSRF 收口拒绝:缺少主机名"
	}
	if ip := net.ParseIP(host); ip != nil {
		if blockedIP(ip) {
			return "仓库地址被 SSRF 收口拒绝:" + host + " 属回环/链路本地(含云元数据)地址"
		}
		return ""
	}
	addrs, err := net.LookupIP(host)
	if err != nil || len(addrs) == 0 {
		return "" // 留给 clone 路径(会失败映射);不在此误拒临时 DNS 抖动。
	}
	for _, ip := range addrs {
		if blockedIP(ip) {
			return "仓库地址被 SSRF 收口拒绝:" + host + " 解析到回环/链路本地(含云元数据)地址"
		}
	}
	return ""
}

// blockedIP 判定 IP 是否落在禁止区:回环、链路本地(含云元数据 169.254.169.254)、未指定。
// 私网(RFC1918 / fc00::/7)不在此列(放行)。
func blockedIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified()
}

// shortSHA 取 commit 的前 7 位(短 sha);不足 7 位原样返回。
func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// mkTempWorkspace 建一个临时构建工作区目录(调用方 defer RemoveAll 销毁;宿主零污染,FR-5)。
func mkTempWorkspace() (string, error) {
	return os.MkdirTemp("", "pipewright-build-*")
}
