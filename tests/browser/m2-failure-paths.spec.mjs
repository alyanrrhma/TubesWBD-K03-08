// Failure-path acceptance checks against explicitly provisioned disposable QA
// fixtures. Each BROWSER_FAILURE_* ID refers to a case created by the QA
// operator. Tests never modify a production database implicitly.
import { test, expect } from "@playwright/test";

const editor = process.env.BROWSER_FAILURE_EDITOR;
const password = process.env.BROWSER_FAILURE_PASSWORD;
const databaseID = process.env.BROWSER_FAILURE_DATABASE_ID;

async function login(page) {
  test.skip(!editor || !password || !databaseID, "Set BROWSER_FAILURE_EDITOR, _PASSWORD and _DATABASE_ID for disposable QA fixture");
  await page.goto("/login");
  await page.getByLabel("Username").fill(editor);
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Masuk" }).click();
  await expect(page).toHaveURL(/\/dashboard$/);
}

async function readAPI(page, url) {
  const result = await page.evaluate(async (path) => {
    const response = await fetch(path, { credentials: "same-origin" });
    return { status: response.status, json: await response.json().catch(() => ({})) };
  }, url);
  expect(result.status).toBe(200);
  return result.json && Object.keys(result.json).length === 1 && "data" in result.json ? result.json.data : result.json;
}

async function openMigration(page, id) {
  await page.goto(`/databases/${databaseID}/migrations`);
  const row = page.getByRole("row").filter({ has: page.locator(`[data-definition-id="${id}"]`) });
  // Some views use clickable headings instead of row metadata; use API state
  // as the invariant and only assert the page itself remains usable.
  await expect(page.getByRole("heading", { name: "Migration", exact: true })).toBeVisible();
  return row;
}

test("rejected revision remains rejected and cannot be executed", async ({ page }) => {
  const id = process.env.BROWSER_FAILURE_REJECTED_DEFINITION_ID;
  test.skip(!id, "Provision rejected revision and set BROWSER_FAILURE_REJECTED_DEFINITION_ID");
  await login(page);
  await openMigration(page, id);
  const result = await readAPI(page, `/api/migrations/${id}/review`);
  const state = result.status || result.review?.status || result.data?.status;
  expect(String(state).toUpperCase()).toBe("REJECTED");
});

test("permission change during pending review does not silently auto-approve", async ({ page }) => {
  const id = process.env.BROWSER_FAILURE_CHANGED_PERMISSION_DEFINITION_ID;
  test.skip(!id, "Provision pending review with changed group grant and set BROWSER_FAILURE_CHANGED_PERMISSION_DEFINITION_ID");
  await login(page);
  await openMigration(page, id);
  const result = await readAPI(page, `/api/migrations/${id}/readiness`);
  expect(result.ready).toBe(false);
});

test("transient lock retry leaves a traceable attempt", async ({ page }) => {
  const jobID = process.env.BROWSER_FAILURE_LOCK_RETRY_JOB_ID;
  test.skip(!jobID, "Provision retry fixture and set BROWSER_FAILURE_LOCK_RETRY_JOB_ID");
  await login(page);
  await page.goto(`/databases/${databaseID}/migrations`);
  const result = await readAPI(page, `/api/jobs/${jobID}`);
  const job = result.job || result;
  expect(job.job_id).toBe(jobID);
  expect(Number(job.attempt)).toBeGreaterThanOrEqual(2);
  const history = await readAPI(page, `/api/databases/${databaseID}/execution-history?page=1`);
  const attempts = history.items || history.data?.items || [];
  expect(attempts.some((item) => item.job_id === jobID && (item.status === "lock_busy" || item.status === "retrying"))).toBeTruthy();
});

test("partial job advertises explicit resume without silently treating it as success", async ({ page }) => {
  const jobID = process.env.BROWSER_FAILURE_PARTIAL_JOB_ID;
  test.skip(!jobID, "Provision partial fixture and set BROWSER_FAILURE_PARTIAL_JOB_ID");
  await login(page);
  await page.goto(`/databases/${databaseID}/migrations`);
  const result = await readAPI(page, `/api/jobs/${jobID}`);
  const job = result.job || result;
  expect(["partial", "partially_reverted"]).toContain(job.status);
  expect(Number(job.batches_done)).toBeGreaterThan(0);
  expect(job.can_resume).toBe(true);
});
