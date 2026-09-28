import { describe, it, expect } from 'vitest'
import { collectArtifactCandidates, slugifyProjectName } from './artifactCandidates'
import type { PipelineStage } from '../../api/pipeline'

// 部署节点产物候选:设计期静态推导(显式命名 / 自动命名 / build_image 镜像),
// 与后端命名规则一致 —— 名字错了部署会「未找到名为 X 的产物」失败,所以这里断言得细一点。

function job(type: string, config: Record<string, string> = {}) {
  return { id: `${type}-id`, name: type, type, summary: '', config }
}

function stages(...jobs: ReturnType<typeof job>[]): PipelineStage[] {
  return [{ id: 's1', name: '构建', kind: 'build', jobs }]
}

describe('slugifyProjectName (镜像产物名 = 项目 slug)', () => {
  it('lowercases and folds non-alphanumerics into single dashes', () => {
    expect(slugifyProjectName('Acme Shop')).toBe('acme-shop')
    expect(slugifyProjectName('  My__App  ')).toBe('my-app')
    expect(slugifyProjectName('a--b')).toBe('a-b')
  })

  it('falls back to "app" when nothing usable remains', () => {
    expect(slugifyProjectName('')).toBe('app')
    expect(slugifyProjectName('   ')).toBe('app')
    expect(slugifyProjectName('前端项目')).toBe('app')
  })
})

describe('collectArtifactCandidates', () => {
  it('collects declared names, auto names and the project image', () => {
    const got = collectArtifactCandidates(
      stages(job('build_frontend', { artifactPath: 'web=frontend/dist' }), job('build_image', {})),
      'Acme Shop',
    )
    expect(got).toEqual([
      { name: 'web', kind: 'file', detail: 'frontend/dist' },
      { name: 'acme-shop', kind: 'image', detail: '' },
    ])
  })

  it('derives the backend auto name (<slug>-<basename>) for unnamed non-wildcard paths', () => {
    const got = collectArtifactCandidates(
      stages(job('build_frontend', { artifactPath: 'frontend/dist' })),
      'acme',
    )
    expect(got).toEqual([{ name: 'acme-dist', kind: 'file', detail: 'frontend/dist' }])
  })

  it('skips unnamed wildcard paths (the real matched filename is unknown at design time)', () => {
    const got = collectArtifactCandidates(
      stages(job('build_backend', { artifactPath: 'backend/target/*.jar\napi=backend/target/*.jar' })),
      'acme',
    )
    expect(got).toEqual([{ name: 'api', kind: 'file', detail: 'backend/target/*.jar' }])
  })

  it('honors a build_image node that emits a file artifact instead of an image', () => {
    const got = collectArtifactCandidates(
      stages(job('build_image', { artifactType: 'jar' }), job('build_image', { artifactType: 'dist' })),
      'acme',
    )
    expect(got).toEqual([
      { name: 'acme', kind: 'file', detail: 'target/*.jar' },
      { name: 'acme-dist', kind: 'file', detail: 'dist/' },
    ])
  })

  it('dedupes by name and keeps declaration order', () => {
    const got = collectArtifactCandidates(
      stages(job('build_frontend', { artifactPath: 'web=frontend/dist\nweb=frontend/dist' })),
      'acme',
    )
    expect(got.map((c) => c.name)).toEqual(['web'])
  })

  it('returns nothing for a pipeline without artifacts, and tolerates a blank project name', () => {
    expect(collectArtifactCandidates(stages(job('git_source')), 'acme')).toEqual([])
    expect(
      collectArtifactCandidates(stages(job('build_image', {})), '').map((c) => c.name),
    ).toEqual(['app'])
  })
})
