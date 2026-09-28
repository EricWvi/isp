import { useCallback, useEffect, useState } from 'react'
import { ArrowRightLeft, Pencil, Plus, RefreshCw, Trash2 } from 'lucide-react'
import { ApiError, request, type Dashboard, type Provider, type Proxy } from './api'
import { ProxyDialog } from './ProxyDialog'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardAction, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Switch } from '@/components/ui/switch'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import {
  AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent,
  AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle,
} from '@/components/ui/alert-dialog'

type Editor = { providerId: string; proxy: Proxy | null; revision: string }
type Danger = { kind: 'delete' | 'disable'; providerId: string; proxy: Proxy; revision: string }

const statusLabels: Record<Proxy['status'], string> = {
  unknown: '待检测', healthy: '可用', suspect: '疑似故障', unavailable: '不可用',
}
const reasonLabels: Record<string, string> = {
  startup: '启动时选择', 'health-available': '健康检测后选择',
  'manual-select': '手动选择', 'manual-rotate': '手动轮换',
  'automatic-failover': '自动故障切换', 'configuration-changed': '配置变更',
  'startup-invalid': '原代理已移除', 'startup-no-candidate': '没有可用候选',
}

function timeLabel(value: string) {
  if (!value) return '—'
  const parsed = new Date(value)
  if (Number.isNaN(parsed.getTime())) return value
  return new Intl.DateTimeFormat('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }).format(parsed)
}

function StatusBadge({ status }: { status: Proxy['status'] }) {
  const variant = status === 'unavailable' ? 'destructive' : status === 'healthy' ? 'default' : 'secondary'
  return <Badge variant={variant}>{statusLabels[status]}</Badge>
}

export default function App() {
  const [dashboard, setDashboard] = useState<Dashboard | null>(null)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [editor, setEditor] = useState<Editor | null>(null)
  const [danger, setDanger] = useState<Danger | null>(null)

  const refresh = useCallback(async (clearError = false) => {
    try {
      setDashboard(await request('GET', '/api/state'))
      if (clearError) setError('')
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : '无法读取服务状态')
    }
  }, [])

  useEffect(() => {
    void refresh()
    const timer = window.setInterval(() => { void refresh() }, 10000)
    return () => window.clearInterval(timer)
  }, [refresh])

  async function mutate(method: string, path: string, revision: string, body?: object): Promise<boolean> {
    setBusy(true)
    setError('')
    try {
      setDashboard(await request(method, path, revision, body))
      return true
    } catch (cause) {
      const apiError = cause instanceof ApiError ? cause : null
      setError(apiError?.status === 409 ? `状态已变化，请确认最新信息后重试。${apiError.message}` : cause instanceof Error ? cause.message : '操作失败')
      if (apiError?.status === 409) void refresh()
      return false
    } finally {
      setBusy(false)
    }
  }

  const currentProvider = dashboard?.providers.find(group => group.id === dashboard.selection.provider_id)
  const current = currentProvider?.proxies.find(proxy => proxy.id === dashboard?.selection.proxy_id)
  const healthyCount = dashboard?.providers.reduce((count, group) => count + group.proxies.filter(proxy => group.enabled && proxy.enabled && proxy.status === 'healthy').length, 0) ?? 0
  const proxyCount = dashboard?.providers.reduce((count, group) => count + group.proxies.length, 0) ?? 0

  async function saveProxy(value: Record<string, unknown>): Promise<boolean> {
    if (!editor) return false
    const providerPath = encodeURIComponent(editor.providerId)
    const path = editor.proxy
      ? `/api/providers/${providerPath}/proxies/${encodeURIComponent(editor.proxy.id)}`
      : `/api/providers/${providerPath}/proxies`
    return mutate(editor.proxy ? 'PUT' : 'POST', path, editor.revision, value)
  }

  async function confirmDanger() {
    if (!danger) return
    const base = `/api/providers/${encodeURIComponent(danger.providerId)}/proxies/${encodeURIComponent(danger.proxy.id)}`
    if (danger.kind === 'delete') await mutate('DELETE', base, danger.revision)
    else await mutate('PATCH', `${base}/enabled`, danger.revision, { enabled: false })
    setDanger(null)
  }

  function proxyActions(group: Provider, proxy: Proxy) {
    const selected = dashboard?.selection.provider_id === group.id && dashboard.selection.proxy_id === proxy.id
    return (
      <div className="flex items-center justify-end gap-1">
        <Button variant="ghost" size="sm" disabled={busy || !proxy.enabled || selected || !group.enabled} onClick={() => void mutate('PUT', '/api/selection', dashboard!.selection_revision, { provider_id: group.id, proxy_id: proxy.id })}>选择</Button>
        <Button variant="ghost" size="icon-sm" aria-label={`编辑 ${proxy.name || proxy.id}`} disabled={busy} onClick={() => setEditor({ providerId: group.id, proxy, revision: dashboard!.config_revision })}><Pencil /></Button>
        <Button variant="ghost" size="sm" disabled={busy} onClick={() => {
          if (proxy.enabled && selected) setDanger({ kind: 'disable', providerId: group.id, proxy, revision: dashboard!.config_revision })
          else void mutate('PATCH', `/api/providers/${encodeURIComponent(group.id)}/proxies/${encodeURIComponent(proxy.id)}/enabled`, dashboard!.config_revision, { enabled: !proxy.enabled })
        }}>{proxy.enabled ? '停用' : '启用'}</Button>
        <Button variant="ghost" size="icon-sm" aria-label={`删除 ${proxy.name || proxy.id}`} disabled={busy} onClick={() => setDanger({ kind: 'delete', providerId: group.id, proxy, revision: dashboard!.config_revision })}><Trash2 /></Button>
      </div>
    )
  }

  return (
    <main className="mx-auto flex min-h-screen max-w-6xl flex-col gap-6 px-4 py-8 sm:px-6 lg:px-8">
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <p className="text-sm font-medium text-muted-foreground">本地代理管理</p>
          <h1 className="mt-1 font-heading text-3xl font-semibold tracking-tight">IP 池服务</h1>
          <p className="mt-2 text-sm text-muted-foreground">一个全局当前代理，切换只影响新连接。</p>
        </div>
        <Button variant="outline" disabled={busy} onClick={() => void refresh(true)}><RefreshCw />刷新状态</Button>
      </header>

      {error && <div role="alert" className="rounded-lg border border-destructive/30 bg-destructive/5 px-4 py-3 text-sm text-destructive">{error}</div>}

      {!dashboard ? <Card><CardContent className="py-8 text-center text-muted-foreground">正在读取服务状态…</CardContent></Card> : <>
        <section className="grid gap-4 md:grid-cols-[minmax(0,1.6fr)_minmax(16rem,1fr)]">
          <Card>
            <CardHeader>
              <CardTitle>当前代理</CardTitle>
              <CardDescription>所有新建 TCP 连接使用同一代理</CardDescription>
              {current && <CardAction><StatusBadge status={current.status} /></CardAction>}
            </CardHeader>
            <CardContent className="space-y-4">
              {current ? <>
                <div><p className="font-heading text-xl font-semibold">{current.name || current.id}</p><p className="mt-1 font-mono text-sm text-muted-foreground">{current.host}:{current.port}</p></div>
                <div className="grid gap-3 text-sm sm:grid-cols-2">
                  <div><span className="text-muted-foreground">Provider</span><p className="mt-1">{currentProvider?.id}</p></div>
                  <div><span className="text-muted-foreground">选中时间</span><p className="mt-1">{timeLabel(dashboard.selection.selected_at)}</p></div>
                  <div><span className="text-muted-foreground">切换原因</span><p className="mt-1">{reasonLabels[dashboard.selection.switch_reason] || dashboard.selection.switch_reason || '—'}</p></div>
                  <div><span className="text-muted-foreground">健康状态</span><p className="mt-1">{statusLabels[current.status]}</p></div>
                </div>
                {current.last_error && <p className="rounded-md bg-muted px-3 py-2 text-sm text-muted-foreground">最近错误：{current.last_error}</p>}
              </> : <div className="rounded-lg border border-dashed p-5 text-sm text-muted-foreground">当前没有选中的代理。健康检测找到候选后会自动选择，也可在下方手动选择。</div>}
            </CardContent>
          </Card>
          <Card>
            <CardHeader><CardTitle>切换控制</CardTitle><CardDescription>{healthyCount} 个健康候选 · 共 {proxyCount} 个代理</CardDescription></CardHeader>
            <CardContent className="space-y-5">
              <div className="flex items-center justify-between gap-4 rounded-lg border p-3"><div><p className="font-medium">自动故障切换</p><p className="mt-1 text-xs text-muted-foreground">确认当前代理不可用后切换</p></div><Switch aria-label="自动故障切换" checked={dashboard.selection.auto_switch} disabled={busy} onCheckedChange={checked => void mutate('PUT', '/api/auto-switch', dashboard.selection_revision, { enabled: checked })} /></div>
              <Button className="w-full" variant="outline" disabled={busy} onClick={() => void mutate('POST', '/api/rotate', dashboard.selection_revision)}><ArrowRightLeft />手动轮换到下一个可用代理</Button>
              {current?.status === 'unavailable' && <p className="text-sm text-destructive">当前代理不可用，新连接会失败。请手动选择或等待自动切换。</p>}
              {healthyCount === 0 && <p className="text-sm text-muted-foreground">当前没有检测确认可用的候选代理。</p>}
            </CardContent>
          </Card>
        </section>

        <section className="space-y-4">
          <div><h2 className="font-heading text-xl font-semibold">Provider 与代理</h2><p className="mt-1 text-sm text-muted-foreground">检测状态每 10 秒更新。编辑连接信息后将重新检测。</p></div>
          {dashboard.providers.length === 0 && <Card><CardContent className="py-8 text-center text-muted-foreground">还没有配置 Provider。请先在 YAML 中添加 Provider。</CardContent></Card>}
          {dashboard.providers.map(group => <Card key={group.id}>
            <CardHeader>
              <CardTitle className="flex items-center gap-2">{group.id}<Badge variant={group.enabled ? 'secondary' : 'outline'}>{group.enabled ? '已启用' : '已停用'}</Badge></CardTitle>
              <CardDescription>{group.type} · {group.proxies.length} 个代理</CardDescription>
              <CardAction><Button size="sm" variant="outline" disabled={busy} onClick={() => setEditor({ providerId: group.id, proxy: null, revision: dashboard.config_revision })}><Plus />新增代理</Button></CardAction>
            </CardHeader>
            <CardContent>
              {group.proxies.length === 0 ? <p className="py-6 text-center text-sm text-muted-foreground">尚无代理</p> : <div className="overflow-x-auto"><Table>
                <TableHeader><TableRow><TableHead>代理</TableHead><TableHead>状态</TableHead><TableHead>最近检测</TableHead><TableHead>连续失败</TableHead><TableHead>下次检测</TableHead><TableHead>启停</TableHead><TableHead className="text-right">操作</TableHead></TableRow></TableHeader>
                <TableBody>{group.proxies.map(proxy => {
                  const selected = dashboard.selection.provider_id === group.id && dashboard.selection.proxy_id === proxy.id
                  return <TableRow key={proxy.id} data-state={selected ? 'selected' : undefined}>
                    <TableCell><div className="flex items-center gap-2"><span className="font-medium">{proxy.name || proxy.id}</span>{selected && <Badge variant="outline">当前</Badge>}</div><p className="mt-1 font-mono text-xs text-muted-foreground">{proxy.host}:{proxy.port}</p><p className="mt-1 text-xs text-muted-foreground">{proxy.id}</p></TableCell>
                    <TableCell><StatusBadge status={proxy.status} />{proxy.last_error && <p title={proxy.last_error} className="mt-1 max-w-40 truncate text-xs text-destructive">{proxy.last_error}</p>}</TableCell>
                    <TableCell>{timeLabel(proxy.last_checked_at)}</TableCell><TableCell>{proxy.consecutive_failures}</TableCell><TableCell>{timeLabel(proxy.next_check_at)}</TableCell><TableCell>{proxy.enabled ? '启用' : '停用'}</TableCell>
                    <TableCell>{proxyActions(group, proxy)}</TableCell>
                  </TableRow>
                })}</TableBody>
              </Table></div>}
            </CardContent>
          </Card>)}
        </section>
      </>}

      {editor && <ProxyDialog key={`${editor.providerId}/${editor.proxy?.id || 'new'}`} providerId={editor.providerId} proxy={editor.proxy} open={true} busy={busy} onOpenChange={open => { if (!open) setEditor(null) }} onSave={saveProxy} />}
      <AlertDialog open={!!danger} onOpenChange={open => { if (!open) setDanger(null) }}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{danger?.kind === 'delete' ? '删除代理？' : '停用当前代理？'}</AlertDialogTitle>
            <AlertDialogDescription>{danger?.kind === 'delete' ? `删除 ${danger?.proxy.name || danger?.proxy.id} 后，已有连接会继续使用原链路，新连接将按当前切换设置处理。` : '停用当前代理后，新连接将按自动切换设置选择替代代理或直接失败。'}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter><AlertDialogCancel>取消</AlertDialogCancel><AlertDialogAction variant="destructive" onClick={() => void confirmDanger()}>{danger?.kind === 'delete' ? '确认删除' : '确认停用'}</AlertDialogAction></AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </main>
  )
}
