import { describe, it, expect } from 'vitest'
import { mount, type DOMWrapper } from '@vue/test-utils'
import JobDrawer from './JobDrawer.vue'
import { t } from '../../i18n'
import type { ArtifactCandidate } from './artifactCandidates'
import type { PipelineJob, PipelineStage } from '../../api/pipeline'

// 部署节点「部署产物」字段:产物类型不再由用户选 —— 候选直接列出本流水线会产出的产物
// (文件产物 + build_image 的项目镜像),选哪件就部署哪件,镜像/文件专属字段随之切换。
// 断言渲染面(候选选项 = 父级传入的候选)、写回(config.artifactName)、字段随所选产物类型切换。

const stage: PipelineStage = { id: 's1', name: '部署', kind: 'deploy', jobs: [] }
const NAME_LABEL = t('pipelineJob.fieldArtifactNameLabel')

const CANDIDATES: ArtifactCandidate[] = [
  { name: 'web', kind: 'file', detail: 'frontend/dist' },
  { name: 'ym_client_front', kind: 'file', detail: 'ym/dist' },
  { name: 'acme-shop', kind: 'image', detail: '' },
]

function deployJob(config: Record<string, string> = {}): PipelineJob {
  return { id: 'd1', name: 'SSH 部署', type: 'deploy_ssh', summary: '', config }
}

/** 取产物下拉:按字段标签定位,不依赖 select 在表单里的次序。 */
function artifactSelect(host: { findAll: (selector: string) => DOMWrapper<Element>[] }): DOMWrapper<Element> | undefined {
  return host.findAll('select').find((s) => s.attributes('aria-label') === NAME_LABEL)
}

function optionValues(select: DOMWrapper<Element>): (string | undefined)[] {
  return select.findAll('option').map((o) => o.attributes('value'))
}

/** 表单里出现的字段标签(据此断言某个字段是否露出)。 */
function fieldLabels(host: { findAll: (selector: string) => DOMWrapper<Element>[] }): string[] {
  return host
    .findAll('input, textarea, select')
    .map((el) => el.attributes('aria-label') ?? '')
    .filter(Boolean)
}

function mountDrawer(config: Record<string, string>, artifacts: ArtifactCandidate[] = CANDIDATES) {
  return mount(JobDrawer, { props: { job: deployJob(config), stage, artifacts } })
}

describe('JobDrawer — deploy artifact picker', () => {
  it('lists the pipeline artifacts as select options (blank = not chosen)', () => {
    const wrapper = mountDrawer({ serverId: 'srv-1' })
    const select = artifactSelect(wrapper)
    expect(select).toBeTruthy()
    expect(optionValues(select!)).toEqual(['', 'web', 'ym_client_front', 'acme-shop'])
  })

  it('labels image candidates so the project image is recognizable', () => {
    const wrapper = mountDrawer({ serverId: 'srv-1' })
    const texts = artifactSelect(wrapper)!.findAll('option').map((o) => o.text())
    const tag = t('pipelineJob.deployArtifactImageTag')
    expect(texts.some((x) => x.includes('acme-shop') && x.includes(tag))).toBe(true)
  })

  it('keeps a configured name that no current node declares (通配/自动命名/旧配置不丢值)', () => {
    const wrapper = mountDrawer({ serverId: 'srv-1', artifactName: 'legacy-name' })
    const select = artifactSelect(wrapper)!
    expect(optionValues(select)).toEqual(['', 'legacy-name', 'web', 'ym_client_front', 'acme-shop'])
    // 已知是 <select> 节点,读原生 value 断言「选中项 = 配置值」。
    expect((select.element as HTMLSelectElement).value).toBe('legacy-name')
  })

  it('writes the picked name into config.artifactName', async () => {
    const wrapper = mountDrawer({ serverId: 'srv-1' })
    const select = artifactSelect(wrapper)!
    ;(select.element as HTMLSelectElement).value = 'ym_client_front'
    await select.trigger('change')

    const last = wrapper.emitted('update')!.at(-1)![0] as Partial<PipelineJob>
    expect(last.config!.artifactName).toBe('ym_client_front')
  })

  it('shows file-specific fields when a file artifact is selected', () => {
    const labels = fieldLabels(mountDrawer({ serverId: 'srv-1', artifactName: 'web' }))
    expect(labels).toContain(t('pipelineJob.fieldDeployPathLabel'))
    expect(labels).toContain(t('pipelineJob.fieldRestartCommandLabel'))
    expect(labels).not.toContain(t('pipelineJob.fieldContainerNameLabel'))
    expect(labels).not.toContain(t('pipelineJob.fieldPortsLabel'))
  })

  it('shows container-specific fields when the project image is selected', () => {
    const labels = fieldLabels(mountDrawer({ serverId: 'srv-1', artifactName: 'acme-shop' }))
    expect(labels).toContain(t('pipelineJob.fieldContainerNameLabel'))
    expect(labels).toContain(t('pipelineJob.fieldPortsLabel'))
    expect(labels).not.toContain(t('pipelineJob.fieldDeployPathLabel'))
    expect(labels).not.toContain(t('pipelineJob.fieldRestartCommandLabel'))
  })

  it('falls back to file fields while nothing is chosen yet', () => {
    const labels = fieldLabels(mountDrawer({ serverId: 'srv-1' }))
    expect(labels).toContain(t('pipelineJob.fieldDeployPathLabel'))
    expect(labels).not.toContain(t('pipelineJob.fieldContainerNameLabel'))
  })
})
