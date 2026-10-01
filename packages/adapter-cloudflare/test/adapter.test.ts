import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import type { AdapterContext, Plan } from "@anyship/core";
import { loadSpec, parseSpec, type DeploySpec } from "@anyship/spec";
import { cloudflareAdapter, DEFAULT_COMPATIBILITY_DATE } from "../src";

const cwd = "/work/app";

function context(overrides: Partial<AdapterContext> = {}): AdapterContext {
  return {
    cwd,
    outDir: join(cwd, ".anyship", "cloudflare"),
    dryRun: false,
    log: () => {},
    exec: async () => 0,
    ...overrides,
  };
}

function spec(input: unknown): DeploySpec {
  const result = parseSpec(input);
  if (!result.ok) throw new Error(result.errors.join("\n"));
  return result.spec;
}

/** Parses the generated wrangler.jsonc, skipping its leading comment line. */
function renderedConfig(plan: Plan): Record<string, unknown> {
  const contents = plan.files[0]!.contents;
  return JSON.parse(contents.slice(contents.indexOf("\n") + 1));
}

const codes = (plan: Plan) => plan.findings.map((f) => f.code);

describe("cloudflare adapter plan", () => {
  it("renders a Worker for an edge server", async () => {
    const plan = await cloudflareAdapter.plan(
      spec({
        version: 1,
        name: "api",
        services: { web: { kind: "server", entry: "src/index.ts", env: { MODE: "prod" }, cron: [{ schedule: "0 * * * *" }] } },
        targets: { cloudflare: { domains: ["api.example.com"] } },
      }),
      context(),
    );
    expect(codes(plan)).toEqual([]);
    expect(plan.files[0]!.path).toBe("/work/app/.anyship/cloudflare/wrangler.jsonc");
    expect(renderedConfig(plan)).toEqual({
      name: "api",
      compatibility_date: DEFAULT_COMPATIBILITY_DATE,
      compatibility_flags: ["nodejs_compat"],
      main: "../../src/index.ts",
      vars: { MODE: "prod" },
      triggers: { crons: ["0 * * * *"] },
      routes: [{ pattern: "api.example.com", custom_domain: true }],
    });
    expect(plan.actions.at(-1)).toMatchObject({ op: "deploy", kind: "worker", name: "api" });
  });

  it("renders static assets and runs the build first", async () => {
    const plan = await cloudflareAdapter.plan(
      spec({
        version: 1,
        name: "site",
        services: { web: { kind: "static", path: "apps/site", build: { command: "npm run build", output: "dist" } } },
        targets: { cloudflare: { spa: true } },
      }),
      context(),
    );
    expect(renderedConfig(plan).assets).toEqual({ directory: "../../apps/site/dist", not_found_handling: "single-page-application" });
    expect(plan.actions.map((a) => a.op)).toEqual(["run", "deploy"]);
  });

  it("binds resources and asks for ids it cannot create", async () => {
    const input = {
      version: 1,
      name: "api",
      services: { web: { kind: "server", entry: "src/index.ts", uses: ["db", "files", "cache"] } },
      resources: { db: { type: "sqlite" }, files: { type: "bucket" }, cache: { type: "kv" } },
    };

    const missing = await cloudflareAdapter.plan(spec(input), context());
    expect(codes(missing).filter((c) => c === "CF_MISSING_RESOURCE_ID")).toHaveLength(2);
    expect(missing.files).toEqual([]);
    expect(missing.actions).toContainEqual(expect.objectContaining({ op: "create", detail: "npx wrangler d1 create api-db" }));

    const bound = await cloudflareAdapter.plan(
      spec({ ...input, targets: { cloudflare: { bindings: { db: { id: "d1-123" }, cache: { id: "kv-456" } } } } }),
      context(),
    );
    const config = renderedConfig(bound);
    expect(config.d1_databases).toEqual([{ binding: "DB", database_name: "api-db", database_id: "d1-123" }]);
    expect(config.kv_namespaces).toEqual([{ binding: "CACHE", id: "kv-456" }]);
    expect(config.r2_buckets).toEqual([{ binding: "FILES", bucket_name: "api-files" }]);
  });

  it("refuses what Workers cannot run, with a reason for each", async () => {
    const eth = await loadSpec(join(import.meta.dirname, "../../../examples/ethereum-node/anyship.json"));
    if (!eth.ok) throw new Error(eth.errors.join("\n"));
    const plan = await cloudflareAdapter.plan(eth.spec, context());
    expect(new Set(codes(plan))).toEqual(new Set(["CF_MULTI_SERVICE", "CF_CONTAINER_IMAGE", "CF_VOLUMES", "CF_NON_HTTP_PORT"]));
    expect(plan.files).toEqual([]);
  });

  it("refuses Node-only code and unknown options", async () => {
    const nodeOnly = await cloudflareAdapter.plan(
      spec({ version: 1, name: "api", services: { web: { kind: "server", start: "node server.js", runtime: { edgeCompatible: false } } } }),
      context(),
    );
    expect(codes(nodeOnly)).toEqual(["CF_EDGE_INCOMPATIBLE"]);

    const typo = await cloudflareAdapter.plan(
      spec({ version: 1, name: "api", services: { web: { kind: "server", entry: "a.ts" } }, targets: { cloudflare: { domain: ["x.com"] } } }),
      context(),
    );
    expect(codes(typo)).toEqual(["CF_BAD_OPTIONS"]);
  });
});

describe("cloudflare adapter apply", () => {
  it("writes config, builds, then deploys with wrangler", async () => {
    const outDir = await mkdtemp(join(tmpdir(), "anyship-cf-"));
    try {
      const calls: string[] = [];
      const ctx = context({
        outDir,
        dryRun: true,
        exec: async (command, args) => {
          calls.push([command, ...args].join(" "));
          return 0;
        },
      });
      const s = spec({ version: 1, name: "site", services: { web: { kind: "static", build: { command: "make", output: "out" } } } });
      const result = await cloudflareAdapter.apply(await cloudflareAdapter.plan(s, ctx), s, ctx);
      const configPath = join(outDir, "wrangler.jsonc");
      expect(result.ok).toBe(true);
      expect(calls).toEqual(["make", `npx wrangler deploy --config ${configPath} --dry-run`]);
      expect(await readFile(configPath, "utf8")).toContain('"name": "site"');
    } finally {
      await rm(outDir, { recursive: true, force: true });
    }
  });

  it("refuses a plan with errors", async () => {
    const s = spec({ version: 1, name: "api", services: { web: { kind: "worker", start: "node job.js" } } });
    const ctx = context({
      exec: async () => {
        throw new Error("must not run");
      },
    });
    const result = await cloudflareAdapter.apply(await cloudflareAdapter.plan(s, ctx), s, ctx);
    expect(result.ok).toBe(false);
  });
});
