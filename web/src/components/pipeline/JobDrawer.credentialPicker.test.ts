import { describe, it, expect } from 'vitest'
import { mount, type DOMWrapper } from '@vue/test-utils'
import JobDrawer from './JobDrawer.vue'
import { t } from '../../i18n'
import type { Credential } from '../../api/credentials'
import type { PipelineJob, PipelineStage } from '../../api/pipeline'

// 「拉取源码」节点的访问凭据下拉:Git 令牌与 Git HTTPS(账号密码 / PAT)都是合法的仓库凭据,
// 两者都必须出现在候选里(曾把候选限死 git_token,Git HTTPS 凭据根本选不到)。
// 断言渲染面:候选 = 传入凭据中类型匹配的那些;选中后写回 config.credentialId。

const stage: PipelineStage = { id: 'stg_src', name: '流水线源', kind: 'source', jobs: [] }
const LABEL = t('pipelineJob.fieldCredentialIdLabel')

const CREDENTIALS: Credential[] = [
  { id: 'cred-token', name: 'gitee-token', type: 'git_token', scope: '', username: '', maskedValue: '····aaaa', lastUsedAt: null, createdAt: '2026-01-01T00:00:00Z' },
  { id: 'cred-http', name: 'codeup-account', type: 'git_http', scope: '', username: 'fxy', maskedValue: '····f3e6', lastUsedAt: null, createdAt: '2026-01-01T00:00:00Z' },
  { id: 'cred-ssh', name: 'deploy-key', type: 'ssh_key', scope: '', username: '', maskedValue: '····bbbb', lastUsedAt: null, createdAt: '2026-01-01T00:00:00Z' },
]

function sourceJob(config: Record<string, string> = {}): PipelineJob {
  return { id: 'job_src', name: '拉取源码', type: 'git_source', summary: '', config }
}

/** 取访问凭据下拉:按字段标签定位,不依赖 select 在表单里的次序。 */
function credentialSelect(
  host: { findAll: (selector: string) => DOMWrapper<Element>[] },
): DOMWrapper<Element> | undefined {
  return host.findAll('select').find((s) => s.attributes('aria-label') === LABEL)
}

function optionValues(select: DOMWrapper<Element>): (string | undefined)[] {
  return select.findAll('option').map((o) => o.attributes('value'))
}

function mountDrawer(config: Record<string, string> = {}) {
  return mount(JobDrawer, {
    props: { job: sourceJob(config), stage, credentials: CREDENTIALS },
  })
}

describe('JobDrawer — git_source credential picker', () => {
  it('offers both Git token and Git HTTPS credentials', () => {
    const select = credentialSelect(mountDrawer())
    expect(select).toBeTruthy()
    expect(optionValues(select!)).toEqual(['', 'cred-token', 'cred-http'])
  })

  it('hides credentials that cannot authenticate a git clone (SSH / registry)', () => {
    const values = optionValues(credentialSelect(mountDrawer())!)
    expect(values).not.toContain('cred-ssh')
  })

  it('shows the picked Git HTTPS credential name and mask', () => {
    const select = credentialSelect(mountDrawer({ credentialId: 'cred-http' }))!
    expect((select.element as HTMLSelectElement).value).toBe('cred-http')
    expect(select.findAll('option').map((o) => o.text()).join('|')).toContain('codeup-account')
  })

  it('writes the picked credential into config.credentialId', async () => {
    const wrapper = mountDrawer()
    const select = credentialSelect(wrapper)!
    ;(select.element as HTMLSelectElement).value = 'cred-http'
    await select.trigger('change')

    const last = wrapper.emitted('update')!.at(-1)![0] as Partial<PipelineJob>
    expect(last.config!.credentialId).toBe('cred-http')
  })

  it('does not clear a configured credential that is missing from the candidate list', () => {
    // 凭据未加载完 / 已删除:已保存的 credentialId 不能被静默清空(仅下拉显示为空)。
    const wrapper = mountDrawer({ credentialId: 'cred-gone' })
    const patches = (wrapper.emitted('update') ?? []).map((e) => e[0] as Partial<PipelineJob>)
    expect(patches.every((p) => p.config?.credentialId !== '')).toBe(true)
  })
})
