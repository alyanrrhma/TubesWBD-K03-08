import { test, expect } from "@playwright/test";

const username = process.env.BROWSER_USERNAME;
const password = process.env.BROWSER_PASSWORD;
const databaseID = process.env.BROWSER_DATABASE_ID;
const isAdmin = process.env.BROWSER_IS_ADMIN === "true";
const targetDatabase = process.env.BROWSER_TARGET_DATABASE;

async function expectNoViewportOverflow(page) {
  const sizes = await page.evaluate(() => ({
    document: document.documentElement.scrollWidth,
    viewport: document.documentElement.clientWidth,
    body: document.body.scrollWidth,
  }));
  expect(sizes.document, `document overflow: ${JSON.stringify(sizes)}`).toBeLessThanOrEqual(sizes.viewport);
  expect(sizes.body, `body overflow: ${JSON.stringify(sizes)}`).toBeLessThanOrEqual(sizes.viewport);
}

async function api(page, method, path, body) {
  return page.evaluate(async ({ method, path, body }) => {
    const sessionResponse = await fetch("/api/auth/me");
    const session = await sessionResponse.json();
    const response = await fetch(path, {
      method,
      headers: {
        "Content-Type": "application/json",
        "X-CSRF-Token": session.csrf_token,
      },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const payload = await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(`${response.status} ${path}: ${JSON.stringify(payload)}`);
    return payload && Object.keys(payload).length === 1 && "data" in payload ? payload.data : payload;
  }, { method, path, body });
}

test("login screen fits viewport and serves SPA shell", async ({ page }) => {
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("response", (response) => {
    if (response.status() >= 400 && !(response.status() === 401 && new URL(response.url()).pathname === "/api/auth/me")) {
      errors.push(`${response.status()} ${response.url()}`);
    }
  });
  await page.goto("/login");
  await expect(page.getByRole("heading", { name: "Masuk" })).toBeVisible();
  await expect(page.getByLabel("Username")).toBeVisible();
  await expect(page.getByLabel("Password")).toBeVisible();
  await expectNoViewportOverflow(page);
  expect(errors).toEqual([]);
});

test("authenticated SPA pages remain usable at configured viewport", async ({ page }) => {
  test.skip(!username || !password, "Set BROWSER_USERNAME and BROWSER_PASSWORD for authenticated browser checks.");
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("response", (response) => {
    if (response.status() >= 400 && !(response.status() === 401 && new URL(response.url()).pathname === "/api/auth/me")) {
      errors.push(`${response.status()} ${response.url()}`);
    }
  });

  await page.goto("/login");
  await page.getByLabel("Username").fill(username);
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Masuk" }).click();
  await expect(page).toHaveURL(/\/dashboard$/);
  await expect(page.getByRole("navigation", { name: "Navigasi utama" })).toBeVisible();
  await expectNoViewportOverflow(page);

  if (databaseID) {
    await page.goto(`/databases/${databaseID}/migrations`);
    await expect(page.getByRole("heading", { name: "Migration", exact: true })).toBeVisible();
    const hideRollback = page.getByLabel("Hide rollback");
    await expect(hideRollback).toBeVisible();
    await hideRollback.check();
    await expect(hideRollback).toBeChecked();
    await expectNoViewportOverflow(page);

    await page.goto(`/databases/${databaseID}`);
    await expect(page.getByRole("heading").first()).toBeVisible();
    await expectNoViewportOverflow(page);
  }

  if (isAdmin) {
    for (const route of ["/admin/users", "/admin/groups", "/admin/connections"]) {
      await page.goto(route);
      await expect(page.getByRole("navigation", { name: "Navigasi utama" })).toBeVisible();
      await expectNoViewportOverflow(page);
    }
  }

  if (targetDatabase && isAdmin) {
    const suffix = `${Date.now()}_${Math.random().toString(36).slice(2, 7)}`;
    const session = await api(page, "GET", "/api/auth/me");
    const connectionResponse = await api(page, "POST", "/api/admin/connections", {
      name: `browser_${suffix}`,
      host: process.env.BROWSER_TARGET_HOST || "127.0.0.1",
      port: Number(process.env.BROWSER_TARGET_PORT || 55432),
      database: targetDatabase,
      username: process.env.BROWSER_TARGET_USERNAME || "dbmp",
      password: process.env.BROWSER_TARGET_PASSWORD || "dbmp_dev_password",
      ssl_mode: "disable",
    });
    const connection = connectionResponse.connection || connectionResponse;
    const connectionID = connection.ID ?? connection.id;
    const groupResponse = await api(page, "POST", "/api/admin/groups", { name: `browser_${suffix}` });
    const group = groupResponse.group || groupResponse;
    const groupID = group.ID ?? group.id;
    await api(page, "POST", `/api/admin/groups/${groupID}/members`, { user_id: session.user.id });
    await api(page, "PUT", `/api/admin/groups/${groupID}/leader`, { user_id: session.user.id });
    await api(page, "POST", `/api/admin/groups/${groupID}/access`, { database_id: connectionID, level: "VIEW_EDIT" });

    const tableName = `qa_${suffix}`;
    const created = await api(page, "POST", `/api/databases/${connectionID}/migrations`, {
      name: `browser migration ${suffix}`,
      description: "Playwright responsive and queue verification",
      up_sql: `CREATE TABLE public."${tableName}" (id bigint PRIMARY KEY)`,
      down_sql: `DROP TABLE public."${tableName}"`,
      irreversible: false,
    });
    const definitionID = created.migration.id ?? created.migration.ID;
    await api(page, "POST", `/api/migrations/${definitionID}/submit-review`, {});
    await api(page, "POST", `/api/migrations/${definitionID}/revisions/1/approvals`, { group_id: groupID });
    const readiness = await api(page, "GET", `/api/migrations/${definitionID}/readiness`);
    expect(readiness.ready).toBe(true);
    const queued = await api(page, "POST", `/api/migrations/${definitionID}/submit`, {});
    let job;
    for (let attempt = 0; attempt < 60; attempt += 1) {
      const statusResponse = await api(page, "GET", `/api/jobs/${queued.job_id}`);
      job = statusResponse.job || statusResponse;
      if (["success", "failed", "partial", "partially_reverted", "uncertain"].includes(String(job.status).toLowerCase())) break;
      await page.waitForTimeout(250);
    }
    expect(job.status).toBe("success");

    await page.goto(`/databases/${connectionID}/migrations`);
    await expect(page.getByRole("heading", { name: "Migration", exact: true })).toBeVisible();
    const historyDetail = page.getByRole("button", { name: "SQL / error" }).first();
    await expect(historyDetail).toBeVisible();
    await historyDetail.click();
    await expect(page.locator("tr[id^='execution-detail-'] pre").first()).toContainText(tableName);
    await page.getByLabel("Hide rollback").check();
    await expect(page.getByLabel("Hide rollback")).toBeChecked();
    await expectNoViewportOverflow(page);
    await page.goto(`/databases/${connectionID}`);
    await expect(page.getByText(tableName, { exact: false }).first()).toBeVisible();
    await expectNoViewportOverflow(page);
  }
  expect(errors).toEqual([]);
});
