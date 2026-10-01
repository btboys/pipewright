package build

import (
	"context"
	"testing"

	"github.com/btboys/pipewright/internal/pipeline"
	"github.com/btboys/pipewright/internal/project"
	"github.com/btboys/pipewright/internal/run"
)

// ─── resolveStageSource ────────────────────────────────────────────────────────

// gitSourceJob 造一个「拉取源码」节点;字段为 "" 表示画布上留空(回落项目/触发值)。
func gitSourceJob(repoURL, branch, credentialID string) pipeline.Job {
	cfg := map[string]any{}
	if repoURL != "" {
		cfg["repoUrl"] = repoURL
	}
	if branch != "" {
		cfg["branch"] = branch
	}
	if credentialID != "" {
		cfg["credentialId"] = credentialID
	}
	return pipeline.Job{ID: "job_src", Name: "拉取源码", Type: "git_source", Config: cfg}
}

func stageOf(jobs ...pipeline.Job) pipeline.Stage {
	return pipeline.Stage{ID: "stg_src", Name: "流水线源", Kind: pipeline.KindSource, Jobs: jobs}
}

func TestResolveStageSource(t *testing.T) {
	proj := &project.Project{RepoURL: "https://example.com/project.git", CredentialID: "cred-project"}
	r := &run.Run{ID: "r1", Trigger: run.Trigger{Branch: "feature/x"}}

	cases := []struct {
		name  string
		stage pipeline.Stage
		want  stageSource
	}{
		{
			name:  "无 git_source 节点 → 全回落项目绑定与触发分支",
			stage: stageOf(pipeline.Job{ID: "j1", Type: pipeline.StepTypeScript}),
			want:  stageSource{RepoURL: proj.RepoURL, CredentialID: proj.CredentialID, Branch: "feature/x"},
		},
		{
			name:  "节点字段留空 → 全回落",
			stage: stageOf(gitSourceJob("", "", ""), pipeline.Job{ID: "j1", Type: pipeline.StepTypeScript}),
			want:  stageSource{RepoURL: proj.RepoURL, CredentialID: proj.CredentialID, Branch: "feature/x"},
		},
		{
			name:  "三项全填 → 全按节点覆盖(仓库/分支/凭据都是用户选的)",
			stage: stageOf(gitSourceJob("https://codeup.aliyun.com/fxy/app.git", "release", "cred-node")),
			want:  stageSource{RepoURL: "https://codeup.aliyun.com/fxy/app.git", CredentialID: "cred-node", Branch: "release"},
		},
		{
			name:  "只覆盖凭据 → 仓库/分支仍回落",
			stage: stageOf(gitSourceJob("", "", "cred-node")),
			want:  stageSource{RepoURL: proj.RepoURL, CredentialID: "cred-node", Branch: "feature/x"},
		},
		{
			name:  "只钉分支 → 仓库/凭据仍回落",
			stage: stageOf(gitSourceJob("", "release", "")),
			want:  stageSource{RepoURL: proj.RepoURL, CredentialID: proj.CredentialID, Branch: "release"},
		},
		{
			name:  "两侧空白按留空处理(trim 后空)",
			stage: stageOf(gitSourceJob("  ", "   ", "  ")),
			want:  stageSource{RepoURL: proj.RepoURL, CredentialID: proj.CredentialID, Branch: "feature/x"},
		},
		{
			name:  "节点值两侧空白被 trim",
			stage: stageOf(gitSourceJob("  https://codeup.aliyun.com/x.git ", " release ", " cred-node ")),
			want:  stageSource{RepoURL: "https://codeup.aliyun.com/x.git", CredentialID: "cred-node", Branch: "release"},
		},
		{
			name:  "多 git_source 节点 → 首个生效(与日志口径一致)",
			stage: stageOf(gitSourceJob("https://a.example.com/a.git", "a", "cred-a"), gitSourceJob("https://b.example.com/b.git", "b", "cred-b")),
			want:  stageSource{RepoURL: "https://a.example.com/a.git", CredentialID: "cred-a", Branch: "a"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveStageSource(c.stage, proj, r); got != c.want {
				t.Errorf("resolveStageSource() = %+v, want %+v", got, c.want)
			}
		})
	}
}

// 退化输入不 panic:项目为 nil / run 为 nil / config 为 nil。
func TestResolveStageSourceNilInputs(t *testing.T) {
	if got := resolveStageSource(stageOf(), nil, nil); got != (stageSource{}) {
		t.Errorf("nil proj+run = %+v, want zero", got)
	}
	stage := stageOf(pipeline.Job{ID: "j", Type: "git_source"}) // Config nil
	want := stageSource{RepoURL: "https://example.com/p.git", CredentialID: "cred-p", Branch: "main"}
	if got := resolveStageSource(stage, &project.Project{RepoURL: want.RepoURL, CredentialID: want.CredentialID}, &run.Run{Trigger: run.Trigger{Branch: "main"}}); got != want {
		t.Errorf("nil config = %+v, want %+v", got, want)
	}
}

// ─── 克隆真正吃这份坐标 ──────────────────────────────────────────────────────────

// capturingCloner 记录克隆入参(替代真触网),用于断言「节点上填的覆盖值真的到了克隆」。
type capturingCloner struct {
	repoURL   string
	username  string
	token     string
	branch    string
	commit    string
	callCount int
}

