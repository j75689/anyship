import { spawn } from "node:child_process";
import { existsSync } from "node:fs";
import { writeFile } from "node:fs/promises";
import { dirname, join, relative, resolve } from "node:path";
import { createInterface } from "node:readline/promises";
import { Command } from "commander";
import { cloudflareAdapter } from "@anyship/adapter-cloudflare";
import {
  detectProject,
  getAdapter,
  hasErrors,
  listAdapters,
  registerAdapter,
  type Adapter,
  type AdapterContext,
} from "@anyship/core";
import { loadSpec, parseSpec, SPEC_FILENAME, type DeploySpec } from "@anyship/spec";
import { printFindings, printPlan, style } from "./output";

export const VERSION = "0.1.0";

registerAdapter(cloudflareAdapter);

class CliError extends Error {}

export function createProgram(): Command {
  const program = new Command()
    .name("anyship")
    .description("Deploy any app to any platform from one spec.")
    .version(VERSION);

  program
    .command("init")
    .description(`detect the project and draft ${SPEC_FILENAME}`)
    .argument("[dir]", "project directory", ".")
    .option("-f, --force", `overwrite an existing ${SPEC_FILENAME}`)
    .action(
      run(async (dir: string, opts: { force?: boolean }) => {
        const root = resolve(dir);
        const file = join(root, SPEC_FILENAME);
        if (existsSync(file) && !opts.force) throw new CliError(`${SPEC_FILENAME} already exists; pass --force to overwrite it`);

        const detection = await detectProject(root);
        console.log(style.bold("Detected:"));
        for (const line of detection.evidence) console.log(`  ${style.dim("•")} ${line}`);
        if (detection.findings.length) {
          console.log(style.bold("\nFindings:"));
          printFindings(detection.findings);
        }

        await writeFile(file, JSON.stringify(detection.spec, null, 2) + "\n");
        const shown = relative(process.cwd(), file);
        console.log(`\n${style.ok("✔")} wrote ${shown.startsWith("..") ? file : shown}`);

        const check = parseSpec(detection.spec);
        if (!check.ok) {
          console.log(style.warn("\nThe draft needs edits before it can be deployed:"));
          for (const e of check.errors) console.log(`  ${style.error("✖")} ${e}`);
          process.exitCode = 1;
        } else {
          console.log(style.dim("Review it, commit it, then run `anyship plan --target <target>`."));
        }
      }),
    );

  program
    .command("validate")
    .description(`check ${SPEC_FILENAME} against the schema`)
    .option("-c, --config <file>", "spec file", SPEC_FILENAME)
    .action(
      run(async (opts: { config: string }) => {
        const spec = await load(opts.config);
        const services = Object.keys(spec.services);
        console.log(`${style.ok("✔")} ${spec.name}: ${services.length} service(s) (${services.join(", ")}), ${Object.keys(spec.resources).length} resource(s)`);
      }),
    );

  program
    .command("plan")
    .description("show what a deploy to a target would do, without changing anything")
    .requiredOption("-t, --target <name>", "deploy target (see `anyship targets`)")
    .option("-c, --config <file>", "spec file", SPEC_FILENAME)
    .action(
      run(async (opts: { target: string; config: string }) => {
        const { spec, adapter, ctx } = await prepare(opts.config, opts.target, false);
        const plan = await adapter.plan(spec, ctx);
        printPlan(plan, ctx.cwd);
        if (hasErrors(plan.findings)) process.exitCode = 1;
      }),
    );

  program
    .command("apply")
    .description("deploy to a target")
    .requiredOption("-t, --target <name>", "deploy target (see `anyship targets`)")
    .option("-c, --config <file>", "spec file", SPEC_FILENAME)
    .option("-y, --yes", "skip the confirmation prompt")
    .option("--dry-run", "generate config and run the platform's dry run without deploying")
    .action(
      run(async (opts: { target: string; config: string; yes?: boolean; dryRun?: boolean }) => {
        const dryRun = Boolean(opts.dryRun);
        const { spec, adapter, ctx } = await prepare(opts.config, opts.target, dryRun);
        const plan = await adapter.plan(spec, ctx);
        printPlan(plan, ctx.cwd);
        if (hasErrors(plan.findings)) {
          process.exitCode = 1;
          return;
        }
        if (!opts.yes && !dryRun && !(await confirm(`\nDeploy ${spec.name} to ${adapter.name}?`))) {
          console.log("Aborted.");
          return;
        }
        const result = await adapter.apply(plan, spec, ctx);
        for (const m of result.messages) console.log(result.ok ? style.ok(`✔ ${m}`) : style.error(`✖ ${m}`));
        if (!result.ok) process.exitCode = 1;
      }),
    );

  program
    .command("targets")
    .description("list available deploy targets")
    .action(() => {
      for (const a of listAdapters()) console.log(`  ${style.bold(a.name.padEnd(12))} ${a.description}`);
    });

  return program;
}

async function load(config: string): Promise<DeploySpec> {
  const result = await loadSpec(resolve(config));
  if (!result.ok) throw new CliError(`${config} is invalid:\n` + result.errors.map((e) => `  ✖ ${e}`).join("\n"));
  return result.spec;
}

async function prepare(config: string, target: string, dryRun: boolean): Promise<{ spec: DeploySpec; adapter: Adapter; ctx: AdapterContext }> {
  const adapter = getAdapter(target);
  if (!adapter) {
    throw new CliError(`unknown target "${target}"; available: ${listAdapters().map((a) => a.name).join(", ")}`);
  }
  const spec = await load(config);
  const cwd = dirname(resolve(config));
  return { spec, adapter, ctx: createContext(cwd, adapter.name, dryRun) };
}

function createContext(cwd: string, target: string, dryRun: boolean): AdapterContext {
  return {
    cwd,
    outDir: join(cwd, ".anyship", target),
    dryRun,
    log: (message) => console.log(style.dim(message)),
    exec: (command, args, options) =>
      new Promise((done) => {
        const child = spawn(command, args, {
          cwd: options.cwd,
          stdio: "inherit",
          // npx is a .cmd shim on Windows, which only runs through a shell.
          shell: options.shell ?? process.platform === "win32",
        });
        child.on("error", (err) => {
          console.error(style.error(err.message));
          done(127);
        });
        child.on("close", (code) => done(code ?? 1));
      }),
  };
}

async function confirm(question: string): Promise<boolean> {
  if (!process.stdin.isTTY) throw new CliError("refusing to deploy without confirmation in a non-interactive shell; pass --yes");
  const rl = createInterface({ input: process.stdin, output: process.stdout });
  try {
    return /^y(es)?$/i.test((await rl.question(`${question} [y/N] `)).trim());
  } finally {
    rl.close();
  }
}

/** Wraps an action so expected failures print cleanly instead of as stack traces. */
function run<A extends unknown[]>(action: (...args: A) => Promise<void>): (...args: A) => Promise<void> {
  return async (...args) => {
    try {
      await action(...args);
    } catch (err) {
      if (!(err instanceof CliError)) throw err;
      console.error(style.error(`error: ${err.message}`));
      process.exitCode = 1;
    }
  };
}
