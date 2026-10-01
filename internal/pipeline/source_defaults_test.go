package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
)

// storedSpec 直读落库的 spec_json —— 必须绕过 Get(),因为 Get() 会重新预填项目绑定值,
// 恰好把「预填值有没有落库」这件事掩盖掉。
func storedSpec(t *testing.T, db *sql.DB, projectID string) Spec {
	t.Helper()
	var raw string
	if err := db.QueryRow(`SELECT spec_json FROM pipeline_configs WHERE project_id = ?`, projectID).Scan(&raw); err != nil {
		t.Fatalf("read spec_json: %v", err)
	}
	var spec Spec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		t.Fatalf("unmarshal spec_json: %v", err)
	}
	return spec
}

func gitSourceConfig(t *testing.T, spec Spec) map[string]any {
	t.Helper()
	for _, st := range spec.Stages {
		for _, jb := range st.Jobs {
			if jb.Type == "git_source" {
				return jb.Config
			}
		}
	}
	t.Fatal("spec 里没有 git_source 节点")
	return nil
}

func projectSourceBinding(t *testing.T, db *sql.DB, projectID string) (repoURL, branch, credID string) {
	t.Helper()
	if err := db.QueryRow(
		`SELECT repo_url, default_branch, credential_id FROM projects WHERE id = ?`, projectID,
	).Scan(&repoURL, &branch, &credID); err != nil {
		t.Fatalf("read project binding: %v", err)
	}
	return repoURL, branch, credID
}

