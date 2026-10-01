// Writes schema/anyship.schema.json so editors can validate and autocomplete anyship.json
// via its "$schema" field. Cross-field rules (unknown resources, etc.) are only enforced
// by `anyship validate`.
import { mkdir, writeFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { z } from "zod";
import { DeploySpecSchema } from "../src/schema";

const out = join(dirname(fileURLToPath(import.meta.url)), "../../../schema/anyship.schema.json");
const jsonSchema = z.toJSONSchema(DeploySpecSchema, { io: "input", unrepresentable: "any" });

await mkdir(dirname(out), { recursive: true });
await writeFile(out, JSON.stringify(jsonSchema, null, 2) + "\n");
console.log(`wrote ${out}`);
