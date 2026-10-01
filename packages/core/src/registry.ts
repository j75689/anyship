import type { Adapter } from "./types";

const adapters = new Map<string, Adapter>();

export function registerAdapter(adapter: Adapter): void {
  if (adapters.has(adapter.name)) throw new Error(`adapter "${adapter.name}" is already registered`);
  adapters.set(adapter.name, adapter);
}

export function getAdapter(name: string): Adapter | undefined {
  return adapters.get(name);
}

export function listAdapters(): Adapter[] {
  return [...adapters.values()];
}
