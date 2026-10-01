package build

import (
	"errors"
	"strings"

	"github.com/btboys/pipewright/internal/pipeline"
	"github.com/btboys/pipewright/internal/project"
	"github.com/btboys/pipewright/internal/run"
	"github.com/btboys/pipewright/internal/vault"
)

// stageSource 是某阶段克隆源码时**实际**使用的坐标(仓库 / 分支 / 访问凭据)。
type stageSource struct {
	RepoURL      string
	CredentialID string
	Branch       string
}

// resolveStageSource 解析阶段级源码坐标:优先本阶段「拉取源码」(git_source)节点上显式填写的
// 仓库地址 / 分支 / 访问凭据,对应字段留空则回落到项目绑定仓库 / 项目绑定凭据 / 本次 run 的触发分支。
//
// 为什么需要这一层:节点面板把这三个字段呈现为「非空即覆盖项目默认值」(见前端字段提示),
// 但克隆长期只读 project.RepoURL / project.CredentialID / r.Trigger.Branch —— 用户在节点上填的
// 仓库地址、钉住的分支、选中的访问凭据全都被静默忽略(git_source 节点的 credentialId 此前只用于
// 日志里那行「凭据:已绑定」)。于是「私有仓库用哪个凭据」实际由项目绑定单点决定,节点选择形同虚设。
//
// 一个阶段只认第一个 git_source 节点(画布上每个阶段至多一个源节点);与 gitSourceLogLines 的
// 报告口径一致,多声明时后写的忽略。
//
// 注:旧版固定流程(setting PIPEWRIGHT_RUNNER=legacy 时的 Builder.Run)手上没有流水线 spec,
// 因此仍只认项目绑定 —— 该路径不经过本函数。
func resolveStageSource(stage pipeline.Stage, proj *project.Project, r *run.Run) stageSource {
	src := stageSource{}
	if r != nil {
		src.Branch = strings.TrimSpace(r.Trigger.Branch)
	}
	if proj != nil {
		src.RepoURL = strings.TrimSpace(proj.RepoURL)
		src.CredentialID = strings.TrimSpace(proj.CredentialID)
	}
	for _, jb := range stage.Jobs {
		if strings.TrimSpace(jb.Type) != "git_source" {
			continue
		}
		if v := cfgString(jb.Config, "repoUrl"); v != "" {
			src.RepoURL = v
		}
		if v := cfgString(jb.Config, "branch"); v != "" {
			src.Branch = v
		}
		if v := cfgString(jb.Config, "credentialId"); v != "" {
			src.CredentialID = v
		}
		break
	}
	return src
}

// stageGitAuth 解析本阶段实际送进克隆的 Git 凭据,返回 (凭据, 是否回落到项目绑定凭据):
//  1. 节点上选的凭据(src.CredentialID,含项目预填的拷贝)能取到 → 用它;
//  2. 取不到(悬空引用:凭据被删除 / 换新)且项目另有绑定 → 回落到项目绑定凭据;
//  3. 都没有 → 空凭据(公开仓库匿名克隆)。
//
// 为什么要有第 2 步:节点上的仓库/分支/凭据在保存时往往被「项目默认值预填」写进 config(见
// pipeline.fillSourceDefaults),这些拷贝在项目换凭据后就变成悬空引用;没有回落兜底的话,
// 私有仓库会静默退化成匿名克隆,报出难懂的「鉴权失败」。回落事实由调用方记日志,来源可见。
func (b *Builder) stageGitAuth(src stageSource, proj *project.Project) (vault.GitAuth, bool) {
	auth, err := b.revealGitAuthErr(src.CredentialID)
	if err == nil {
		return auth, false
	}
	// 未绑任何凭据不算「悬空」:公开仓库本就走匿名克隆,无需回落(也无从回落)。
	if errors.Is(err, errNoGitCredential) {
		return vault.GitAuth{}, false
	}
	if proj == nil {
		return vault.GitAuth{}, false
	}
	projectCred := strings.TrimSpace(proj.CredentialID)
	if projectCred == "" || projectCred == strings.TrimSpace(src.CredentialID) {
		return vault.GitAuth{}, false
	}
	fallback, ferr := b.revealGitAuthErr(projectCred)
	if ferr != nil {
		return vault.GitAuth{}, false
	}
	return fallback, true
}
