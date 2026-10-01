// Package gitauth centralizes how an HTTPS git token is turned into BasicAuth
// credentials for clone / ls-remote across the codebase (source reader, project
// prober, build cloner, AI diff/analyze).
//
// 背景:不同平台对「token 经 HTTPS BasicAuth」的用户名要求不同:
//   - GitHub / GitLab 等:用户名任意非空,密码=token(历史上本项目一律写死
//     Username="git",对这些平台没问题)。
//   - **Gitee**:个人访问令牌走 HTTPS 时,用户名必须是「真实账号用户名」,密码=token;
//     用 "git" 当用户名会被拒(表现为「凭据错误 / 认证失败」)。这正是用户真实
//     Gitee 令牌克隆失败的根因。
//   - **Bitbucket / 云效 Codeup / 自建 Gitea、GitLab 等**:填的是「账号 + 密码(或
//     App Password)」,用户名必须与账号严格配对;写死 "git" 必被拒。
//
// 因此用户名按「显式填写优先」决定:
//  1. 凭据里填了 Git 用户名 → 任何 host 都照用(通用账号密码场景的唯一正确做法);
//  2. 没填且 host 是 gitee.com(及其子域)→ 取 URL 路径 owner 段(个人仓库场景
//     owner==账号名,正确);
//  3. 其余 → 沿用 "git"(与历史行为一致,不回归 GitHub/GitLab 的 token 场景)。
//
// 安全:token 仅作为 BasicAuth.Password,绝不拼进 URL / 日志 / 错误。
package gitauth

import (
	"net/url"
	"strings"

	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// defaultUsername 是既没显式用户名、又取不到 Gitee owner 段时的兜底用户名
// (GitHub / GitLab 的 token 场景接受任意非空用户名)。
const defaultUsername = "git"

// BasicAuth 依据 repoURL 选择合适的 BasicAuth 用户名,密码恒为 token。
// token 为空时返回 nil，让 go-git 按匿名公开仓访问。
func BasicAuth(repoURL, username, token string) *githttp.BasicAuth {
	if strings.TrimSpace(token) == "" {
		return nil
	}
	return &githttp.BasicAuth{Username: Username(repoURL, username), Password: token}
}

// Username 返回该 repoURL 应使用的 BasicAuth 用户名。
//   - 显式用户名非空:任何 host 都照用(账号密码 / App Password 场景必须配对真实用户名)。
//   - 显式用户名为空且 host 是 gitee.com / *.gitee.com:取 URL 路径第一段(owner);取不到则回退 "git"。
//   - 其余 host:恒为 "git"(历史行为,不回归 GitHub/GitLab 的 token 场景)。
//
// 解析失败 / 无 host 时回退 "git",绝不 panic。
func Username(repoURL, explicitUsername string) string {
	if username := strings.TrimSpace(explicitUsername); username != "" {
		return username
	}
	u, err := url.Parse(strings.TrimSpace(repoURL))
	if err != nil {
		return defaultUsername
	}
	host := strings.ToLower(u.Hostname()) // Hostname() 自动剥离端口与用户信息
	if host == "" {
		return defaultUsername
	}
	if host == "gitee.com" || strings.HasSuffix(host, ".gitee.com") {
		if owner := firstPathSegment(u.Path); owner != "" {
			return owner
		}
	}
	return defaultUsername
}

// firstPathSegment 取 URL path 的第一段(owner)。"/cool-jiawei/aireboot.git" → "cool-jiawei"。
func firstPathSegment(p string) string {
	p = strings.TrimLeft(p, "/")
	if p == "" {
		return ""
	}
	if i := strings.IndexByte(p, '/'); i >= 0 {
		p = p[:i]
	}
	return strings.TrimSpace(p)
}
