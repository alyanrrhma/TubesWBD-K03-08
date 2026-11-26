import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { test } from "node:test";

const clientPath = new URL("../../web/static/js/api/client.js", import.meta.url);
const source = await readFile(clientPath, "utf8");
const client = await import(`data:text/javascript;charset=utf-8,${encodeURIComponent(source)}`);

function response(status, payload, contentType = "application/json") {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: { get: () => contentType },
    json: async () => payload,
    text: async () => payload,
  };
}

test("unwraps workflow API success envelopes", async () => {
  globalThis.fetch = async () => response(200, { data: { id: 8, state: "draft" } });

  assert.deepEqual(await client.request("/api/migrations/8"), { id: 8, state: "draft" });
});

test("preserves bare legacy success payloads, including data fields", async () => {
  const legacyPayload = { data: { rows: [] }, pages: 3 };
  globalThis.fetch = async () => response(200, legacyPayload);

  assert.deepEqual(await client.request("/api/tables/1/data"), legacyPayload);
});

test("preserves structured workflow error details", async () => {
  globalThis.fetch = async () => response(409, {
    error: { code: "approval_incomplete", message: "Persetujuan belum lengkap." },
  });

  await assert.rejects(client.request("/api/migrations/8/submit"), (error) => {
    assert.equal(error.message, "Persetujuan belum lengkap.");
    assert.equal(error.status, 409);
    assert.equal(error.code, "approval_incomplete");
    return true;
  });
});

test("preserves legacy string errors", async () => {
  globalThis.fetch = async () => response(403, { error: "Akses ditolak." });

  await assert.rejects(client.request("/api/admin/users"), (error) => {
    assert.equal(error.message, "Akses ditolak.");
    assert.equal(error.status, 403);
    assert.equal(error.code, undefined);
    return true;
  });
});

test("keeps non-JSON response text and HTTP status", async () => {
  globalThis.fetch = async () => response(200, "plain response", "text/plain");
  assert.equal(await client.request("/api/text"), "plain response");

  globalThis.fetch = async () => response(502, "upstream unavailable", "text/plain");
  await assert.rejects(client.request("/api/text"), (error) => {
    assert.equal(error.message, "upstream unavailable");
    assert.equal(error.status, 502);
    return true;
  });
});

test("dispatches the unauthorized event for 401 responses", async () => {
  let eventName = "";
  globalThis.window = { dispatchEvent: (event) => { eventName = event.type; } };
  globalThis.CustomEvent = class CustomEvent {
    constructor(type) { this.type = type; }
  };
  globalThis.fetch = async () => response(401, { error: "Sesi berakhir." });

  await assert.rejects(client.request("/api/me"), /Sesi berakhir/);
  assert.equal(eventName, "dbmp:unauthorized");
});

test("keeps credentials and CSRF headers for mutations", async () => {
  let requestOptions;
  client.setCSRF("csrf-test-token");
  globalThis.fetch = async (_path, options) => {
    requestOptions = options;
    return response(202, { data: { job_id: 14 } });
  };

  assert.deepEqual(await client.request("/api/migrations/8/submit", {
    method: "POST",
    body: { revision_id: 3 },
  }), { job_id: 14 });
  assert.equal(requestOptions.credentials, "same-origin");
  assert.equal(requestOptions.headers.get("X-CSRF-Token"), "csrf-test-token");
  client.setCSRF("");
});
