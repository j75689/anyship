import { existsSync } from "node:fs";
import { readdir, readFile } from "node:fs/promises";
import { basename, extname, join, relative } from "node:path";
import { SPEC_VERSION, type DeploySpecInput, type ResourceType, type ServiceInput } from "@anyship/spec";
import type { Finding } from "./types";

/**
 * Rule-based project detection. Deterministic by design: the same repo always
 * yields the same draft spec. It only drafts anyship.json — the user reviews
 * and commits it, and every later deploy reads that file, not the detector.
 */

export interface Detection {
  spec: DeploySpecInput;
  findings: Finding[];
  /** Human-readable reasons behind each guess. */
  evidence: string[];
}

interface FrameworkRule {
  dep: string;
  framework: string;
  kind: "static" | "server";
  output?: string;
  /** Known to run on edge runtimes such as Cloudflare Workers. */
  edge?: boolean;
}

// First match wins, so meta-frameworks come before the libraries they build on.
const FRAMEWORKS: FrameworkRule[] = [
  { dep: "next", framework: "nextjs", kind: "server" },
  { dep: "nuxt", framework: "nuxt", kind: "server" },
  { dep: "@sveltejs/kit", framework: "sveltekit", kind: "server" },
  { dep: "astro", framework: "astro", kind: "static", output: "dist" },
  { dep: "hono", framework: "hono", kind: "server", edge: true },
  { dep: "@nestjs/core", framework: "nestjs", kind: "server" },
  { dep: "fastify", framework: "fastify", kind: "server" },
  { dep: "express", framework: "express", kind: "server" },
  { dep: "react-scripts", framework: "create-react-app", kind: "static", output: "build" },
  { dep: "vite", framework: "vite", kind: "static", output: "dist" },
];

const LOCKFILES: [file: string, pm: string][] = [
  ["bun.lock", "bun"],
  ["bun.lockb", "bun"],
  ["pnpm-lock.yaml", "pnpm"],
  ["yarn.lock", "yarn"],
  ["package-lock.json", "npm"],
];

const RESOURCE_DEPS: [dep: string, type: ResourceType][] = [
  ["pg", "postgres"],
  ["postgres", "postgres"],
  ["@neondatabase/serverless", "postgres"],
  ["mysql2", "mysql"],
  ["better-sqlite3", "sqlite"],
  ["@libsql/client", "sqlite"],
  ["ioredis", "redis"],
  ["redis", "redis"],
  ["@aws-sdk/client-s3", "bucket"],
];

const RESOURCE_NAMES: Record<ResourceType, string> = {
  postgres: "db",
  mysql: "db",
  sqlite: "db",
  redis: "cache",
  bucket: "storage",
  kv: "kv",
};

/** Node built-ins that edge runtimes don't provide at all. */
const EDGE_BLOCKING_MODULES = ["child_process", "cluster", "dgram", "worker_threads"];
/** Node built-ins edge runtimes only partly emulate. */
const EDGE_LIMITED_MODULES = ["fs", "net"];
/** Dependencies that ship native binaries. */
const NATIVE_DEPS = ["sharp", "bcrypt", "better-sqlite3", "sqlite3", "canvas", "argon2"];

