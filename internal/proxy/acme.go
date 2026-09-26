package proxy

import (
	"context"
	"fmt"
	"strings"

	"github.com/btboys/pipewright/internal/target"
)

// acme.sh 证书管理(tls_mode=acme.sh)。
//
// 定位:tls_mode=acme.sh 的路由,证书**不由 Caddy 自签**,而由目标主机上的 acme.sh 经 DNS-01 签发,
// 再让 Caddy 以显式 `tls <fullchain> <privkey>` 加载。这样 stock caddy:2 也能签泛域名 ——
// 不依赖含 DNS 插件的自构建镜像。
//
// 数据流(全程经 target.Service 的 SSH + array 命令,不拼 shell):
//
//	DNS 凭据(进程内取自 vault) → acme.sh --issue --dns <plugin> -d <domain...>(env 前缀注入凭据)
//	                          → acme.sh --install-cert → <acmeHostDir>/<dir>/fullchain.pem + privkey.pem
//	                          → 只读挂进 Caddy 容器(acmeHostDir → acmeContainerDir)
//	                          → Caddyfile `tls <acmeContainerDir>/<dir>/fullchain.pem <...>/privkey.pem`
//
// 续期由 acme.sh 自带 cron 完成(--install-cert 注册的 --reloadcmd 会在续期后自动重载 Caddy);
// Pipewright 只负责首次签发,以及运维手动触发的「签发 / 续期」。
//
// 为何必须 DNS-01:该模式下 Caddy 仍占着宿主 80/443,acme.sh 的 standalone / webroot 无从校验。
const (
	// acmeHostDir 是目标主机上存放 acme.sh 签发结果(fullchain / privkey)的目录。
	acmeHostDir = "/etc/pipewright/acme"
	// acmeContainerDir 是把 acmeHostDir 只读挂进 Caddy 容器的路径(Caddyfile 里的证书路径以此开头)。
	acmeContainerDir = acmeHostDir
	// acmeShBin 是目标主机上 acme.sh 的可执行名(需已在 PATH,即已 `acme.sh --install`)。
	acmeShBin = "acme.sh"
	// acmeCertFile / acmeKeyFile 是 install-cert 落盘的文件名(Caddy 显式 tls 读这两个)。
	acmeCertFile = "fullchain.pem"
	acmeKeyFile  = "privkey.pem"
	// acmeKeyLength 是签发密钥类型(ec-256:体积小、握手快,LE 与各 DNS 厂商均支持)。
	acmeKeyLength = "ec-256"
	// acmeReloadCmd 是 acme.sh 每次签发/续期成功后执行的 Caddy 重载命令(优雅热加载,零停机)。
	// acme.sh 在 cron 续期时也会执行它,故证书更新后 Caddy 无需人工干预即生效。纯常量,无用户输入。
	acmeReloadCmd = "docker exec " + caddyContainer + " caddy reload --config " + caddyfilePath + " --adapter caddyfile"
)

// acmeCertDirName 把域名映射为证书落盘目录名(文件系统安全:通配符 `*` 换成 `_`)。
// 例:`example.com` → `example.com`;`*.example.com` → `_.example.com`。
func acmeCertDirName(domain string) string { return strings.ReplaceAll(domain, "*", "_") }

// acmeHostCertDir 返回该域名证书在**目标主机上**的目录(install-cert 落盘 / 探测读取)。
func acmeHostCertDir(domain string) string { return acmeHostDir + "/" + acmeCertDirName(domain) }

// acmeHostCertPath 返回该域名 fullchain 在目标主机上的路径。
func acmeHostCertPath(domain string) string { return acmeHostCertDir(domain) + "/" + acmeCertFile }

// acmeContainerCertPath / acmeContainerKeyPath 返回 Caddyfile 里引用的**容器内**证书/私钥路径。
func acmeContainerCertPath(domain string) string {
	return acmeContainerDir + "/" + acmeCertDirName(domain) + "/" + acmeCertFile
}

func acmeContainerKeyPath(domain string) string {
	return acmeContainerDir + "/" + acmeCertDirName(domain) + "/" + acmeKeyFile
}

// acmeDNSPlugin 把 Pipewright 的 DNS 提供商类型 + 凭据映射为 acme.sh 的 DNS 插件名与环境变量。
// 凭据形态与 renderSite 的 Caddy DNS-01 渲染一致:单 token(cloudflare)/ "id,secret" 两段(其余)。
// 未知类型 / 凭据不完整 → ErrInvalidDNSProvider(不猜、不降级)。
func acmeDNSPlugin(providerType, token string) (string, []string, error) {
	id, secret := splitDNSCred(token)
	switch providerType {
	case "cloudflare":
		if token == "" {
			return "", nil, ErrInvalidDNSProvider
		}
		return "dns_cf", []string{"CF_Token=" + token}, nil
	case "dnspod", "tencentcloud":
		// 腾讯云 DNSPod:acme.sh 用 dns_dp 插件,凭据为 API ID + Token 两段。
		if id == "" || secret == "" {
			return "", nil, ErrInvalidDNSProvider
		}
		return "dns_dp", []string{"DP_Id=" + id, "DP_Key=" + secret}, nil
	case "alidns":
		// 阿里云 DNS:acme.sh 用 dns_ali 插件,凭据为 AccessKeyId + AccessKeySecret 两段。
		if id == "" || secret == "" {
			return "", nil, ErrInvalidDNSProvider
		}
		return "dns_ali", []string{"Ali_Key=" + id, "Ali_Secret=" + secret}, nil
	default:
		return "", nil, ErrInvalidDNSProvider
	}
}

