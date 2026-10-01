import { z } from "zod";

/**
 * anyship.json — a platform-neutral description of what an app *needs*
 * (processes, ports, disks, databases, secrets), not how a given platform
 * provides it. Adapters translate this into platform config and either
 * satisfy each need or report that they can't.
 */

export const SPEC_VERSION = 1;

const Name = z
  .string()
  .regex(/^[a-z][a-z0-9-]{0,62}$/, "use lowercase letters, digits and dashes, starting with a letter");

const SecretName = z
  .string()
  .regex(/^[A-Z][A-Z0-9_]*$/, "use UPPER_SNAKE_CASE");

export const PortSchema = z.object({
  name: z.string().optional(),
  port: z.number().int().min(1).max(65535),
  /** `http` ports can sit behind a platform's HTTP router; anything else needs raw TCP/UDP exposure. */
  protocol: z.enum(["http", "tcp", "udp", "tcp+udp"]).default("http"),
  /** `internal` ports are reachable only by other services of this spec. */
  exposure: z.enum(["public", "internal"]).default("public"),
});

export const VolumeSchema = z.object({
  name: Name,
  mountPath: z.string().startsWith("/", "must be an absolute path"),
  size: z.string().regex(/^\d+(GB|TB)$/, 'use a size like "20GB" or "2TB"'),
  class: z.enum(["standard", "nvme"]).default("standard"),
});

export const CronSchema = z.object({
  schedule: z.string().min(1),
  /** Shell command for process-based platforms. Edge platforms invoke their scheduled handler instead. */
  command: z.string().optional(),
});

export const ServiceSchema = z.object({
  /**
   * static — built files served as-is.
   * server — a process (or edge handler) that answers requests.
   * worker — a long-running background process with no inbound traffic.
   */
  kind: z.enum(["static", "server", "worker"]),
  /** Source directory, relative to the spec file. */
  path: z.string().default("."),
  image: z.string().optional(),
  dockerfile: z.string().optional(),
  build: z
    .object({
      command: z.string().optional(),
      /** Build output directory, relative to `path`. */
      output: z.string().optional(),
    })
    .optional(),
  /** Command that starts the process. */
  start: z.string().optional(),
  /** Module exporting a fetch handler, for edge runtimes. Relative to `path`. */
  entry: z.string().optional(),
  ports: z.array(PortSchema).default([]),
  volumes: z.array(VolumeSchema).default([]),
  replicas: z.number().int().min(1).default(1),
  env: z.record(z.string(), z.string()).default({}),
  /** Names of top-level `secrets` this service receives. */
  secrets: z.array(SecretName).default([]),
  /** Names of top-level `resources` this service binds to. */
  uses: z.array(Name).default([]),
  cron: z.array(CronSchema).default([]),
  runtime: z
    .object({
      language: z.string().optional(),
      framework: z.string().optional(),
      /** false when the code relies on APIs edge runtimes (e.g. Workers) don't provide. */
      edgeCompatible: z.boolean().optional(),
    })
    .default({}),
  healthCheck: z
    .object({
      path: z.string().optional(),
      command: z.string().optional(),
    })
    .optional(),
  dependsOn: z.array(Name).default([]),
});

export const ResourceSchema = z.object({
  type: z.enum(["postgres", "mysql", "sqlite", "redis", "bucket", "kv"]),
  /** The resource already exists outside anyship; adapters only wire it up. */
  external: z.boolean().default(false),
});

export const SecretSchema = z.object({
  /** Have anyship generate the value instead of asking for it. */
  generate: z.enum(["hex32"]).optional(),
  description: z.string().optional(),
});

export const DeploySpecSchema = z
  .object({
    $schema: z.string().optional(),
    version: z.literal(SPEC_VERSION),
    name: Name,
    services: z.record(Name, ServiceSchema),
    resources: z.record(Name, ResourceSchema).default({}),
    secrets: z.record(SecretName, SecretSchema).default({}),
    /** Per-platform overrides, keyed by adapter name. Each adapter validates its own block. */
    targets: z.record(z.string(), z.record(z.string(), z.unknown())).default({}),
  })
  .superRefine((spec, ctx) => {
    const issue = (path: (string | number)[], message: string) =>
      ctx.addIssue({ code: "custom", path, message });

    const serviceNames = Object.keys(spec.services);
    if (serviceNames.length === 0) issue(["services"], "at least one service is required");

    for (const [name, svc] of Object.entries(spec.services)) {
      const at = (...path: (string | number)[]) => ["services", name, ...path];

      svc.uses.forEach((resource, i) => {
        if (!(resource in spec.resources)) issue(at("uses", i), `unknown resource "${resource}"`);
      });
      svc.secrets.forEach((secret, i) => {
        if (!(secret in spec.secrets)) issue(at("secrets", i), `secret "${secret}" is not declared in top-level secrets`);
      });
      svc.dependsOn.forEach((dep, i) => {
        if (dep === name) issue(at("dependsOn", i), "a service cannot depend on itself");
        else if (!serviceNames.includes(dep)) issue(at("dependsOn", i), `unknown service "${dep}"`);
      });

      if (svc.kind === "static" && (svc.image || svc.dockerfile)) {
        issue(at("kind"), 'static services are built into files; use kind "server" to run an image');
      }
      if (svc.kind !== "static" && !svc.start && !svc.entry && !svc.image && !svc.dockerfile) {
        issue(at(), "needs one of start, entry, image or dockerfile");
      }

      const seen = new Set<number>();
      svc.ports.forEach((p, i) => {
        if (seen.has(p.port)) issue(at("ports", i, "port"), `port ${p.port} is listed twice`);
        seen.add(p.port);
      });
    }
  });

export type DeploySpec = z.output<typeof DeploySpecSchema>;
export type DeploySpecInput = z.input<typeof DeploySpecSchema>;
export type Service = DeploySpec["services"][string];
export type ServiceInput = DeploySpecInput["services"][string];
export type Resource = DeploySpec["resources"][string];
export type ResourceType = Resource["type"];
export type Port = z.output<typeof PortSchema>;