// 前端保存流程的等价物:Get() 拿到(带预填的)spec → 原样回传 Save()。
// 预填值不得落库,否则项目之后换仓库/换凭据时,节点上的陈旧拷贝会盖住项目绑定。
func TestSaveStripsInheritedSourceDefaults(t *testing.T) {
	svc, db, projID := newSvc(t)
	ctx := context.Background()

	// 先落一份源码字段全空的流水线(等价于用户在画布上刚建好的形态)。
	if _, err := svc.Save(ctx, projID, emptySourceSpec()); err != nil {
		t.Fatalf("Save(empty): %v", err)
	}
	cfg, err := svc.Get(ctx, projID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// 前置条件:预填确实发生了(否则这个测试什么也没验证)。
	filled := gitSourceConfig(t, cfg.Spec)
	if filled["repoUrl"] == nil || filled["credentialId"] == nil {
		t.Fatalf("前置条件不成立:Get 未预填源码字段, got %+v", filled)
	}

	// 前端保存流程的等价物:Get() 拿到(带预填的)spec → 原样回传 Save()。
	if _, err := svc.Save(ctx, projID, cfg.Spec); err != nil {
		t.Fatalf("Save: %v", err)
	}

	storedFull := storedSpec(t, db, projID)
	stored := gitSourceConfig(t, storedFull)
	for _, key := range []string{"repoUrl", "branch", "credentialId"} {
		if v, ok := stored[key]; ok {
			t.Errorf("继承自项目绑定的 %s 落库了(%v),应被清掉", key, v)
		}
	}
	// 摘要仍保留(画布卡片靠它显示源码信息,不清)。
	if storedFull.Stages[0].Jobs[0].Summary == "" {
		t.Error("summary 被一并清掉了,画布卡片会失去源码信息")
	}
}

// emptySourceSpec 造一份「源阶段 + git_source 节点,config 全空」的流水线。
func emptySourceSpec() Spec {
	return Spec{Stages: []Stage{
		{Name: "源码", Kind: KindSource, Jobs: []Job{
			{Name: "拉取", Type: "git_source", Config: map[string]any{}},
		}},
	}}
}

// 清完只是「不落库」:Get() 仍按项目绑定预填供展示,所以画布上照样看得到生效值。
func TestGetRefillsInheritedSourceAfterStrip(t *testing.T) {
	svc, _, projID := newSvc(t)
	ctx := context.Background()

	cfg, _ := svc.Get(ctx, projID)
	if _, err := svc.Save(ctx, projID, cfg.Spec); err != nil {
		t.Fatalf("Save: %v", err)
	}
	again, err := svc.Get(ctx, projID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got := gitSourceConfig(t, again.Spec)
	if got["repoUrl"] != "https://gitee.com/acme/shop.git" || got["branch"] != "main" {
		t.Errorf("Get 未重新预填项目绑定供展示, got repoUrl=%v branch=%v", got["repoUrl"], got["branch"])
	}
}

// 用户显式填的**不同**值是真覆盖,必须原样落库 —— 那是本项目「节点非空即覆盖」的输入。
func TestSaveKeepsExplicitSourceOverrides(t *testing.T) {
	svc, db, projID := newSvc(t)
	ctx := context.Background()

	spec := Spec{Stages: []Stage{
		{Name: "源码", Kind: KindSource, Jobs: []Job{
			{Name: "拉取", Type: "git_source", Config: map[string]any{
				"repoUrl":      "https://codeup.aliyun.com/fxy/other.git",
				"branch":       "release",
				"credentialId": "cred-other",
			}},
		}},
	}}
	if _, err := svc.Save(ctx, projID, spec); err != nil {
		t.Fatalf("Save: %v", err)
	}

	stored := gitSourceConfig(t, storedSpec(t, db, projID))
	want := map[string]string{
		"repoUrl":      "https://codeup.aliyun.com/fxy/other.git",
		"branch":       "release",
		"credentialId": "cred-other",
	}
	for key, wantVal := range want {
		if got, _ := stored[key].(string); got != wantVal {
			t.Errorf("显式覆盖 %s = %q, want %q(被误清)", key, got, wantVal)
		}
	}
}

// 只清与项目相同的那一项,同节点里其余真覆盖不受影响(逐字段判定,不是整节点丢弃)。
func TestSaveStripsOnlyInheritedField(t *testing.T) {
	svc, db, projID := newSvc(t)
	ctx := context.Background()
	repoURL, _, credID := projectSourceBinding(t, db, projID)

	spec := Spec{Stages: []Stage{
		{Name: "源码", Kind: KindSource, Jobs: []Job{
			{Name: "拉取", Type: "git_source", Config: map[string]any{
				"repoUrl":      repoURL, // 与项目相同 → 继承值,应清
				"branch":       "release",
				"credentialId": credID, // 与项目相同 → 继承值,应清
			}},
		}},
	}}
	if _, err := svc.Save(ctx, projID, spec); err != nil {
		t.Fatalf("Save: %v", err)
	}

	stored := gitSourceConfig(t, storedSpec(t, db, projID))
	if _, ok := stored["repoUrl"]; ok {
		t.Error("与项目相同的 repoUrl 应被清掉")
	}
	if _, ok := stored["credentialId"]; ok {
		t.Error("与项目相同的 credentialId 应被清掉")
	}
	if got, _ := stored["branch"].(string); got != "release" {
		t.Errorf("branch = %q, want release(真覆盖被误清)", got)
	}
}

// 非 git_source 节点不受影响(只该动源码节点)。
func TestSaveLeavesOtherJobKindsUntouched(t *testing.T) {
	svc, db, projID := newSvc(t)
	ctx := context.Background()
	repoURL, _, credID := projectSourceBinding(t, db, projID)

	// build_image 节点上凑巧同名的键:不得被源码字段清理逻辑误删。
	spec := Spec{Stages: []Stage{
		{Name: "源码", Kind: KindSource, Jobs: []Job{
			{Name: "拉取", Type: "git_source", Config: map[string]any{}},
		}},
		{Name: "构建", Kind: KindBuild, Jobs: []Job{
			{Name: "构建", Type: "build_image", Config: map[string]any{"repoUrl": repoURL, "credentialId": credID}},
		}},
	}}
	if _, err := svc.Save(ctx, projID, spec); err != nil {
		t.Fatalf("Save: %v", err)
	}

	cfg := storedSpec(t, db, projID).Stages[1].Jobs[0].Config
	if got, _ := cfg["repoUrl"].(string); got != repoURL {
		t.Errorf("非源码节点的 repoUrl 被误清, got %q", got)
	}
	if got, _ := cfg["credentialId"].(string); got != credID {
		t.Errorf("非源码节点的 credentialId 被误清, got %q", got)
	}
}
