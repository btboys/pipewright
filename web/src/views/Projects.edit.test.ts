import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import Projects from './Projects.vue'
import { t } from '../i18n'
import { HttpError } from '../api/http'
import type { Credential } from '../api/credentials'
import type { Project, UpdateProjectInput } from '../api/projects'

// 「编辑项目」弹窗:名称 / 默认分支 / 仓库凭据都可改(此前只能改名称,凭据改不了)。
// 关键契约是**只提交改动过的字段**:PATCH 带 credentialId 会触发后端重做 ls-remote 校验,
// 若改个名字也顺带把凭据塞进去,仓库一时不可达就连名字都改不了。
// 弹窗经 Teleport 挂到 body,故直接查 document。

// ─── module mocks ──────────────────────────────────────────────────────────

const push = vi.fn()
vi.mock('vue-router', () => ({ useRouter: () => ({ push }) }))

const listProjects = vi.fn()
const updateProject = vi.fn()
vi.mock('../api/projects', async () => {
  const actual = await vi.importActual<typeof import('../api/projects')>('../api/projects')
  return {
    ...actual,
    listProjects: (...args: unknown[]) => listProjects(...args),
    updateProject: (...args: unknown[]) => updateProject(...args),
  }
})

const listCredentials = vi.fn()
vi.mock('../api/credentials', async () => {
  const actual = await vi.importActual<typeof import('../api/credentials')>('../api/credentials')
  return {
    ...actual,
    listCredentials: (...args: unknown[]) => listCredentials(...args),
    createCredential: vi.fn(),
  }
})

vi.mock('../api/runs', () => ({ triggerManual: vi.fn() }))
vi.mock('../api/refs', () => ({ listRefs: vi.fn(), listCommits: vi.fn() }))
vi.mock('../api/parameters', () => ({ getParameters: vi.fn(), validateParamValues: vi.fn() }))

// ─── fixtures ──────────────────────────────────────────────────────────────

const PROJECT: Project = {
  id: 'p1',
  name: 'acme-shop',
  repoUrl: 'https://codeup.aliyun.com/fxy/acme-shop.git',
  defaultBranch: 'master',
  credentialId: 'cred-old',
  credentialName: 'codeup-old',
  pacEnabled: false,
  prStatusEnabled: false,
  lastRunStatus: null,
  targetServers: [],
  createdAt: '2026-01-01T00:00:00Z',
  updatedAt: '2026-01-01T00:00:00Z',
}

const CREDENTIALS: Credential[] = [
  { id: 'cred-old', name: 'codeup-old', type: 'git_token', scope: '', username: '', maskedValue: '····old', lastUsedAt: null, createdAt: '2026-01-01T00:00:00Z' },
  { id: 'cred-new', name: 'gitee-https', type: 'git_http', scope: '', username: 'fxy', maskedValue: '····f3e6', lastUsedAt: null, createdAt: '2026-01-01T00:00:00Z' },
]

// ─── helpers ───────────────────────────────────────────────────────────────

/** 弹窗在 Teleport 里,按选择器从 document 取。 */
function el<T extends HTMLElement = HTMLElement>(selector: string): T | null {
  return document.querySelector<T>(selector)
}

function must<T extends HTMLElement = HTMLElement>(selector: string): T {
  const node = el<T>(selector)
  if (!node) throw new Error(`未找到节点: ${selector}`)
  return node
}

function setInput(selector: string, value: string): void {
  const input = must<HTMLInputElement>(selector)
  input.value = value
  input.dispatchEvent(new Event('input'))
}

async function submitModalForm(): Promise<void> {
  const form = must('#edit-name').closest('form')
  if (!form) throw new Error('未找到编辑表单')
  form.dispatchEvent(new Event('submit', { cancelable: true }))
  await flushPromises()
}

async function mountPage() {
  const wrapper = mount(Projects, { attachTo: document.body })
  await flushPromises()
  return wrapper
}

type Page = Awaited<ReturnType<typeof mountPage>>

/** 点开列表行上的「编辑」动作按钮。 */
async function openEditModal(wrapper: Page): Promise<void> {
  const label = t('projects.actionEditAria', { name: PROJECT.name })
  const button = wrapper.findAll('button').find((b) => b.attributes('aria-label') === label)
  if (!button) throw new Error('未找到编辑按钮')
  await button.trigger('click')
  await nextTick()
}

