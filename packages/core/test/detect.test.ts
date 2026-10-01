import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import { parseSpec } from "@anyship/spec";
import { detectProject } from "../src";

const dirs: string[] = [];

async function project(files: Record<string, string | object>): Promise<string> {
  const dir = await mkdtemp(join(tmpdir(), "anyship-detect-"));
  dirs.push(dir);
  for (const [name, contents] of Object.entries(files)) {
    const file = join(dir, name);
    await mkdir(dirname(file), { recursive: true });
    await writeFile(file, typeof contents === "string" ? contents : JSON.stringify(contents));
  }
  return dir;
}

afterEach(async () => {
  await Promise.all(dirs.splice(0).map((d) => rm(d, { recursive: true, force: true })));
});

describe("detectProject", () => {
  it("drafts an edge-compatible Hono service", async () => {
    const dir = await project({
      "package.json": { name: "@acme/API", dependencies: { hono: "^4" } },
      "pnpm-lock.yaml": "",
      "src/index.ts": 'import { Hono } from "hono";\nexport default new Hono();\n',
    });
    const { spec, findings } = await detectProject(dir);
    expect(spec.name).toBe("api");
    expect(spec.services.web).toMatchObject({
      kind: "server",
      entry: "src/index.ts",
      runtime: { framework: "hono", edgeCompatible: true },
    });
    expect(findings.filter((f) => f.level === "error")).toEqual([]);
    expect(parseSpec(spec).ok).toBe(true);
  });

  it("flags Node-only code as edge-incompatible", async () => {
    const dir = await project({
      "package.json": { dependencies: { express: "^5" }, scripts: { start: "PORT=8080 node server.js" } },
      "server.js": 'const { exec } = require("node:child_process");\nconst fs = require("fs");\n',
    });
    const { spec, findings } = await detectProject(dir);
    expect(spec.services.web).toMatchObject({
      start: "npm run start",
      ports: [{ port: 8080 }],
      runtime: { framework: "express", edgeCompatible: false },
    });
    expect(findings.map((f) => f.code)).toEqual(expect.arrayContaining(["EDGE_UNSUPPORTED_MODULE", "EDGE_LIMITED_MODULE"]));
  });

  it("maps database dependencies to resources", async () => {
    const dir = await project({
      "package.json": { dependencies: { hono: "^4", pg: "^8", "@aws-sdk/client-s3": "^3" } },
      "src/index.ts": "export default {};",
    });
    const { spec } = await detectProject(dir);
    expect(spec.resources).toEqual({ db: { type: "postgres" }, storage: { type: "bucket" } });
    expect(spec.services.web?.uses).toEqual(["db", "storage"]);
  });

  it("detects a Vite static site", async () => {
    const dir = await project({ "package.json": { devDependencies: { vite: "^7" }, scripts: { build: "vite build" } }, "yarn.lock": "" });
    const { spec } = await detectProject(dir);
    expect(spec.services.web).toMatchObject({ kind: "static", build: { command: "yarn run build", output: "dist" } });
  });

  it("falls back to a Dockerfile", async () => {
    const dir = await project({ Dockerfile: "FROM alpine\nEXPOSE 8000\n" });
    const { spec } = await detectProject(dir);
    expect(spec.services.web).toMatchObject({ kind: "server", dockerfile: "Dockerfile", ports: [{ port: 8000 }] });
  });

  it("reports projects it cannot understand", async () => {
    const dir = await project({ "go.mod": "module example.com/x\n" });
    const { findings, evidence } = await detectProject(dir);
    expect(findings.map((f) => f.code)).toContain("DETECT_UNKNOWN");
    expect(evidence).toContain("go.mod found → Go project");
  });
});
