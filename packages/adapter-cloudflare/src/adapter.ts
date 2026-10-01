import { mkdir, writeFile } from "node:fs/promises";
import { dirname, join, relative, resolve, sep } from "node:path";
import { z } from "zod";
import type { DeploySpec, ResourceType, Service } from "@anyship/spec";
import { hasErrors, type Adapter, type Finding, type Plan, type PlannedAction } from "@anyship/core";

/**
 * Cloudflare Workers adapter. Renders a wrangler.jsonc from the spec and
 * deploys it with `wrangler deploy`. Cloudflare can't host everything a spec
 * can describe (disks, raw TCP/UDP, container images, Node-only code), so
 * plan() reports each unmet need as an error instead of quietly dropping it.
 */

export const DEFAULT_COMPATIBILITY_DATE = "2026-09-01";

export const CloudflareOptionsSchema = z
  .object({
    /** Worker name; defaults to the spec name. */
    name: z.string().optional(),
    compatibilityDate: z.string().regex(/^\d{4}-\d{2}-\d{2}$/).optional(),
    accountId: z.string().optional(),
    /** Custom domains to attach to the Worker. */
    domains: z.array(z.string()).optional(),
    /** Single-page app: serve index.html for unknown paths. */
    spa: z.boolean().optional(),
    /**
     * Existing Cloudflare resources, keyed by spec resource name: the D1
     * database id, KV namespace id, Hyperdrive config id, or R2 bucket name.
     */
    bindings: z.record(z.string(), z.object({ id: z.string().min(1) })).optional(),
  })
  .strict();

export type CloudflareOptions = z.infer<typeof CloudflareOptionsSchema>;

interface CloudflarePlanData {
  configPath: string;
  serviceDir: string;
  buildCommand?: string;
}

type BindingSection = "d1_databases" | "r2_buckets" | "kv_namespaces" | "hyperdrive";

interface BindingRule {
  section: BindingSection;
  /** Cloudflare product name, for messages. */
  product: string;
  /** Command that creates the resource and prints its id. */
  createCommand(name: string): string;
  /** R2 buckets are addressed by name, so they need no id from the user. */
  needsId: boolean;
}

const BINDING_RULES: Record<ResourceType, BindingRule | null> = {
  sqlite: { section: "d1_databases", product: "D1", createCommand: (n) => `npx wrangler d1 create ${n}`, needsId: true },
  kv: { section: "kv_namespaces", product: "KV", createCommand: (n) => `npx wrangler kv namespace create ${n}`, needsId: true },
  bucket: { section: "r2_buckets", product: "R2", createCommand: (n) => `npx wrangler r2 bucket create ${n}`, needsId: false },
  postgres: {
    section: "hyperdrive",
    product: "Hyperdrive",
    createCommand: (n) => `npx wrangler hyperdrive create ${n} --connection-string="postgres://..."`,
    needsId: true,
  },
  mysql: {
    section: "hyperdrive",
    product: "Hyperdrive",
    createCommand: (n) => `npx wrangler hyperdrive create ${n} --connection-string="mysql://..."`,
    needsId: true,
  },
  redis: null,
};

