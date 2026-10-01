import { relative } from "node:path";
import { styleText } from "node:util";
import type { Finding, Plan, PlannedAction } from "@anyship/core";

export const style = {
  error: (s: string) => styleText("red", s),
  warn: (s: string) => styleText("yellow", s),
  ok: (s: string) => styleText("green", s),
  dim: (s: string) => styleText("dim", s),
  bold: (s: string) => styleText("bold", s),
};

const FINDING_ICON: Record<Finding["level"], string> = {
  error: style.error("✖"),
  warning: style.warn("!"),
  info: style.dim("i"),
};

const ACTION_ICON: Record<PlannedAction["op"], string> = {
  create: style.ok("+"),
  run: style.dim("$"),
  deploy: style.ok("↑"),
  note: style.dim("•"),
};

export function printFindings(findings: Finding[]): void {
  for (const f of findings) {
    const scope = f.service ? style.dim(`[${f.service}] `) : "";
    const file = f.file ? style.dim(` (${f.file})`) : "";
    console.log(`  ${FINDING_ICON[f.level]} ${scope}${f.message}${file}`);
    if (f.hint) console.log(style.dim(`      → ${f.hint}`));
  }
}

export function printPlan(plan: Plan, cwd: string): void {
  console.log(style.bold(`\nPlan for ${plan.target}`));
  if (plan.findings.length) {
    console.log("\nFindings:");
    printFindings(plan.findings);
  }
  if (plan.actions.length) {
    console.log("\nActions:");
    for (const a of plan.actions) {
      const detail = a.detail ? style.dim(`  ${a.detail}`) : "";
      console.log(`  ${ACTION_ICON[a.op]} ${a.kind} ${style.bold(a.name)}${detail}`);
    }
  }
  if (plan.files.length) {
    console.log("\nGenerated files:");
    for (const f of plan.files) console.log(`  ~ ${relative(cwd, f.path)}`);
  }
  const errors = plan.findings.filter((f) => f.level === "error").length;
  console.log(
    errors
      ? `\n${style.error(`${errors} error(s)`)} — this plan cannot be applied.`
      : `\n${style.ok("Plan is ready.")}`,
  );
}
