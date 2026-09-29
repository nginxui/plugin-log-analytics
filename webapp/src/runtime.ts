// State shared by the entry bundle and the on-demand chunks. Each output file
// carries its own copy of the modules it imports, so anything that has to be a
// singleton lives on a global under a registered symbol.
import type { PluginRegistry } from '@nginxui/plugin-sdk'

export interface Runtime {
  registry?: PluginRegistry
  /** Status store, created on first use by store/status.ts. */
  status?: unknown
  /** Time of the last warm request in milliseconds. */
  warmedAt?: number
}

const RUNTIME_KEY = Symbol.for('com.nginxui.log-analytics.runtime')

export function getRuntime(): Runtime {
  const holder = globalThis as unknown as Record<symbol, Runtime | undefined>
  holder[RUNTIME_KEY] ??= {}
  return holder[RUNTIME_KEY]
}

export function setRegistry(registry: PluginRegistry): void {
  getRuntime().registry = registry
}

export function requireRegistry(): PluginRegistry {
  const registry = getRuntime().registry
  if (!registry)
    throw new Error('[log-analytics] registry is not ready yet')
  return registry
}