export const cloudflareAdapter: Adapter = {
  name: "cloudflare",
  description: "Cloudflare Workers (edge handlers and static assets) via wrangler",

  async plan(spec, ctx) {
    const findings: Finding[] = [];
    const actions: PlannedAction[] = [];
    const plan: Plan = { target: "cloudflare", findings, actions, files: [] };

    const parsed = CloudflareOptionsSchema.safeParse(spec.targets.cloudflare ?? {});
    if (!parsed.success) {
      for (const issue of parsed.error.issues) {
        findings.push({
          level: "error",
          code: "CF_BAD_OPTIONS",
          message: `targets.cloudflare.${issue.path.join(".")}: ${issue.message}`,
        });
      }
      return plan;
    }
    const options = parsed.data;

    const services = Object.entries(spec.services);
    if (services.length !== 1) {
      findings.push({
        level: "error",
        code: "CF_MULTI_SERVICE",
        message: `The Cloudflare target deploys one Worker, but this spec has ${services.length} services (${services.map(([n]) => n).join(", ")}).`,
        hint: "Split the spec per service, or use a container/VPS target.",
      });
    }
    for (const [name, svc] of services) checkService(name, svc, spec, findings);
    if (hasErrors(findings)) return plan;

    const [serviceName, svc] = services[0]!;
    const workerName = options.name ?? spec.name;
    const serviceDir = resolve(ctx.cwd, svc.path);
    const configPath = join(ctx.outDir, "wrangler.jsonc");
    // wrangler resolves paths relative to the config file, which lives in outDir.
    const fromConfig = (target: string) => toPosix(relative(ctx.outDir, target)) || ".";

    const config: Record<string, unknown> = {
      name: workerName,
      compatibility_date: options.compatibilityDate ?? DEFAULT_COMPATIBILITY_DATE,
      compatibility_flags: ["nodejs_compat"],
    };
    if (options.accountId) config.account_id = options.accountId;

    if (svc.kind === "server") {
      config.main = fromConfig(resolve(serviceDir, svc.entry!));
    } else {
      const output = svc.build?.output ?? ".";
      const assetsDir = resolve(serviceDir, output);
      config.assets = {
        directory: fromConfig(assetsDir),
        ...(options.spa ? { not_found_handling: "single-page-application" } : {}),
      };
      if (assetsDir === serviceDir) {
        findings.push({
          level: "warning",
          code: "CF_ASSETS_ROOT",
          message: "Static assets are served from the service root, so every file in it will be uploaded.",
          service: serviceName,
          hint: "Set services.<name>.build.output to the directory that holds only the built site.",
        });
      }
    }

    if (Object.keys(svc.env).length) config.vars = svc.env;
    if (svc.cron.length) config.triggers = { crons: svc.cron.map((c) => c.schedule) };
    if (options.domains?.length) {
      config.routes = options.domains.map((pattern) => ({ pattern, custom_domain: true }));
    }

    for (const resourceName of svc.uses) {
      const resource = spec.resources[resourceName]!;
      const rule = BINDING_RULES[resource.type]!;
      const bindingName = resourceName.toUpperCase().replace(/-/g, "_");
      const cfName = `${workerName}-${resourceName}`;
      const id = options.bindings?.[resourceName]?.id;

      if (rule.needsId && !id) {
        findings.push({
          level: "error",
          code: "CF_MISSING_RESOURCE_ID",
          message: `Resource "${resourceName}" (${resource.type}) needs an existing ${rule.product} id.`,
          service: serviceName,
          hint: `Run \`${rule.createCommand(cfName)}\` and set targets.cloudflare.bindings.${resourceName}.id.`,
        });
        actions.push({ op: "create", kind: rule.product, name: cfName, detail: rule.createCommand(cfName) });
        continue;
      }

      const entries = (config[rule.section] ??= []) as Record<string, string>[];
      switch (rule.section) {
        case "d1_databases":
          entries.push({ binding: bindingName, database_name: cfName, database_id: id! });
          break;
        case "kv_namespaces":
        case "hyperdrive":
          entries.push({ binding: bindingName, id: id! });
          break;
        case "r2_buckets":
          entries.push({ binding: bindingName, bucket_name: id ?? cfName });
          if (!id) {
            findings.push({
              level: "info",
              code: "CF_R2_BUCKET",
              message: `R2 bucket "${cfName}" must exist before deploying.`,
              service: serviceName,
              hint: `Run \`${rule.createCommand(cfName)}\`, or set targets.cloudflare.bindings.${resourceName}.id to an existing bucket name.`,
            });
          }
          break;
      }
    }

    for (const secret of svc.secrets) {
      actions.push({ op: "note", kind: "secret", name: secret, detail: `set it once with: npx wrangler secret put ${secret} --name ${workerName}` });
    }

    // Missing resource ids are errors; the create actions above tell the user what to run.
    if (hasErrors(findings)) return plan;

    if (svc.build?.command) actions.push({ op: "run", kind: "build", name: serviceName, detail: svc.build.command });
    actions.push({ op: "deploy", kind: "worker", name: workerName, detail: `wrangler deploy --config ${toPosix(relative(ctx.cwd, configPath))}` });

    plan.files.push({
      path: configPath,
      contents: `// Generated by anyship from anyship.json. Edit anyship.json instead; this file is overwritten.\n${JSON.stringify(config, null, 2)}\n`,
    });
    plan.data = { configPath, serviceDir, buildCommand: svc.build?.command } satisfies CloudflarePlanData;
    return plan;
  },

  async apply(plan, _spec, ctx) {
    if (hasErrors(plan.findings)) return { ok: false, messages: ["The plan has errors; fix them and plan again."] };
    const data = plan.data as CloudflarePlanData;

    for (const file of plan.files) {
      await mkdir(dirname(file.path), { recursive: true });
      await writeFile(file.path, file.contents);
      ctx.log(`wrote ${toPosix(relative(ctx.cwd, file.path))}`);
    }

    if (data.buildCommand) {
      ctx.log(`$ ${data.buildCommand}`);
      const code = await ctx.exec(data.buildCommand, [], { cwd: data.serviceDir, shell: true });
      if (code !== 0) return { ok: false, messages: [`Build failed (exit ${code}).`] };
    }

    const args = ["wrangler", "deploy", "--config", data.configPath];
    if (ctx.dryRun) args.push("--dry-run");
    ctx.log(`$ npx ${args.join(" ")}`);
    const code = await ctx.exec("npx", args, { cwd: ctx.cwd });
    if (code !== 0) return { ok: false, messages: [`wrangler deploy failed (exit ${code}).`] };
    return { ok: true, messages: [ctx.dryRun ? "Dry run finished; nothing was deployed." : "Deployed to Cloudflare."] };
  },
};

