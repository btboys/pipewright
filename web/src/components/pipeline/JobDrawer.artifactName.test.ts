import { describe, it, expect } from 'vitest'
import { mount, type DOMWrapper } from '@vue/test-utils'
import JobDrawer from './JobDrawer.vue'
import { t } from '../../i18n'
import type { PipelineJob, PipelineStage } from '../../api/pipeline'

// 部署节点「部署哪个产物」字段:一次构建产出多件同类产物(多个前端 dist)时,用它指定部署哪一件。
// 断言渲染面(下拉选项来自父级传入的声明名)、写回(config.artifactName)、镜像产物下不露出。

const stage: PipelineStage = { id: 's1', name: '部署', kind: 'deploy', jobs: [] }
const NAME_LABEL = t('pipelineJob.fieldArtifactNameLabel')

function deployJob(config: Record<string, string> = {}): PipelineJob {
  return { id: 'd1', name: 'SSH 部署', type: 'deploy_ssh', summary: '', config }
}

/** 取产物名下拉:按字段标签定位,不依赖 select 在表单里的次序。 */
function artifactSelect(host: { findAll: (selector: string) => DOMWrapper<Element>[] }): DOMWrapper<Element> | undefined {
  return host.findAll('select').find((s) => s.attributes('aria-label') === NAME_LABEL)
}

function optionValues(select: DOMWrapper<Element>): (string | undefined)[] {
  return select.findAll('option').map((o) => o.attributes('value'))
}

describe('JobDrawer — deploy artifact name', () => {
  it('lists the declared artifact names as select options (blank = auto)', () => {
    const wrapper = mount(JobDrawer, {
      props: {
        job: deployJob({ serverId: 'srv-1' }),
        stage,
        artifactNames: ['fxy_admin_front', 'ym_client_front'],
      },
    })
    const select = artifactSelect(wrapper)
    expect(select).toBeTruthy()
    expect(optionValues(select!)).toEqual(['', 'fxy_admin_front', 'ym_client_front'])
  })

  it('keeps a configured name that no current node declares (通配/自动命名/旧配置不丢值)', () => {
    const wrapper = mount(JobDrawer, {
      props: {
        job: deployJob({ serverId: 'srv-1', artifactName: 'legacy-name' }),
        stage,
        artifactNames: ['fxy_admin_front'],
      },
    })
    const select = artifactSelect(wrapper)!
    expect(optionValues(select)).toEqual(['', 'legacy-name', 'fxy_admin_front'])
    // 已知是 <select> 节点,读原生 value 断言「选中项 = 配置值」。
    expect((select.element as HTMLSelectElement).value).toBe('legacy-name')
  })

  it('writes the picked name into config.artifactName', async () => {
    const wrapper = mount(JobDrawer, {
      props: {
        job: deployJob({ serverId: 'srv-1' }),
        stage,
        artifactNames: ['fxy_admin_front', 'ym_client_front'],
      },
    })
    const select = artifactSelect(wrapper)!
    ;(select.element as HTMLSelectElement).value = 'ym_client_front'
    await select.trigger('change')

    const last = wrapper.emitted('update')!.at(-1)![0] as Partial<PipelineJob>
    expect(last.config!.artifactName).toBe('ym_client_front')
  })

  it('hides the field for image artifacts (镜像按类型挑即可)', () => {
    const wrapper = mount(JobDrawer, {
      props: {
        job: deployJob({ serverId: 'srv-1', artifactType: 'image' }),
        stage,
        artifactNames: ['fxy_admin_front'],
      },
    })
    expect(artifactSelect(wrapper)).toBeUndefined()
  })
})