func (c *capturingCloner) Clone(_ context.Context, repoURL, username, token, branch, commit, _ string) (*CloneResolved, error) {
	c.callCount++
	c.repoURL, c.username, c.token, c.branch, c.commit = repoURL, username, token, branch, commit
	return &CloneResolved{CommitShort: "abc1234"}, nil
}

// ─── 悬空凭据的回落兜底 ──────────────────────────────────────────────────────────

func TestStageGitAuth(t *testing.T) {
	cases := []struct {
		name         string
		secrets      map[string]string
		src          stageSource
		proj         *project.Project
		wantToken    string
		wantFallback bool
	}{
		{
			name:      "节点凭据可用 → 用它,无回落",
			secrets:   map[string]string{"cred-node": "node-token", "cred-project": "project-token"},
			src:       stageSource{CredentialID: "cred-node"},
			proj:      &project.Project{CredentialID: "cred-project"},
			wantToken: "node-token",
		},
		{
			name:         "节点凭据已成悬空引用 → 回落项目凭据(不让私有仓库静默匿名克隆)",
			secrets:      map[string]string{"cred-project": "project-token"},
			src:          stageSource{CredentialID: "cred-deleted"},
			proj:         &project.Project{CredentialID: "cred-project"},
			wantToken:    "project-token",
			wantFallback: true,
		},
		{
			name:      "节点未绑凭据 → 公开仓库匿名(不算悬空,不回落)",
			secrets:   map[string]string{"cred-project": "project-token"},
			src:       stageSource{},
			proj:      &project.Project{CredentialID: "cred-project"},
			wantToken: "",
		},
		{
			name:      "节点与项目指向同一凭据且已删 → 空凭据,不重复回落",
			secrets:   map[string]string{},
			src:       stageSource{CredentialID: "cred-gone"},
			proj:      &project.Project{CredentialID: "cred-gone"},
			wantToken: "",
		},
		{
			name:      "项目也没绑凭据 → 空凭据",
			secrets:   map[string]string{},
			src:       stageSource{CredentialID: "cred-gone"},
			proj:      &project.Project{},
			wantToken: "",
		},
		{
			name:      "项目无 project 行(nil)→ 空凭据,不 panic",
			secrets:   map[string]string{},
			src:       stageSource{CredentialID: "cred-gone"},
			proj:      nil,
			wantToken: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := &Builder{vault: fakeVault{secrets: c.secrets}}
			auth, fellBack := b.stageGitAuth(c.src, c.proj)
			if auth.Token != c.wantToken {
				t.Errorf("token = %q, want %q", auth.Token, c.wantToken)
			}
			if fellBack != c.wantFallback {
				t.Errorf("fellBack = %v, want %v", fellBack, c.wantFallback)
			}
		})
	}
}

// 保险库未注入(未配置保险库 / 单测)= 取不到凭据,按匿名克隆处理,不 panic。
func TestStageGitAuthWithoutVault(t *testing.T) {
	b := &Builder{}
	auth, fellBack := b.stageGitAuth(stageSource{CredentialID: "cred-x"}, &project.Project{CredentialID: "cred-y"})
	if auth.Token != "" || fellBack {
		t.Errorf("no vault = (%+v, %v), want (empty, false)", auth, fellBack)
	}
}

func TestCloneJobWorkspaceUsesStageSource(t *testing.T) {
	proj := &project.Project{ID: "p1", RepoURL: "https://example.com/project.git", CredentialID: "cred-project"}
	r := &run.Run{ID: "r1", ProjectID: "p1", Trigger: run.Trigger{Branch: "feature/x", Commit: "c0ffee"}}

	cases := []struct {
		name       string
		job        pipeline.Job
		wantRepo   string
		wantBranch string
		wantToken  string
	}{
		{
			name:       "节点留空 → 用项目绑定仓库与凭据、触发分支",
			job:        gitSourceJob("", "", ""),
			wantRepo:   "https://example.com/project.git",
			wantBranch: "feature/x",
			wantToken:  "project-token",
		},
		{
			name:       "节点全填 → 仓库/分支/凭据都按节点",
			job:        gitSourceJob("https://codeup.aliyun.com/fxy/app.git", "release", "cred-node"),
			wantRepo:   "https://codeup.aliyun.com/fxy/app.git",
			wantBranch: "release",
			wantToken:  "node-token",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cl := &capturingCloner{}
			b := newDAGTestBuilder(&recordingDriver{}, cl)
			b.vault = fakeVault{secrets: map[string]string{"cred-project": "project-token", "cred-node": "node-token"}}

			_, _, cleanup, err := b.cloneJobWorkspace(context.Background(), r, stageOf(c.job), proj, &fakeReporter{})
			defer cleanup()
			if err != nil {
				t.Fatalf("cloneJobWorkspace: %v", err)
			}
			if cl.callCount != 1 {
				t.Fatalf("clone called %d times, want 1", cl.callCount)
			}
			if cl.repoURL != c.wantRepo {
				t.Errorf("repoURL = %q, want %q", cl.repoURL, c.wantRepo)
			}
			if cl.branch != c.wantBranch {
				t.Errorf("branch = %q, want %q", cl.branch, c.wantBranch)
			}
			if cl.token != c.wantToken {
				t.Errorf("token = %q, want %q(凭据取错 → 私有仓库必然鉴权失败)", cl.token, c.wantToken)
			}
			if cl.commit != "c0ffee" {
				t.Errorf("commit = %q, want c0ffee", cl.commit)
			}
		})
	}
}