function checkService(name: string, svc: Service, spec: DeploySpec, findings: Finding[]): void {
  const error = (code: string, message: string, hint?: string) =>
    findings.push({ level: "error", code, message, service: name, hint });

  if (svc.image || svc.dockerfile) {
    error(
      "CF_CONTAINER_IMAGE",
      "Container images need Cloudflare Containers, which this adapter does not support yet.",
      "Deploy this service to a container/VPS target for now.",
    );
  }
  if (svc.kind === "worker") {
    error(
      "CF_BACKGROUND_WORKER",
      "Workers cannot run long-lived background processes.",
      "Move the work to cron triggers or Queues, or use a container/VPS target.",
    );
  }
  if (svc.volumes.length) {
    error(
      "CF_VOLUMES",
      `Workers have no persistent disk (volumes: ${svc.volumes.map((v) => v.name).join(", ")}).`,
      "Store data in D1 (sqlite), R2 (bucket) or KV resources instead.",
    );
  }
  for (const port of svc.ports) {
    if (port.protocol !== "http") {
      error("CF_NON_HTTP_PORT", `Port ${port.port} uses ${port.protocol}; Workers only receive HTTP requests.`);
    }
  }
  if (svc.kind === "server" && !svc.image && !svc.dockerfile) {
    if (svc.runtime.framework === "nextjs") {
      error(
        "CF_FRAMEWORK_ADAPTER",
        "Next.js needs the OpenNext Cloudflare adapter, which anyship does not drive yet.",
        "Build with @opennextjs/cloudflare and point services.<name>.entry at its worker output.",
      );
    } else if (svc.runtime.edgeCompatible === false) {
      error(
        "CF_EDGE_INCOMPATIBLE",
        "This service uses APIs that Cloudflare Workers do not provide (see `anyship init` findings).",
        "Remove the Node-only code paths, or use a container/VPS target.",
      );
    } else if (!svc.entry) {
      error(
        "CF_NO_ENTRY",
        "Workers need an entry module that exports a fetch handler.",
        "Set services.<name>.entry, e.g. \"src/index.ts\".",
      );
    }
  }
  for (const resourceName of svc.uses) {
    const type = spec.resources[resourceName]!.type;
    if (!BINDING_RULES[type]) {
      error(
        "CF_UNSUPPORTED_RESOURCE",
        `Cloudflare has no managed ${type} (resource "${resourceName}").`,
        "Use an HTTP-based external service, or KV for simple caching.",
      );
    }
  }
  if (svc.replicas > 1) {
    findings.push({ level: "info", code: "CF_REPLICAS_IGNORED", message: "Workers scale automatically; replicas is ignored.", service: name });
  }
  if (svc.cron.some((c) => c.command)) {
    findings.push({
      level: "warning",
      code: "CF_CRON_COMMAND",
      message: "Cron triggers call the Worker's scheduled() handler; cron commands are ignored.",
      service: name,
    });
  }
}

function toPosix(path: string): string {
  return path.split(sep).join("/");
}
