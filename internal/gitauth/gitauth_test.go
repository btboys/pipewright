package gitauth

import "testing"

func TestUsername(t *testing.T) {
	cases := []struct {
		name    string
		repoURL string
		want    string
	}{
		// Gitee:取 owner 段当用户名(用户的真实仓库场景)
		{"gitee personal repo", "https://gitee.com/cool-jiawei/aireboot.git", "cool-jiawei"},
		{"gitee no .git suffix", "https://gitee.com/cool-jiawei/aireboot", "cool-jiawei"},
		{"gitee with port", "https://gitee.com:443/octo/app.git", "octo"},
		{"gitee subdomain", "https://api.gitee.com/octo/app.git", "octo"},
		{"gitee with creds in url", "https://u:p@gitee.com/octo/app.git", "octo"},
		// 非 Gitee:沿用 "git"(不回归)
		{"github", "https://github.com/octo/app.git", "git"},
		{"gitlab", "https://gitlab.com/octo/app.git", "git"},
		{"self-hosted", "https://git.example.com/octo/app.git", "git"},
		// 退化:仍回退 "git",不 panic
		{"empty", "", "git"},
		{"garbage", "::::not a url", "git"},
		{"gitee no path", "https://gitee.com", "git"},
		{"gitee root slash", "https://gitee.com/", "git"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Username(c.repoURL, ""); got != c.want {
				t.Errorf("Username(%q, empty) = %q, want %q", c.repoURL, got, c.want)
			}
		})
	}
}

func TestBasicAuthCarriesToken(t *testing.T) {
	auth := BasicAuth("https://gitee.com/cool-jiawei/aireboot.git", "actual-account", "token-value")
	if auth.Username != "actual-account" {
		t.Errorf("username = %q, want actual-account", auth.Username)
	}
	if auth.Password != "token-value" {
		t.Errorf("password not carried through")
	}
}

func TestBasicAuthReturnsNilWithoutToken(t *testing.T) {
	if auth := BasicAuth("file:///tmp/fixture", "", ""); auth != nil {
		t.Fatalf("empty token should use anonymous access, got %+v", auth)
	}
}

func TestUsernameUsesExplicitAccountForAnyHost(t *testing.T) {
	// 显式用户名对所有 host 生效:Bitbucket / 云效 Codeup / 自建 Gitea、GitLab
	// 的「账号 + 密码(或 App Password)」必须配对真实用户名,写死 "git" 必被拒。
	cases := []struct {
		name    string
		repoURL string
	}{
		{"gitee", "https://gitee.com/university-org/private-repo.git"},
		{"github", "https://github.com/org/private-repo.git"},
		{"gitlab", "https://gitlab.example.com/org/private-repo.git"},
		{"codeup", "https://codeup.aliyun.com/fxy/fenxi365/financial-v5.git"},
		{"bitbucket", "https://bitbucket.org/org/private-repo.git"},
		{"gitee subdomain", "https://api.gitee.com/octo/app.git"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Username(c.repoURL, "actual-account"); got != "actual-account" {
				t.Fatalf("Username(%q, explicit) = %q, want actual-account", c.repoURL, got)
			}
		})
	}
}

func TestUsernameTrimsExplicitAccount(t *testing.T) {
	if got := Username("https://gitlab.example.com/org/app.git", "  actual-account  "); got != "actual-account" {
		t.Fatalf("Username() = %q, want trimmed actual-account", got)
	}
}

// 显式用户名为空时保持历史回退:仅 Gitee 取 owner 段,其余 host 仍是 "git"。
func TestUsernameFallsBackWhenExplicitAccountBlank(t *testing.T) {
	cases := []struct {
		name    string
		repoURL string
		want    string
	}{
		{"blank explicit gitee falls back to owner", "https://gitee.com/cool-jiawei/aireboot.git", "cool-jiawei"},
		{"blank explicit github", "https://github.com/org/private-repo.git", "git"},
		{"blank explicit codeup", "https://codeup.aliyun.com/fxy/fenxi365/financial-v5.git", "git"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, blank := range []string{"", "   "} {
				if got := Username(c.repoURL, blank); got != c.want {
					t.Fatalf("Username(%q, %q) = %q, want %q", c.repoURL, blank, got, c.want)
				}
			}
		})
	}
}
