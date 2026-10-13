import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { fetchOverview, resetSetting, setSetting } from './api';
import { RefreshIcon } from './components/icons';
import { ServiceList } from './components/ServiceList';
import { SettingsPanel } from './components/SettingsPanel';
import { Toasts, type Toast } from './components/Toasts';
import type { Overview, RosterView, ServiceSettingsView } from './types';

let toastSeq = 0;

export function App() {
  const [overview, setOverview] = useState<Overview | null>(null);
  const [roster, setRoster] = useState<RosterView | null>(null);
  const [connected, setConnected] = useState(false);
  const [selected, setSelected] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [toasts, setToasts] = useState<Toast[]>([]);

  const pushToast = useCallback((kind: Toast['kind'], text: string) => {
    const id = ++toastSeq;
    setToasts((t) => [...t, { id, kind, text }]);
    setTimeout(() => setToasts((t) => t.filter((x) => x.id !== id)), 4200);
  }, []);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const o = await fetchOverview();
      setOverview(o);
      setConnected(o.connected);
      if (o.roster) setRoster(o.roster);
      setLoadError(null);
    } catch (err) {
      setLoadError((err as Error).message);
    } finally {
      setLoading(false);
    }
  }, []);

  const loadRef = useRef(load);
  useEffect(() => {
    loadRef.current = load;
  }, [load]);
  useEffect(() => {
    void load();
  }, [load]);

  // SSE：中心连接状态与名册实时更新；重连后的首次 connected 事件触发一次重载。
  const wasConnectedRef = useRef(false);
  const rosterCountRef = useRef(-1);
  useEffect(() => {
    const es = new EventSource('/api/events');
    es.addEventListener('status', (ev) => {
      const d = JSON.parse((ev as MessageEvent).data) as { connected: boolean };
      setConnected(d.connected);
      if (d.connected && !wasConnectedRef.current) void loadRef.current();
      wasConnectedRef.current = d.connected;
    });
    es.addEventListener('roster', (ev) => {
      const d = JSON.parse((ev as MessageEvent).data) as RosterView;
      setRoster(d);
      const count = d.services?.length ?? 0;
      if (rosterCountRef.current >= 0 && count !== rosterCountRef.current) void loadRef.current();
      rosterCountRef.current = count;
    });
    return () => es.close();
  }, []);

  const services = useMemo(() => roster?.services ?? [], [roster]);

  useEffect(() => {
    if (services.length === 0) {
      setSelected(null);
      return;
    }
    if (selected !== null && services.some((s) => s.name === selected)) return;
    setSelected(services[0]?.name ?? null);
  }, [services, selected]);

  const selectedService = useMemo(() => services.find((s) => s.name === selected) ?? null, [services, selected]);
  const selectedSettings = useMemo(() => {
    if (overview === null || selected === null) return null;
    return overview.settings?.services.find((s) => s.service === selected)?.settings ?? [];
  }, [overview, selected]);

  const applyUpdates = useCallback((updated: ServiceSettingsView[]) => {
    setOverview((prev) => {
      if (!prev) return prev;
      const list = [...(prev.settings?.services ?? [])];
      for (const u of updated) {
        const idx = list.findIndex((s) => s.service === u.service);
        if (idx >= 0) list[idx] = u;
        else list.push(u);
      }
      return { ...prev, settings: { services: list } };
    });
  }, []);

  const onSave = useCallback(
    async (service: string, key: string, value: string) => {
      try {
        const updated = await setSetting(service, key, value);
        applyUpdates(updated);
        pushToast('ok', `已写入 ${service}.${key}`);
      } catch (err) {
        pushToast('error', `写入失败：${(err as Error).message}`);
        throw err;
      }
    },
    [applyUpdates, pushToast],
  );

  const onReset = useCallback(
    async (service: string, key: string) => {
      try {
        const updated = await resetSetting(service, key);
        applyUpdates(updated);
        pushToast('ok', `已重置 ${service}.${key} 为默认值`);
      } catch (err) {
        pushToast('error', `重置失败：${(err as Error).message}`);
        throw err;
      }
    },
    [applyUpdates, pushToast],
  );

  const settingsCountOf = useCallback(
    (name: string) => overview?.settings?.services.find((s) => s.service === name)?.settings?.length ?? 0,
    [overview],
  );

  return (
    <div className="app">
      <header className="topbar">
        <div className="brand">
          <h1>SUMP</h1>
          <span className="brand-sub">设置中心</span>
        </div>
        <div className="topbar-meta">
          <span className={connected ? 'status status-on' : 'status status-off'}>
            <span className="status-dot" aria-hidden="true" />
            {connected ? '已连接设置中心' : '未连接设置中心'}
          </span>
          {overview && <span className="mono muted center-addr">{overview.center}</span>}
          <button type="button" className="btn ghost" onClick={() => void load()} disabled={loading}>
            <RefreshIcon spinning={loading} />
            刷新
          </button>
        </div>
      </header>

      <main className="layout">
        <aside className="sidebar">
          <p className="section-label">服务（{services.length}）</p>
          <ServiceList services={services} selected={selected} onSelect={setSelected} settingsCountOf={settingsCountOf} />
          {loadError && (
            <p className="inline-error" role="alert">
              加载失败：{loadError}
            </p>
          )}
        </aside>
        <section className="content">
          <SettingsPanel
            service={selectedService}
            settings={selectedSettings}
            connected={connected}
            onSave={onSave}
            onReset={onReset}
          />
        </section>
      </main>

      <footer className="footer muted">
        数据源：设置中心 <span className="mono">{overview?.center ?? ''}</span> · 面板为视图层，不持业务状态
        {overview && <span> · 更新于 {new Date(overview.fetchedAt).toLocaleTimeString()}</span>}
      </footer>

      <Toasts items={toasts} />
    </div>
  );
}