// issueAcmeCert 在目标主机上经 acme.sh 签发(或按 acme.sh 自己的节奏续期)证书,install-cert 落盘到
// acmeHostDir,并注册 Caddy 重载命令。
//
// 幂等与限速:不加 --force —— acme.sh 在证书仍在有效期时直接跳过(打印下次续期时间),避免反复重签
// 触发 Let's Encrypt 速率限制。确需强制重签由运维在主机上自行 `acme.sh --renew --force`。
//
// domain 是主域名(acme.sh 以此为证书主键,--install-cert -d 也用它),aliases 一并签进同一张证书
// (与 Caddy 站点块覆盖的域名集合一致)。域名均已经 domainRe / wildcardDomainRe 校验(无空白/引号/
// shell 元字符),且全程以 array 形式传参,不拼 shell。
func issueAcmeCert(ctx context.Context, tg target.Service, serverID, domain string, aliases []string, providerType, token string) error {
	plugin, env, err := acmeDNSPlugin(providerType, token)
	if err != nil {
		return err
	}
	// 1) 证书落盘目录(容器内只读挂载的宿主侧目录)。
	dir := acmeHostCertDir(domain)
	if res, eerr := tg.Exec(ctx, serverID, []string{"mkdir", "-p", dir}); eerr != nil {
		return mapExecErr(eerr)
	} else if res.ExitCode != 0 {
		return fmt.Errorf("%w:mkdir %s: %s", ErrAcmeIssue, dir, strings.TrimSpace(firstNonEmpty(res.Stderr, res.Stdout)))
	}

	// 2) 签发:env 前缀注入 DNS 凭据(凭据只经进程 argv,不落盘、不经 shell、不入日志)。
	issue := append([]string{"env"}, env...)
	issue = append(issue,
		acmeShBin, "--issue",
		"--dns", plugin,
		"--keylength", acmeKeyLength,
		"--server", "letsencrypt",
		"-d", domain,
	)
	for _, a := range aliases {
		issue = append(issue, "-d", a)
	}
	if res, eerr := tg.Exec(ctx, serverID, issue); eerr != nil {
		return mapExecErr(eerr)
	} else if res.ExitCode != 0 {
		return fmt.Errorf("%w:%s", ErrAcmeIssue, acmeIssueReason(res))
	}

	// 3) 安装证书到宿主目录 + 注册重载命令(续期后 acme.sh 会自行执行它重载 Caddy)。
	install := []string{
		acmeShBin, "--install-cert", "-d", domain, "--ecc",
		"--key-file", dir + "/" + acmeKeyFile,
		"--fullchain-file", acmeHostCertPath(domain),
		"--reloadcmd", acmeReloadCmd,
	}
	if res, eerr := tg.Exec(ctx, serverID, install); eerr != nil {
		return mapExecErr(eerr)
	} else if res.ExitCode != 0 {
		return fmt.Errorf("%w:%s", ErrAcmeIssue, strings.TrimSpace(firstNonEmpty(res.Stderr, res.Stdout)))
	}
	return nil
}

// acmeFailDetail 从 acme.sh 失败错误里抽出可展示的人话原因(剥掉 ErrAcmeIssue 前缀)。
// 非 ErrAcmeIssue 的错误(传输层等)原样返回错误文本。
func acmeFailDetail(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimPrefix(err.Error(), ErrAcmeIssue.Error()+":")
	msg = strings.TrimSpace(msg)
	if msg == "" {
		msg = ErrAcmeIssue.Error()
	}
	return msg
}

// acmeIssueReason 把 acme.sh 的失败输出压成人话可读的原因(acme.sh 把详情写到 stdout)。
// exit 127 = 命令不存在 → 明确提示「主机未安装 acme.sh」,而不是把 shell 的英文报错丢给用户。
func acmeIssueReason(res *target.ExecResult) string {
	if res.ExitCode == 127 {
		return "目标主机未安装 acme.sh(请先在主机上执行:curl https://get.acme.sh | sh -s email=you@example.com)"
	}
	msg := strings.TrimSpace(firstNonEmpty(res.Stderr, res.Stdout))
	if msg == "" {
		return fmt.Sprintf("acme.sh 退出码 %d,无输出", res.ExitCode)
	}
	// acme.sh 输出冗长;取末尾 300 字符(错误原因通常在最后)。
	if len(msg) > 300 {
		msg = "…" + msg[len(msg)-300:]
	}
	return msg
}

// acmeCertsReady 报告一批证书目录里哪些**已在目标主机上装好证书文件**(fullchain 存在)。
// 单次 SSH 往返:一个静态 sh 脚本 + 目录名作位置参数(不拼进脚本文本,注入面为空)。
// 传输层错误 / 脚本失败 → 全部视为「未就绪」(调用方据此保守渲染,不让 Caddy 引用不存在的文件)。
//
// 为何要预判:Caddy 对 `tls <cert> <key>` 引用的文件在**配置加载期**即读取,文件不存在会导致
// reload 失败(不像自动 ACME 那样容忍异步失败),故渲染前必须先确认文件在位。
//
// 入参 dirNames 是 acmeCertDirName(domain) 的结果(文件系统安全的目录名)。
func acmeCertsReady(ctx context.Context, tg target.Service, serverID string, dirNames []string) map[string]bool {
	ready := map[string]bool{}
	if tg == nil || strings.TrimSpace(serverID) == "" || len(dirNames) == 0 {
		return ready
	}
	const script = `for d in "$@"; do [ -f "$ROOT/$d/` + acmeCertFile + `" ] && echo "$d"; done`
	args := append([]string{"env", "ROOT=" + acmeHostDir, "sh", "-c", script, "_"}, dirNames...)
	res, err := tg.Exec(ctx, serverID, args)
	if err != nil || res == nil || res.ExitCode != 0 {
		return ready
	}
	for _, tok := range strings.Fields(res.Stdout) {
		ready[tok] = true
	}
	return ready
}