/** 在凭据下拉里选一条(自定义 listbox:先开 trigger,再点 option)。 */
async function pickCredential(name: string): Promise<void> {
  must<HTMLButtonElement>('#edit-cred').click()
  await nextTick()
  const option = [...document.querySelectorAll<HTMLButtonElement>('.credential-select__option')].find((o) =>
    o.textContent?.includes(name),
  )
  if (!option) throw new Error(`凭据选项未渲染: ${name}`)
  option.click()
  await nextTick()
}

function lastPatch(): UpdateProjectInput | undefined {
  return updateProject.mock.calls.at(-1)?.[1] as UpdateProjectInput | undefined
}

// ─── tests ─────────────────────────────────────────────────────────────────

describe('Projects — 编辑项目弹窗', () => {
  beforeEach(() => {
    push.mockReset()
    listProjects.mockReset().mockResolvedValue([PROJECT])
    listCredentials.mockReset().mockResolvedValue(CREDENTIALS)
    updateProject
      .mockReset()
      .mockImplementation((id: string, input: UpdateProjectInput) => Promise.resolve({ ...PROJECT, ...input, id }))
  })

  afterEach(() => {
    document.body.innerHTML = ''
    vi.restoreAllMocks()
  })

  it('回填名称 / 默认分支 / 仓库凭据,并把仓库地址只读展示', async () => {
    const wrapper = await mountPage()
    await openEditModal(wrapper)

    expect(must<HTMLInputElement>('#edit-name').value).toBe('acme-shop')
    expect(must<HTMLInputElement>('#edit-branch').value).toBe('master')
    expect(must('#edit-cred').textContent).toContain('codeup-old')

    // 仓库地址:只读 + 写明创建后不可改(后端 PATCH 不接受 repoUrl)。
    const repo = must<HTMLInputElement>('#edit-repo')
    expect(repo.value).toBe(PROJECT.repoUrl)
    expect(repo.readOnly).toBe(true)
    expect(must('#edit-repo-hint').textContent).toContain('不可修改')

    wrapper.unmount()
  })

  it('只改名称时只提交 name(不夹带凭据,避免白白触发仓库连通性校验)', async () => {
    const wrapper = await mountPage()
    await openEditModal(wrapper)

    setInput('#edit-name', 'acme-shop-v2')
    await submitModalForm()

    expect(updateProject).toHaveBeenCalledTimes(1)
    expect(lastPatch()).toEqual({ name: 'acme-shop-v2' })
    expect(el('#edit-name')).toBeNull() // 保存成功后弹窗关闭
    wrapper.unmount()
  })

  it('换凭据时提交新的 credentialId(下拉里能选到 Git HTTPS 凭据)', async () => {
    const wrapper = await mountPage()
    await openEditModal(wrapper)

    await pickCredential('gitee-https')
    expect(must('#edit-cred').textContent).toContain('gitee-https')

    await submitModalForm()

    expect(lastPatch()).toEqual({ credentialId: 'cred-new' })
    wrapper.unmount()
  })

  it('同时改名称、分支与凭据时三项一起提交', async () => {
    const wrapper = await mountPage()
    await openEditModal(wrapper)

    setInput('#edit-name', 'acme-shop-v2')
    setInput('#edit-branch', 'release')
    await pickCredential('gitee-https')
    await submitModalForm()

    expect(lastPatch()).toEqual({ name: 'acme-shop-v2', defaultBranch: 'release', credentialId: 'cred-new' })
    wrapper.unmount()
  })

  it('没有任何改动时不发请求', async () => {
    const wrapper = await mountPage()
    await openEditModal(wrapper)

    await submitModalForm()

    expect(updateProject).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('名称为空时就地报错且不发请求', async () => {
    const wrapper = await mountPage()
    await openEditModal(wrapper)

    setInput('#edit-name', '   ')
    await submitModalForm()

    expect(updateProject).not.toHaveBeenCalled()
    expect(must('#edit-name-err').textContent).toContain(t('projects.errNameEmpty'))
    wrapper.unmount()
  })

  it('凭据校验失败(credential_error)时错误落在凭据字段上且保留弹窗', async () => {
    updateProject.mockRejectedValueOnce(
      new HttpError(422, { code: 'credential_error', message: 'credential check failed' }, 'credential check failed'),
    )
    const wrapper = await mountPage()
    await openEditModal(wrapper)

    await pickCredential('gitee-https')
    await submitModalForm()

    expect(el('#edit-name')).not.toBeNull() // 弹窗仍在,便于换一条凭据重试
    expect(must('#edit-cred-err').textContent).toContain(t('projects.editErrCredField'))
    expect(document.body.textContent).toContain(t('projects.editErrCredBanner'))
    wrapper.unmount()
  })
})
