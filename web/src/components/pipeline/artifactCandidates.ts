/**
 * artifactCandidates.ts —— 部署节点「部署产物」选择器的候选清单(设计期静态推导)。
 *
 * 部署节点不再让用户选「产物类型」:候选直接列出本流水线**会产出的产物**,选哪件就部署哪件,
 * 产物类型由候选自身决定(镜像 / 文件)。后端 deploy.DeployForStage 也只看名字(cfg["artifactName"]),
 * 按名精确匹配 run 里的真实产物;未选 / 名字不存在 = 诚实失败,绝不替用户猜一件。
 *
 * 候选来源与后端产物命名规则一一对应(见 internal/build/dag_stage_exec.go / builder.go):
 *
 *   1. 脚本类节点的 artifactPath 声明(`[名称=]路径`,每行一条):
 *      - 显式命名(`web=frontend/dist`)→ 直接用该名。
 *      - 未命名(`frontend/dist`)→ 后端自动命名 `<项目slug>-<路径基名>`;通配(`*.jar`)的真实
 *        匹配文件名设计期算不出来,故不入候选(可在「高级参数」里手填)。
 *   2. build_image 节点 → 镜像产物,名称 = 项目名 slug(builder.go 里 Name: slug);
 *      若该节点 artifactType 显式写成 jar/dist,则按文件产物命名(slug / <slug>-dist)。
 *
 * 候选是**设计期预测**:构建真的没产出该产物时,部署会以「未找到名为 X 的产物(可部署产物:…)」
 * 失败 —— 比悄悄发错产物上线安全。
 */
import type { PipelineStage } from '../../api/pipeline'
import { splitArtifactLine } from './stepCompile'

export interface ArtifactCandidate {
  /** 产物名(写进 config.artifactName;后端按此精确匹配) */
  name: string
  kind: 'file' | 'image'
  /** 展示用说明:镜像 → 空(渲染为「镜像」标签);文件 → 声明的路径 */
  detail: string
}

/**
 * 项目名 → 镜像产物名。规则与 internal/build/builder.go 的 slugify 一致:
 * 小写、仅保留 a-z0-9、其余字符折成单个 `-`、首尾 `-` 去掉、空 → "app"。
 */
export function slugifyProjectName(name: string): string {
  let out = ''
  let prevDash = false
  for (const ch of name.trim().toLowerCase()) {
    if (/^[a-z0-9]$/.test(ch)) {
      out += ch
      prevDash = false
    } else if (!prevDash && out !== '') {
      out += '-'
      prevDash = true
    }
  }
  return out.replace(/^-+|-+$/g, '') || 'app'
}

/** 路径的基名(`frontend/dist` → `dist`;`backend/target/*.jar` → `*.jar`) */
function pathBase(p: string): string {
  const s = p.replace(/[/\\]+$/, '')
  const i = Math.max(s.lastIndexOf('/'), s.lastIndexOf('\\'))
  return i >= 0 ? s.slice(i + 1) : s
}

/** 通配路径的真实匹配文件名设计期未知 → 无法预测自动产物名。 */
function hasWildcard(p: string): boolean {
  return p.includes('*') || p.includes('?') || p.includes('[')
}

/**
 * 收集部署节点的产物候选(去重,保持流水线声明顺序)。
 * projectName 用于推导镜像名与自动命名(`<slug>-<基名>`);缺省时空串(自动命名退化为 `-<基名>`)。
 */
export function collectArtifactCandidates(
  stages: PipelineStage[],
  projectName: string,
): ArtifactCandidate[] {
  const slug = slugifyProjectName(projectName)
  const out: ArtifactCandidate[] = []
  const seen = new Set<string>()
  const push = (name: string, kind: ArtifactCandidate['kind'], detail: string): void => {
    const n = name.trim()
    if (!n || seen.has(n)) return
    seen.add(n)
    out.push({ name: n, kind, detail })
  }

  for (const stage of stages) {
    for (const job of stage.jobs) {
      const cfg = job.config ?? {}
      // 1) 脚本类节点的 artifactPath 声明(每行 `[名称=]路径`)。
      for (const line of (cfg.artifactPath ?? '').replace(/\r/g, '').split('\n')) {
        if (!line.trim()) continue
        const { name, path } = splitArtifactLine(line)
        if (name) {
          // 显式命名:构建多条通配匹配时后端会给 `名称-<基名>` 加后缀,设计期仍以声明名入候选。
          push(name, 'file', path)
          continue
        }
        // 未命名:后端自动命名 <slug>-<路径基名>;通配无法预测实际文件名,跳过。
        const base = pathBase(path)
        if (!hasWildcard(path) && base) push(`${slug}-${base}`, 'file', path)
      }
      // 2) build_image 节点:默认产镜像(Name = slug);显式写 jar/dist 则按文件产物命名。
      if (job.type === 'build_image') {
        const t = (cfg.artifactType ?? '').trim()
        if (t === 'jar') push(slug, 'file', 'target/*.jar')
        else if (t === 'dist') push(`${slug}-dist`, 'file', 'dist/')
        else push(slug, 'image', '')
      }
    }
  }
  return out
}
