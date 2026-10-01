import { readFile } from "node:fs/promises";
import { DeploySpecSchema, type DeploySpec } from "./schema";

export const SPEC_FILENAME = "anyship.json";

export type ParseResult = { ok: true; spec: DeploySpec } | { ok: false; errors: string[] };

export function parseSpec(input: unknown): ParseResult {
  const result = DeploySpecSchema.safeParse(input);
  if (result.success) return { ok: true, spec: result.data };
  return {
    ok: false,
    errors: result.error.issues.map((i) => `${i.path.join(".") || "(root)"}: ${i.message}`),
  };
}

export async function loadSpec(file: string): Promise<ParseResult> {
  let raw: string;
  try {
    raw = await readFile(file, "utf8");
  } catch (err) {
    return { ok: false, errors: [`cannot read ${file}: ${(err as Error).message}`] };
  }
  let json: unknown;
  try {
    json = JSON.parse(raw);
  } catch (err) {
    return { ok: false, errors: [`${file} is not valid JSON: ${(err as Error).message}`] };
  }
  return parseSpec(json);
}
