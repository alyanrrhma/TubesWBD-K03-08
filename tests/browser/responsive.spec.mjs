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

async function waitForVisibleJob(page) {
  const region = page.locator("[data-job-status]");
  await expect(region).toHaveAttribute("data-job-id", /.+/);
  const jobID = await region.getAttribute("data-job-id");
  let job;
  for (let attempt = 0; attempt < 60; attempt += 1) {
    const statusResponse = await api(page, "GET", `/api/jobs/${jobID}`);
    job = statusResponse.job || statusResponse;
    if (["success", "failed", "partial", "partially_reverted", "uncertain"].includes(String(job.status).toLowerCase())) break;
    await page.waitForTimeout(250);
  }
  expect(job.status).toBe("success");
  await expect(region).toContainText("Berhasil");
  return job;
}

async function approveOperationInUI(page, operation) {
  const decision = page.locator(`[data-op-review-decision="approved"]`).first();
  await expect(decision).toBeVisible();
  await decision.click();
  const submit = page.getByRole("button", { name: `Antrekan ${operation} yang disetujui` });
  await expect(submit).toBeVisible();
  await submit.click();
  return waitForVisibleJob(page);
}

test("login screen fits viewport and serves SPA shell", async ({ page }) => {
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("response", (response) => {
    const path = new URL(response.url()).pathname;
    const expectedAnonymousSession = response.status() === 401 && path === "/api/auth/me";
    const expectedMissingOperationReview = response.status() === 404
      && response.request().method() === "GET"
      && /\/operations\/(rollback|forward)\/review$/.test(path);
    if (response.status() >= 400 && !expectedAnonymousSession && !expectedMissingOperationReview) {
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
    const path = new URL(response.url()).pathname;
    const expectedAnonymousSession = response.status() === 401 && path === "/api/auth/me";
    const expectedMissingOperationReview = response.status() === 404
      && response.request().method() === "GET"
      && /\/operations\/(rollback|forward)\/review$/.test(path);
    if (response.status() >= 400 && !expectedAnonymousSession && !expectedMissingOperationReview) {
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
    const connectionName = `browser_${suffix}`;
    const connectionResponse = await api(page, "POST", "/api/admin/connections", {
      name: connectionName,
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
    const session = await api(page, "GET", "/api/auth/me");
    await api(page, "POST", `/api/admin/groups/${groupID}/members`, { user_id: session.user.id });
    await api(page, "PUT", `/api/admin/groups/${groupID}/leader`, { user_id: session.user.id });

    const helperUsername = `member_${suffix}`;
    const helperUserResponse = await api(page, "POST", "/api/admin/users", {
      username: helperUsername,
      password: "BrowserMemberPass2026!",
      is_platform_admin: false,
    });
    const helperUser = helperUserResponse.user || helperUserResponse;
    await page.goto("/admin/groups");
    const groupRow = page.getByRole("row").filter({ hasText: `browser_${suffix}` });
    await groupRow.getByRole("button", { name: "Atur group" }).click();
    const groupDetails = page.locator("#group-details");
    const accessForm = groupDetails.locator("#group-access-form");
    await accessForm.locator('[name="database_id"]').selectOption(String(connectionID));
    await accessForm.locator('[name="level"]').selectOption("VIEW_ONLY");
    await accessForm.getByRole("button", { name: "Simpan akses" }).click();
    await expect(groupDetails.getByText("Akses database berhasil disimpan.")).toBeVisible();
    const accessRow = groupDetails.locator(`[data-group-access-row="${connectionID}"]`);
    await expect(accessRow).toContainText(connectionName);
    await expect(accessRow).toContainText("View Only");

    await groupDetails.locator("#group-access-form [name='database_id']").selectOption(String(connectionID));
    await groupDetails.locator("#group-access-form [name='level']").selectOption("VIEW_EDIT");
    await groupDetails.locator("#group-access-form").getByRole("button", { name: "Simpan akses" }).click();
    await expect(groupDetails.locator(`[data-group-access-row="${connectionID}"]`)).toContainText("View + Edit");

    await page.once("dialog", dialog => dialog.accept());
    await groupDetails.locator(`[data-revoke-group-access="${connectionID}"]`).click();
    await expect(groupDetails.locator(`[data-group-access-row="${connectionID}"]`)).toHaveCount(0);

    await groupDetails.locator("#group-access-form [name='database_id']").selectOption(String(connectionID));
    await groupDetails.locator("#group-access-form [name='level']").selectOption("VIEW_EDIT");
    await groupDetails.locator("#group-access-form").getByRole("button", { name: "Simpan akses" }).click();
    await expect(groupDetails.locator(`[data-group-access-row="${connectionID}"]`)).toContainText("View + Edit");
    await expectNoViewportOverflow(page);

    await groupDetails.locator("#group-member").selectOption(String(helperUser.ID ?? helperUser.id));
    await groupDetails.getByRole("button", { name: "Tambahkan anggota" }).click();
    await expect(groupDetails.getByRole("list").getByText(helperUsername)).toBeVisible();
    await groupDetails.locator("#group-leader").selectOption(String(session.user.id));
    await groupDetails.getByRole("button", { name: "Simpan leader" }).click();
    await expect(groupDetails.getByText("Leader approval berhasil diperbarui.")).toBeVisible();

    const tableName = `qa_${suffix}`;
    await page.goto(`/databases/${connectionID}/migrations`);
    const draftForm = page.locator("[data-create-form-wrapper] [data-revision-form]");
    await draftForm.locator('[name="name"]').fill(`browser migration ${suffix}`);
    await draftForm.locator('[name="description"]').fill("Playwright responsive and queue verification");
    await draftForm.locator('[name="up_sql"]').fill(`CREATE TABLE public."${tableName}" (id bigint PRIMARY KEY)`);
    await draftForm.locator('[name="down_sql"]').fill(`DROP TABLE public."${tableName}"`);
    await draftForm.getByRole("button", { name: "Simpan revision draft" }).click();
    await expect(page.getByRole("heading", { name: `browser migration ${suffix}` })).toBeVisible();
    const definitionID = await api(page, "GET", `/api/databases/${connectionID}/migrations`).then(result => {
      const migration = (result.migrations || result.items || []).find(item => item.name === `browser migration ${suffix}`);
      return migration?.id ?? migration?.ID;
    });
    expect(definitionID).toBeTruthy();
    await page.getByRole("button", { name: "Kirim untuk review" }).click();
    await expect(page.getByRole("button", { name: "Setujui" })).toBeVisible();
    await page.getByRole("button", { name: "Setujui" }).click();
    await expect(page.getByText("Semua persyaratan review terpenuhi.")).toBeVisible();
    await page.once("dialog", dialog => dialog.accept());
    await page.getByRole("button", { name: "Antrekan eksekusi yang disetujui" }).click();
    await waitForVisibleJob(page);

    await page.once("dialog", dialog => dialog.accept());
    await page.locator('[data-enqueue-operation="rollback"]').click();
    await expect(page.getByRole("heading", { name: /Review rollback/ })).toBeVisible();
    await approveOperationInUI(page, "rollback");

    await page.once("dialog", dialog => dialog.accept());
    await page.locator('[data-enqueue-operation="forward"]').click();
    await expect(page.getByRole("heading", { name: /Review forward/ })).toBeVisible();
    await approveOperationInUI(page, "forward");

    await page.goto(`/databases/${connectionID}/migrations`);
    await expect(page.getByRole("heading", { name: "Migration", exact: true })).toBeVisible();
    const applyHistoryRow = page.getByRole("row").filter({ hasText: "apply" }).first();
    const historyDetail = applyHistoryRow.getByRole("button", { name: "SQL / error" });
    await expect(historyDetail).toBeVisible();
    await historyDetail.click();
    const applyDetailRow = applyHistoryRow.locator("xpath=following-sibling::tr[1]");
    if (!(await applyDetailRow.isVisible())) await historyDetail.click();
    await expect(applyDetailRow).toBeVisible();
    await expect(applyDetailRow.locator("pre")).toContainText(tableName);
    await page.getByLabel("Hide rollback").check();
    await expect(page.getByLabel("Hide rollback")).toBeChecked();
    await expectNoViewportOverflow(page);
    await page.locator("#migration-sql").fill("SELECT 1");
    await page.getByRole("button", { name: "Analisis execution plan" }).click();
    await expect(page.locator("#migration-plan")).toBeVisible();
    await expect(page.locator("#migration-plan-output")).not.toBeEmpty();
    await page.goto(`/databases/${connectionID}?schema=public`);
    await expect(page.getByText(tableName, { exact: false }).first()).toBeVisible();
    await page.getByRole("link", { name: "customers" }).click();
    await expect(page.getByRole("heading", { name: "public.customers" })).toBeVisible();
    await page.getByRole("link", { name: "Data", exact: true }).click();
    await expect(page.getByText("Baris 1–50 dari 1000")).toBeVisible();
    await expectNoViewportOverflow(page);

    await page.goto(`/databases/${connectionID}/migrations`);
    const batchForm = page.locator("[data-create-form-wrapper] [data-revision-form]");
    await batchForm.locator('[name="name"]').fill(`browser batch migration ${suffix}`);
    await batchForm.locator('[name="description"]').fill("Playwright batch worker verification");
    await batchForm.locator('[name="up_sql"]').fill("-- dbmp:batch table=customers size=250\nUPDATE customers SET name=name WHERE id=ANY($1::bigint[])");
    await batchForm.locator('[name="down_sql"]').fill("UPDATE customers SET name=name WHERE id=ANY($1::bigint[])");
    await batchForm.getByRole("button", { name: "Simpan revision draft" }).click();
    await expect(page.getByRole("heading", { name: `browser batch migration ${suffix}` })).toBeVisible();
    await page.getByRole("button", { name: "Kirim untuk review" }).click();
    await expect(page.getByRole("button", { name: "Setujui" })).toBeVisible();
    await page.getByRole("button", { name: "Setujui" }).click();
    await expect(page.getByText("Semua persyaratan review terpenuhi.")).toBeVisible();
    await page.once("dialog", dialog => dialog.accept());
    await page.getByRole("button", { name: "Antrekan eksekusi yang disetujui" }).click();
    const batchJob = await waitForVisibleJob(page);
    expect(batchJob.mode).toBe("batch");
    expect(batchJob.batches_done).toBe(4);
    expect(batchJob.rows_done).toBe(1000);

    await page.goto("/admin/users");
    const helperUserRow = page.getByRole("row").filter({ hasText: helperUsername });
    await page.once("dialog", dialog => dialog.accept());
    await helperUserRow.getByRole("button", { name: "Hapus" }).click();
    await expect(page.getByRole("row").filter({ hasText: helperUsername })).toHaveCount(0);
    await expectNoViewportOverflow(page);
  }
  expect(errors).toEqual([]);
});
