/**
 * Triggers API — aligns to frozen 2.3 contract.
 *
 * GET  /api/projects/{id}/trigger                  → TriggerConfig
 * PUT  /api/projects/{id}/trigger                  → TriggerConfig  (needs CSRF)
 * POST /api/projects/{id}/trigger/secret/reset     → SecretResetResult (needs CSRF)
 * GET  /api/projects/{id}/trigger/deliveries       → { items: TriggerDelivery[] }
 *
 * webhookSecretMasked is always masked (e.g. "whsec_••••a91f").
 * The full plaintext secret is ONLY available in SecretResetResult.webhookSecret,
 * returned once at reset time and never again.
 *
 * targetServerIds: column modeled for forward compat (Story 4-1), not
 * validated for server existence in this story (TODO: enable in 4-1).
 */

import { http } from './http'

// ─── Domain types ─────────────────────────────────────────────────────────────

export interface TriggerEvents {
  push: boolean
  tag: boolean
  pullRequest: boolean
  release: boolean
}

export interface BranchMapping {
  /** Server-assigned id present on existing rows; undefined for new (unsaved) rows. */
  id?: string
  branchPattern: string
  environment: string
  /**
   * TODO (Story 4-1): Once target_servers table exists, validate ids here.
   * For now, the list is accepted as-is without existence checking.
   */
  targetServerIds: string[]
}

export type UnmatchedPolicy = 'record' | 'ignore'

export interface TriggerConfig {
  /** Read-only webhook endpoint URL, e.g. "/api/webhooks/<token>". */
  webhookUrl: string
  /**
   * Masked display, e.g. "whsec_••••a91f".
   * Never contains plaintext.  Copy the full secret from SecretResetResult only.
   */
  webhookSecretMasked: string
  events: TriggerEvents
  branchMappings: BranchMapping[]
  unmatchedPolicy: UnmatchedPolicy
  /** 路径过滤 glob 列表(monorepo · P0);空 = 不启用(放行一切)。 */
  pathFilters: string[]
  /** 自定义 token 校验请求头;空 = 后端按内置回退链自动识别(X-Gitee-Token → X-Codeup-Token → X-Gitlab-Token)。 */
  tokenHeader: string
}

export interface SaveTriggerInput {
  events: TriggerEvents
  branchMappings: Array<{
    branchPattern: string
    environment: string
    targetServerIds: string[]
  }>
  unmatchedPolicy: UnmatchedPolicy
  pathFilters: string[]
  /** 自定义 token 校验请求头;空 = 后端按内置回退链自动识别(X-Gitee-Token → X-Codeup-Token → X-Gitlab-Token)。 */
  tokenHeader: string
}

export interface SecretResetResult {
  /** Full plaintext secret — one-time only, never stored client-side after display. */
  webhookSecret: string
  webhookSecretMasked: string
}

/**
 * One recorded webhook delivery. Every delivery lands here (accepted or not), so the
 * trigger page can show *why* nothing fired — which gate stopped it and on what value —
 * instead of making the operator infer it from the raw HTTP response.
 *
 * `outcome` is `'accepted'` (then `runId` is set) or one of the ignore reasons:
 * `event_not_subscribed` | `no_branch_match` | `path_no_match` | `duplicate` |
 * `unmatched_ignored` | `unmatched_recorded`.
 */
export interface TriggerDelivery {
  id: string
  /** Normalized event name, e.g. `'Push Hook'`; derived from `object_kind` when the
   *  platform sends no event header (云效 Codeup). */
  event: string
  /** Triggered ref: branch name for pushes, tag name for tag/release events. */
  branch: string
  commit: string
  outcome: string
  /** Set only when `outcome === 'accepted'`. */
  runId: string
  createdAt: string
}

// ─── API functions ────────────────────────────────────────────────────────────

export async function getTrigger(projectId: string): Promise<TriggerConfig> {
  return http.get<TriggerConfig>(`/api/projects/${projectId}/trigger`)
}

/** Recent webhook deliveries (newest first). `limit` defaults to 20 server-side (max 100). */
export async function listTriggerDeliveries(
  projectId: string,
  limit = 20,
): Promise<TriggerDelivery[]> {
  const res = await http.get<{ items: TriggerDelivery[] }>(
    `/api/projects/${projectId}/trigger/deliveries?limit=${limit}`,
  )
  return res.items ?? []
}

export async function saveTrigger(
  projectId: string,
  input: SaveTriggerInput,
): Promise<TriggerConfig> {
  return http.put<TriggerConfig>(`/api/projects/${projectId}/trigger`, input)
}

export async function resetSecret(projectId: string): Promise<SecretResetResult> {
  return http.post<SecretResetResult>(`/api/projects/${projectId}/trigger/secret/reset`)
}
