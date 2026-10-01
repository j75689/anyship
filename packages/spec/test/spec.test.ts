import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { loadSpec, parseSpec } from "../src";

const examples = join(import.meta.dirname, "../../../examples");

describe("parseSpec", () => {
  it("fills defaults for a minimal spec", () => {
    const result = parseSpec({ version: 1, name: "app", services: { web: { kind: "server", start: "node index.js" } } });
    expect(result.ok).toBe(true);
    if (!result.ok) return;
    expect(result.spec.services.web).toMatchObject({ path: ".", replicas: 1, ports: [], volumes: [], uses: [] });
    expect(result.spec.resources).toEqual({});
  });

  it("rejects dangling references", () => {
    const result = parseSpec({
      version: 1,
      name: "app",
      services: {
        web: { kind: "server", start: "x", uses: ["db"], secrets: ["API_KEY"], dependsOn: ["web", "missing"] },
      },
    });
    expect(result.ok).toBe(false);
    if (result.ok) return;
    expect(result.errors).toEqual(
      expect.arrayContaining([
        'services.web.uses.0: unknown resource "db"',
        'services.web.secrets.0: secret "API_KEY" is not declared in top-level secrets',
        "services.web.dependsOn.0: a service cannot depend on itself",
        'services.web.dependsOn.1: unknown service "missing"',
      ]),
    );
  });

  it("requires a way to run non-static services", () => {
    const result = parseSpec({ version: 1, name: "app", services: { web: { kind: "server" } } });
    expect(result.ok).toBe(false);
    if (result.ok) return;
    expect(result.errors).toContain("services.web: needs one of start, entry, image or dockerfile");
  });

  it("rejects invalid names and duplicate ports", () => {
    const result = parseSpec({
      version: 1,
      name: "My App",
      services: { web: { kind: "server", start: "x", ports: [{ port: 80 }, { port: 80 }] } },
    });
    expect(result.ok).toBe(false);
    if (result.ok) return;
    expect(result.errors.some((e) => e.startsWith("name:"))).toBe(true);
    expect(result.errors).toContain("services.web.ports.1.port: port 80 is listed twice");
  });
});

describe("examples", () => {
  it.each(["hono-worker", "ethereum-node"])("%s/anyship.json is valid", async (example) => {
    const result = await loadSpec(join(examples, example, "anyship.json"));
    expect(result.ok ? [] : result.errors).toEqual([]);
  });
});
