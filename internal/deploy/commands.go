package deploy

import (
	"path"
	"strings"

	"github.com/btboys/pipewright/internal/run"
)

// commands.go 保留部署侧共用的**路径 / 名称净化 helper**。
//
// 各产物类型的部署命令已全部由专门编排构造,不再有「按 type 拼扁平命令」的分支:
//   - dist / jar / archive → release.go:发布目录 + current 软链原子切换(只放置产物,**部署期
//     不启动进程**,尤其绝不 `java -jar` —— 胖 jar 加 `--version` 会真启动应用并阻塞到超时;
//     启动/重载交给 restartCommand,健康门控裁决成败)。
//   - image → image_release.go:docker pull → 停旧起新 → 健康门控 → 失败回滚上一镜像。
//
// 命令一律 array 化([]string)经 target.Exec 执行,绝不在平台侧拼 shell(AC-SEC-02)。

// defaultDeployRoot 是未在 Config 指定路径时的部署根目录(本机真验友好:用临时区,不需 root)。
const defaultDeployRoot = "/tmp/pipewright-deploy"

// sanitizeName 把产物名净化为安全目录段(仅字母数字 . _ -;其余替为 _)。
func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// deployFileName 取产物落地文件名(reference 的 base 名;无则用净化产物名 + .bin)。
func deployFileName(a run.Artifact) string {
	base := path.Base(strings.TrimRight(a.Reference, "/"))
	base = sanitizeName(base)
	if base == "" || base == "." {
		base = sanitizeName(a.Name) + ".bin"
	}
	return base
}