const SOURCE_EXTENSIONS = new Set([".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".mts", ".cts"]);
const SKIP_DIRS = new Set(["node_modules", "dist", "build", ".next", ".git", ".anyship", ".wrangler", "coverage"]);
const MAX_SCANNED_FILES = 2000;

const ENTRY_CANDIDATES = ["src/index.ts", "src/index.js", "src/worker.ts", "src/worker.js", "index.ts", "index.js"];

const IMPORT_RE = new RegExp(
  String.raw`(?:from\s+|require\(\s*|import\(\s*)["'](?:node:)?(` +
    [...EDGE_BLOCKING_MODULES, ...EDGE_LIMITED_MODULES].join("|") +
    String.raw`)(?:/[^"']*)?["']`,
  "g",
);

const PORT_RE = /(?:--port[= ]|-p[= ]|PORT=)(\d{2,5})\b/;

interface PackageJson {
  name?: string;
  main?: string;
  scripts?: Record<string, string>;
  dependencies?: Record<string, string>;
  devDependencies?: Record<string, string>;
}

export async function detectProject(dir: string): Promise<Detection> {
  const findings: Finding[] = [];
  const evidence: string[] = [];
  const files = new Set(await readdir(dir).catch(() => [] as string[]));
  const pkg = files.has("package.json") ? await readJson<PackageJson>(join(dir, "package.json")) : undefined;
  const name = specName(pkg?.name ?? basename(dir));

  const draft = (service: ServiceInput, resources: DeploySpecInput["resources"] = {}): Detection => ({
    spec: { version: SPEC_VERSION, name, services: { web: service }, resources },
    findings,
    evidence,
  });

  if (!pkg) {
    if (files.has("Dockerfile")) {
      evidence.push("Dockerfile found → container service built from it");
      const exposed = await dockerfilePort(join(dir, "Dockerfile"));
      if (exposed) evidence.push(`Dockerfile EXPOSE ${exposed}`);
      return draft({ kind: "server", dockerfile: "Dockerfile", ports: exposed ? [{ port: exposed }] : [] });
    }
    if (files.has("index.html")) {
      evidence.push("index.html without package.json → static site served from the project root");
      return draft({ kind: "static", build: { output: "." } });
    }
    for (const [file, language] of [["go.mod", "Go"], ["requirements.txt", "Python"], ["pyproject.toml", "Python"], ["Cargo.toml", "Rust"]] as const) {
      if (files.has(file)) evidence.push(`${file} found → ${language} project`);
    }
    findings.push({
      level: "error",
      code: "DETECT_UNKNOWN",
      message: "Could not work out how to build or start this project.",
      hint: "Add a Dockerfile, or fill in services.web.start in anyship.json. Only JavaScript projects are auto-detected so far.",
    });
    return draft({ kind: "server", start: "" });
  }

  const deps = { ...pkg.dependencies, ...pkg.devDependencies };
  const scripts = pkg.scripts ?? {};
  const pm = LOCKFILES.find(([file]) => files.has(file))?.[1] ?? "npm";
  evidence.push(`package manager: ${pm}`);

  const rule = FRAMEWORKS.find((r) => r.dep in deps);
  if (rule) evidence.push(`dependency "${rule.dep}" → ${rule.framework} (${rule.kind})`);
  const kind = rule?.kind ?? "server";

  const service: ServiceInput = { kind, runtime: { language: "javascript", framework: rule?.framework } };
  if (scripts.build) service.build = { command: `${pm} run build` };
  if (kind === "static") {
    service.build = { ...service.build, output: rule?.output ?? "dist" };
  } else {
    if (scripts.start) service.start = `${pm} run start`;
    else if (pkg.main) service.start = `node ${pkg.main}`;

    const entry = ENTRY_CANDIDATES.find((candidate) => existsSync(join(dir, candidate)));
    if (rule?.edge && entry) {
      service.entry = entry;
      evidence.push(`edge entry module: ${entry}`);
    }

    const port = Object.values(pickScripts(scripts)).map((s) => PORT_RE.exec(s)?.[1]).find(Boolean);
    if (port) {
      service.ports = [{ port: Number(port) }];
      evidence.push(`port ${port} from package.json scripts`);
    } else if (service.start) {
      service.ports = [{ port: 3000 }];
      findings.push({ level: "info", code: "DETECT_PORT_ASSUMED", message: "No port found in scripts; assumed 3000.", service: "web" });
    }
    if (!service.start && !service.entry) {
      findings.push({
        level: "error",
        code: "DETECT_NO_START",
        message: "No start script, main field or entry module found.",
        service: "web",
        hint: 'Add a "start" script to package.json or set services.web.start in anyship.json.',
      });
      service.start = "";
    }
  }

  const resources: NonNullable<DeploySpecInput["resources"]> = {};
  for (const [dep, type] of RESOURCE_DEPS) {
    if (!(dep in deps)) continue;
    const resourceName = RESOURCE_NAMES[type];
    if (resources[resourceName]) continue;
    resources[resourceName] = { type };
    evidence.push(`dependency "${dep}" → ${type} resource "${resourceName}"`);
  }
  if (Object.keys(resources).length) service.uses = Object.keys(resources);

  if (kind === "server") {
    const blockers = await scanEdgeCompatibility(dir, deps, findings);
    if (blockers > 0) service.runtime = { ...service.runtime, edgeCompatible: false };
    else if (rule?.edge) service.runtime = { ...service.runtime, edgeCompatible: true };
  }

  return draft(service, resources);
}

/** Records edge-runtime incompatibilities as findings and returns how many are blocking. */
async function scanEdgeCompatibility(dir: string, deps: Record<string, string>, findings: Finding[]): Promise<number> {
  let blockers = 0;
  for (const dep of NATIVE_DEPS) {
    if (!(dep in deps)) continue;
    blockers++;
    findings.push({
      level: "warning",
      code: "EDGE_NATIVE_DEPENDENCY",
      message: `"${dep}" ships native binaries, which edge runtimes cannot load.`,
      service: "web",
    });
  }
  for (const file of await listSourceFiles(dir)) {
    const source = await readFile(file, "utf8").catch(() => "");
    const modules = new Set([...source.matchAll(IMPORT_RE)].map((m) => m[1]!));
    for (const mod of modules) {
      const blocking = EDGE_BLOCKING_MODULES.includes(mod);
      if (blocking) blockers++;
      findings.push({
        level: blocking ? "warning" : "info",
        code: blocking ? "EDGE_UNSUPPORTED_MODULE" : "EDGE_LIMITED_MODULE",
        message: blocking
          ? `imports node:${mod}, which edge runtimes do not provide.`
          : `imports node:${mod}, which edge runtimes only partly emulate.`,
        service: "web",
        file: relative(dir, file),
      });
    }
  }
  return blockers;
}

async function listSourceFiles(root: string): Promise<string[]> {
  const out: string[] = [];
  const queue = [root];
  while (queue.length && out.length < MAX_SCANNED_FILES) {
    const dir = queue.shift()!;
    const entries = await readdir(dir, { withFileTypes: true }).catch(() => []);
    for (const entry of entries) {
      if (entry.isDirectory()) {
        if (!SKIP_DIRS.has(entry.name) && !entry.name.startsWith(".")) queue.push(join(dir, entry.name));
      } else if (SOURCE_EXTENSIONS.has(extname(entry.name)) && !entry.name.endsWith(".d.ts")) {
        out.push(join(dir, entry.name));
      }
    }
  }
  return out.sort();
}

function pickScripts(scripts: Record<string, string>): Record<string, string> {
  const picked: Record<string, string> = {};
  for (const key of ["start", "dev", "serve", "preview"]) if (scripts[key]) picked[key] = scripts[key];
  return picked;
}

async function dockerfilePort(file: string): Promise<number | undefined> {
  const text = await readFile(file, "utf8").catch(() => "");
  const match = /^\s*EXPOSE\s+(\d+)/im.exec(text);
  return match ? Number(match[1]) : undefined;
}

async function readJson<T>(file: string): Promise<T | undefined> {
  try {
    return JSON.parse(await readFile(file, "utf8")) as T;
  } catch {
    return undefined;
  }
}

function specName(raw: string): string {
  const cleaned = raw
    .toLowerCase()
    .replace(/^@[^/]+\//, "")
    .replace(/[^a-z0-9-]+/g, "-")
    .replace(/^[^a-z]+/, "")
    .replace(/-+$/, "")
    .slice(0, 63);
  return cleaned || "app";
}
