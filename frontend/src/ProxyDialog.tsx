import { useState, type FormEvent } from 'react'
import type { Proxy } from './api'
import { Button } from '@/components/ui/button'
import {
  Dialog, DialogContent, DialogDescription, DialogFooter,
  DialogHeader, DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'

type Props = {
  providerId: string
  proxy: Proxy | null
  open: boolean
  busy: boolean
  onOpenChange: (open: boolean) => void
  onSave: (value: Record<string, unknown>) => Promise<boolean>
}

export function ProxyDialog({ providerId, proxy, open, busy, onOpenChange, onSave }: Props) {
  const [id, setId] = useState(proxy?.id ?? '')
  const [name, setName] = useState(proxy?.name ?? '')
  const [host, setHost] = useState(proxy?.host ?? '')
  const [port, setPort] = useState(String(proxy?.port ?? 1080))
  const [username, setUsername] = useState(proxy?.username ?? '')
  const [password, setPassword] = useState('')
  const [clearPassword, setClearPassword] = useState(false)
  const [enabled, setEnabled] = useState(proxy?.enabled ?? true)

  async function submit(event: FormEvent) {
    event.preventDefault()
    const value: Record<string, unknown> = {
      id: proxy?.id ?? id.trim(), name: name.trim(), host: host.trim(),
      port: Number(port), enabled, username,
    }
    if (!proxy || password || clearPassword || !username) {
      value.password = clearPassword || !username ? '' : password
    }
    if (await onSave(value)) onOpenChange(false)
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{proxy ? '编辑代理' : '新增代理'}</DialogTitle>
          <DialogDescription>Provider：{providerId}。代理 ID 创建后保持不变。</DialogDescription>
        </DialogHeader>
        <form onSubmit={submit} className="grid gap-4">
          <div className="grid gap-2">
            <Label htmlFor="proxy-id">代理 ID</Label>
            <Input id="proxy-id" value={id} onChange={event => setId(event.target.value)} disabled={!!proxy} required pattern="[a-z][a-z0-9]*(-[a-z0-9]+)*" placeholder="proxy-seller-01" />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="proxy-name">名称或备注</Label>
            <Input id="proxy-name" value={name} onChange={event => setName(event.target.value)} placeholder="主代理" />
          </div>
          <div className="grid grid-cols-[1fr_7rem] gap-3">
            <div className="grid gap-2"><Label htmlFor="proxy-host">主机</Label><Input id="proxy-host" value={host} onChange={event => setHost(event.target.value)} required placeholder="proxy.example.com" /></div>
            <div className="grid gap-2"><Label htmlFor="proxy-port">端口</Label><Input id="proxy-port" type="number" min="1" max="65535" value={port} onChange={event => setPort(event.target.value)} required /></div>
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div className="grid gap-2"><Label htmlFor="proxy-user">用户名</Label><Input id="proxy-user" value={username} onChange={event => setUsername(event.target.value)} autoComplete="off" placeholder="可选" /></div>
            <div className="grid gap-2"><Label htmlFor="proxy-password">密码</Label><Input id="proxy-password" type="password" value={password} onChange={event => setPassword(event.target.value)} autoComplete="new-password" placeholder={proxy?.has_password ? '留空则保持原密码' : '可选'} disabled={clearPassword} /></div>
          </div>
          {proxy?.has_password && <label className="flex items-center gap-2 text-sm text-muted-foreground"><input type="checkbox" checked={clearPassword} onChange={event => setClearPassword(event.target.checked)} />清除已保存的密码</label>}
          <div className="flex items-center justify-between rounded-lg border p-3"><Label htmlFor="proxy-enabled">启用代理</Label><Switch id="proxy-enabled" checked={enabled} onCheckedChange={setEnabled} /></div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>取消</Button>
            <Button type="submit" disabled={busy}>{busy ? '保存中…' : '保存'}</Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
