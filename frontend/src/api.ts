export type Proxy = {
  id: string
  name: string
  host: string
  port: number
  enabled: boolean
  username: string
  has_password: boolean
  udp_capability: 'unknown' | 'supported' | 'unsupported'
  udp_status: 'unknown' | 'supported' | 'unsupported'
  status: 'unknown' | 'healthy' | 'suspect' | 'unavailable'
  consecutive_failures: number
  last_checked_at: string
  last_result: string
  last_error: string
  next_check_at: string
}

export type Provider = {
  id: string
  type: string
  protocol: 'socks5' | 'http'
  enabled: boolean
  proxies: Proxy[]
}

export type Dashboard = {
  config_revision: string
  selection_revision: string
  selection: {
    provider_id: string
    proxy_id: string
    auto_switch: boolean
    selected_at: string
    switch_reason: string
  }
  http_selection_revision: string
  http_selection: {
    provider_id: string
    proxy_id: string
    auto_switch: boolean
    selected_at: string
    switch_reason: string
  }
  providers: Provider[]
}

export class ApiError extends Error {
  constructor(public status: number, message: string) {
    super(message)
  }
}

export async function request(
  method: string,
  path: string,
  revision?: string,
  body?: object,
): Promise<Dashboard> {
  const response = await fetch(path, {
    method,
    headers: {
      ...(revision ? { 'If-Match': `"${revision}"` } : {}),
      ...(body ? { 'Content-Type': 'application/json' } : {}),
    },
    body: body ? JSON.stringify(body) : undefined,
    cache: 'no-store',
  })
  const payload = await response.json().catch(() => ({}))
  if (!response.ok) {
    throw new ApiError(response.status, payload.error || `请求失败（${response.status}）`)
  }
  return payload as Dashboard
}
