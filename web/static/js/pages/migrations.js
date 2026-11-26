import { request } from "../api/client.js";
import { escapeHTML } from "../router.js";
import { loading } from "../components/layout.js";
import { renderMigrationWorkflow } from "./migration-workflow.js";
import { filterExecutionHistory } from "./execution-history.js";

export async function migrationsPage(root, shell, user, { id }) {
  root.innerHTML = shell(user, "Migration", loading);
  await draw();

  async function draw() {
    const page = Math.max(1, Number(new URLSearchParams(location.search).get("page")) || 1);
    const hideRollback = localStorage.getItem("dbmp:hide-rollback") === "true";
    const [databaseResult, history] = await Promise.all([
      request(`/api/databases/${id}`),
      request(`/api/databases/${id}/execution-history?page=${page}&hide_rollback=${hideRollback}`),
    ]);
    const db = databaseResult.database;
    const canEdit = db.level === "VIEW_EDIT";
    const currentPage = history.page ?? page;
    const pageSize = history.page_size ?? 20;
    const totalPages = Math.max(1, Math.ceil((history.total ?? 0) / pageSize));
    const items = filterExecutionHistory(history.items || [], hideRollback);

    root.innerHTML = shell(user, `Migration – ${db.name}`, `
      <nav class="crumbs" aria-label="Lokasi"><a href="/dashboard" data-link>Database</a> / <a href="/databases/${id}" data-link>${escapeHTML(db.name)}</a> / <span>Migration</span></nav>
      <header class="pagehead">
        <h1>Migration</h1>
        <span class="level ${canEdit ? "level-edit" : "level-view"}">${canEdit ? "View + Edit" : "View Only"}</span>
      </header>
      <nav class="tabs" aria-label="Bagian database">
        <a href="/databases/${id}" data-link>Schema dan tabel</a>
        <a href="/databases/${id}/migrations" data-link aria-current="page">Migration</a>
      </nav>
      <section id="migration-workflow">
        <h2>Migration workflow M2</h2>
        <p class="muted">Buat revision draft, kirim untuk persetujuan group, lalu antrekan eksekusi melalui worker.</p>
        <div id="migration-workflow-content" aria-live="polite">${loading}</div>
      </section>
      ${canEdit ? `
        <section>
          <h2>Analisis SQL</h2>
          <div class="stack">
            <label for="migration-sql">SQL</label>
            <textarea id="migration-sql" name="sql" class="sql" rows="9" maxlength="1048576" spellcheck="false" placeholder="ALTER TABLE customers ADD COLUMN phone TEXT;"></textarea>
            <p class="muted">EXPLAIN menampilkan estimasi tanpa menjalankan query. EXPLAIN ANALYZE benar-benar menjalankan statement, lalu me-rollback transaksi. Efek non-transaksional seperti sequence atau efek eksternal dari fungsi mungkin tidak dapat dibatalkan.</p>
            <div class="actions">
              <button class="btn btn-secondary" type="button" id="explain-migration">Analisis execution plan</button>
              <button class="btn btn-secondary" type="button" id="analyze-migration">Jalankan EXPLAIN ANALYZE</button>
            </div>
            <p id="migration-error" class="flash flash-error" role="alert" hidden></p>
          </div>
          <p class="notice">Alat ini hanya menganalisis SQL. Eksekusi migration dilakukan lewat workflow M2 di atas.</p>
          <section id="migration-plan" aria-live="polite" hidden>
            <h3 id="migration-plan-title"></h3>
            <p id="migration-plan-description" class="muted"></p>
            <pre id="migration-plan-output" class="sqlblock"></pre>
          </section>
        </section>` : `
        <div class="notice"><strong>Akses View Only.</strong> Anda dapat melihat riwayat migration, tetapi tidak dapat menjalankannya. Minta Platform Admin untuk akses View + Edit bila diperlukan.</div>`}
      <section>
        <header class="pagehead"><h2>Riwayat seluruh attempt</h2>
          <label class="check"><input id="hide-rollback" type="checkbox" ${hideRollback ? "checked" : ""}> Hide rollback</label>
        </header>
        ${items.length ? `<div class="scroll"><table>
          <thead><tr><th>Migration / revision</th><th>Operasi</th><th>Attempt</th><th>Status</th><th>Actor</th><th>Waktu</th><th>Detail</th></tr></thead>
          <tbody>${items.map((m, index) => {
            const status = String(m.status || "unknown").toLowerCase();
            const succeeded = status === "success";
            const detailID = `execution-detail-${index}`;
            return `<tr>
              <td class="mono">${m.legacy ? `M1 v${m.revision}` : `M${m.migration_id} r${m.revision}`}<br><small>${escapeHTML(m.execution_id || "legacy")}</small></td>
              <td>${escapeHTML(m.operation)}</td>
              <td>${m.legacy ? "-" : Number(m.attempt)}</td>
              <td><span class="status ${succeeded ? "status-ok" : "status-fail"}">${escapeHTML(status)}</span></td>
              <td>${escapeHTML(m.actor || "")}</td>
              <td>${escapeHTML(formatTime(m.started_at))}</td>
              <td><button class="btn btn-secondary" type="button" data-history-detail="${detailID}">SQL / error</button></td>
            </tr>`;
          }).map((row, index) => `${row}<tr id="execution-detail-${index}" hidden><td colspan="7"><pre class="sqlblock"></pre></td></tr>`).join("")}</tbody>
        </table></div>` : `<div class="empty"><p>Belum ada migration pada database ini.</p></div>`}
        ${totalPages > 1 ? `<nav class="pager" aria-label="Halaman riwayat migration">
          ${currentPage > 1 ? `<a data-link href="/databases/${id}/migrations?page=${currentPage - 1}">Riwayat lebih baru</a>` : `<span class="muted">Riwayat lebih baru</span>`}
          <span>Halaman ${currentPage} dari ${totalPages}</span>
          ${currentPage < totalPages ? `<a data-link href="/databases/${id}/migrations?page=${currentPage + 1}">Riwayat lebih lama</a>` : `<span class="muted">Riwayat lebih lama</span>`}
        </nav>` : ""}
      </section>`);

    await renderMigrationWorkflow(root, id, user, canEdit);
    root.querySelector("#hide-rollback")?.addEventListener("change", (event) => {
      localStorage.setItem("dbmp:hide-rollback", String(event.currentTarget.checked));
      draw();
    });
    items.forEach((item, index) => {
      const button = root.querySelector(`[data-history-detail="execution-detail-${index}"]`);
      button?.addEventListener("click", () => {
        const row = root.querySelector(`#execution-detail-${index}`);
        const pre = row?.querySelector("pre");
        if (!row || !pre) return;
        row.hidden = !row.hidden;
        pre.textContent = [item.sql_started ? "SQL dikirim ke PostgreSQL" : "SQL belum dijalankan (gagal sebelum eksekusi)", item.sql, item.error ? `Error: ${item.error}` : "", item.sqlstate ? `SQLSTATE: ${item.sqlstate}` : "", item.job_id ? `Job: ${item.job_id}` : "", item.source_execution_id ? `Source execution: ${item.source_execution_id}` : ""].filter(Boolean).join("\n\n");
      });
    });
    if (!canEdit) return;

    const sqlInput = root.querySelector("#migration-sql");
    const error = root.querySelector("#migration-error");
    const planSection = root.querySelector("#migration-plan");
    const planTitle = root.querySelector("#migration-plan-title");
    const planDescription = root.querySelector("#migration-plan-description");
    const planOutput = root.querySelector("#migration-plan-output");

    async function explain(analyze) {
      error.hidden = true;
      planSection.hidden = true;
      const button = root.querySelector(analyze ? "#analyze-migration" : "#explain-migration");
      button.disabled = true;
      try {
        const result = await request(`/api/databases/${id}/migrations/explain`, {
          method: "POST",
          body: { sql: sqlInput.value, analyze },
        });
        planTitle.textContent = analyze ? "Actual execution plan" : "Estimated execution plan";
        planDescription.textContent = analyze
          ? "EXPLAIN ANALYZE menjalankan statement di dalam transaksi yang di-rollback."
          : "EXPLAIN menampilkan estimasi tanpa menjalankan statement.";
        planOutput.textContent = result.plan || "";
        planSection.hidden = false;
      } catch (e) {
        error.textContent = e.message;
        error.hidden = false;
      } finally {
        button.disabled = false;
      }
    }

    function formatTime(value) {
      if (!value) return "-";
      const date = new Date(value);
      return Number.isNaN(date.getTime())
        ? String(value)
        : `${new Intl.DateTimeFormat("en-GB", { timeZone: "UTC", day: "numeric", month: "short", year: "numeric", hour: "2-digit", minute: "2-digit", hourCycle: "h23" }).format(date).replace(",", "")} UTC`;
    }

    root.querySelector("#explain-migration").addEventListener("click", () => explain(false));
    root.querySelector("#analyze-migration").addEventListener("click", () => {
      if (confirm("EXPLAIN ANALYZE menjalankan statement; beberapa efek non-transaksional mungkin tidak dapat di-rollback. Lanjutkan hanya untuk query yang aman?")) {
        explain(true);
      }
    });
  }
}
