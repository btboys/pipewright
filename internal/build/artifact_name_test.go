package build

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/btboys/pipewright/internal/pipeline"
	"github.com/btboys/pipewright/internal/run"
)

// splitArtifactName 的边界:`名称=路径` 才拆;合法名称之外的 `=` 一律归路径,旧格式语义不变。
func TestSplitArtifactName(t *testing.T) {
	cases := []struct {
		line string
		name string
		path string
	}{
		{"frontend/dist", "", "frontend/dist"},                  // 旧格式:纯路径
		{"web-dist=frontend/dist", "web-dist", "frontend/dist"}, // 自定义名称
		{" web-dist = frontend/dist ", "web-dist", "frontend/dist"},
		{"{{app}}-dist=dist", "{{app}}-dist", "dist"}, // 名称可参数化
		{"a/b=x", "", "a/b=x"},                        // 名称含路径分隔 → 整行是路径
		{"*.jar=target", "", "*.jar=target"},          // 名称含通配 → 整行是路径
		{"=dist", "", "=dist"},                        // 空名称 → 整行是路径
		{"web-dist=", "", "web-dist="},                // 空路径 → 整行是路径
		{"", "", ""},                                  // 空行
	}
	for _, c := range cases {
		name, path := splitArtifactName(c.line)
		if name != c.name || path != c.path {
			t.Errorf("splitArtifactName(%q) = (%q,%q), want (%q,%q)", c.line, name, path, c.name, c.path)
		}
	}
}

// 声明了名称的产物用该名称;未声明的仍自动命名为 slug-<路径基名>;命名通配多命中 → 附基名区分。
func TestCollectScriptArtifactsCustomName(t *testing.T) {
	b, _ := newStoreBuilder(t)
	ws := t.TempDir()
	mkFile := func(rel, body string) {
		t.Helper()
		p := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	mkFile("frontend/dist/index.html", "<html>")
	mkFile("backend/target/a.jar", "A")
	mkFile("backend/target/b.jar", "B")

	job := pipeline.Job{ID: "j1", Name: "构建", Type: pipeline.StepTypeScript, Config: map[string]any{
		"artifactPath": "web-dist=frontend/dist\nbackend/target/*.jar\npkg=backend/target/*.jar",
	}}
	rep := &fakeReporter{}
	b.collectScriptArtifacts(context.Background(), []pipeline.Job{job}, ws, "proj", "构建", rep)

	byName := map[string]run.Artifact{}
	for _, a := range rep.arts {
		byName[a.Name] = a
	}
	if len(rep.arts) != 5 {
		t.Fatalf("应产出 5 件产物(1 dist + 2 未命名 jar + 2 命名 jar),got %d:%v", len(rep.arts), rep.arts)
	}
	// 自定义名称 + 真字节仍归档(stored=true,reference=制品库句柄)。
	named, ok := byName["web-dist"]
	if !ok {
		t.Fatalf("dist 应命名为 web-dist,got %v", byName)
	}
	if named.Metadata["stored"] != true {
		t.Errorf("命名不改归档语义,dist 应 stored=true,metadata=%v", named.Metadata)
	}
	// 未声明名称:自动 slug-<基名>。
	for _, want := range []string{"proj-a.jar", "proj-b.jar"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("未命名产物应自动命名 %s,got %v", want, byName)
		}
	}
	// 命名通配多命中:名称 + 基名,避免同名不可辨。
	for _, want := range []string{"pkg-a.jar", "pkg-b.jar"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("命名通配多命中应拆名 %s,got %v", want, byName)
		}
	}
}
