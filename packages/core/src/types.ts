import type { DeploySpec } from "@anyship/spec";

export type FindingLevel = "error" | "warning" | "info";

/** Something an adapter or the detector wants the user to know. `error` blocks apply. */
export interface Finding {
  level: FindingLevel;
  /** Stable identifier, e.g. CF_VOLUMES — safe to match on in tests and docs. */
  code: string;
  message: string;
  service?: string;
  file?: string;
  hint?: string;
}

export interface PlannedAction {
  op: "create" | "run" | "deploy" | "note";
  /** What is acted on, e.g. "worker", "d1-database", "build". */
  kind: string;
  name: string;
  detail?: string;
}

export interface GeneratedFile {
  /** Absolute path. */
  path: string;
  contents: string;
}

export interface Plan {
  target: string;
  findings: Finding[];
  actions: PlannedAction[];
  files: GeneratedFile[];
  /** Adapter-private data handed from plan() to apply(). */
  data?: unknown;
}

export interface ApplyResult {
  ok: boolean;
  messages: string[];
}

export interface ExecOptions {
  cwd: string;
  /** Run `command` through the shell (for user-supplied build commands). */
  shell?: boolean;
}

export interface AdapterContext {
  /** Directory containing anyship.json. */
  cwd: string;
  /** Scratch directory for generated platform config, e.g. <cwd>/.anyship/<target>. */
  outDir: string;
  dryRun: boolean;
  log(message: string): void;
  /** Runs a command with inherited stdio and resolves to its exit code. */
  exec(command: string, args: string[], options: ExecOptions): Promise<number>;
}

/**
 * A deploy target. plan() must be side-effect free: it only inspects the spec
 * and reports what apply() would do. apply() must refuse a plan with errors.
 */
export interface Adapter {
  name: string;
  description: string;
  plan(spec: DeploySpec, ctx: AdapterContext): Promise<Plan>;
  apply(plan: Plan, spec: DeploySpec, ctx: AdapterContext): Promise<ApplyResult>;
}

export const hasErrors = (findings: Finding[]): boolean => findings.some((f) => f.level === "error");
