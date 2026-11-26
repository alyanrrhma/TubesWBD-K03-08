import { request } from "../api/client.js";
import { escapeHTML } from "../router.js";

const statusNames = {
  DRAFT: "Draft",
  PENDING_REVIEW: "Menunggu review",
  APPROVED: "Disetujui",
  REJECTED: "Ditolak",
  EXECUTED: "Sudah dieksekusi",
};

function statusLabel(status) {
  return statusNames[status] || status || "Tidak diketahui";
}

function revisionForm(migration, revision) {
  const irreversible = Boolean(revision?.irreversible);
  return `
    <form class="stack" data-revision-form>
      <label>Nama migration<input name="name" required maxlength="200" value="${escapeHTML(migration?.name || "")}"></label>
      <label>Deskripsi<input name="description" maxlength="2000" value="${escapeHTML(migration?.description || "")}"></label>
      <label>Up SQL<textarea class="sql" name="up_sql" rows="9" maxlength="1048576" required spellcheck="false">${escapeHTML(revision?.up_sql || "")}</textarea></label>
      <label class="check"><input name="irreversible" type="checkbox" ${irreversible ? "checked" : ""}> Migration tidak dapat di-rollback</label>
      <label>Alasan irreversible<input name="irreversible_reason" maxlength="2000" value="${escapeHTML(revision?.irreversible_reason || "")}"></label>
      <label>Down SQL<textarea class="sql" name="down_sql" rows="7" maxlength="1048576" spellcheck="false">${escapeHTML(revision?.down_sql || "")}</textarea></label>
      <p class="muted">Untuk batch, tulis directive yang akan ditinjau sebagai baris pertama <code>-- dbmp:batch table=TABLE size=N</code>. SQL harus memakai <code>$1</code> sebagai array ID; down SQL batch juga harus memakai <code>$1</code> kecuali migration irreversible.</p>
      <p class="flash flash-error" data-form-error role="alert" hidden></p>
      <button class="btn" type="submit">Simpan revision draft</button>
    </form>`;
}

export async function renderMigrationWorkflow(root, databaseID, user, canEdit) {
  const content = root.querySelector("#migration-workflow-content");
  const jobStorageKey = `dbmp:m2:job:${databaseID}`;
  let storageWarning = "";
  let activeJob = null;
  try {
    const savedJob = sessionStorage.getItem(jobStorageKey);
    if (savedJob) {
      const restored = JSON.parse(savedJob);
      if (restored && restored.job_id && restored.execution_id && restored.migrationID) {
        activeJob = restored;
      } else {
        sessionStorage.removeItem(jobStorageKey);
        storageWarning = "Status job tersimpan tidak valid dan tidak dapat dipulihkan.";
      }
    }
  } catch (error) {
    storageWarning = `Tidak dapat memulihkan status job tersimpan: ${error.message}`;
  }
  let selectedDefinitionID = activeJob?.migrationID || null;
  let listRequestID = 0;
  let detailRequestID = 0;
  let pollTimer = null;
  let pollGeneration = 0;
  const pollInterval = 2000;
  const maxPolls = 300;
  const terminalJobStates = new Set(["success", "failed", "uncertain", "partial", "partially_reverted"]);

  function persistJob(job) {
    try {
      const saved = {
        job_id: job.job_id,
        execution_id: job.execution_id,
        migrationID: job.migrationID,
        operation: job.operation,
        status: job.latest?.status || job.status,
        latest: job.latest ? {
          status: job.latest.status,
          operation: job.latest.operation,
          mode: job.latest.mode,
          attempt: job.latest.attempt,
          batches_done: job.latest.batches_done,
          rows_done: job.latest.rows_done,
        } : undefined,
      };
      sessionStorage.setItem(jobStorageKey, JSON.stringify(saved));
      job.storageError = "";
      storageWarning = "";
      return true;
    } catch (error) {
      job.storageError = error.message;
      return false;
    }
  }

  function stopPolling() {
    pollGeneration += 1;
    if (pollTimer !== null) clearTimeout(pollTimer);
    pollTimer = null;
  }

  function jobStatusMarkup(job) {
    return `<div data-job-status data-job-id="${escapeHTML(job.job_id)}"></div>`;
  }

  function cleanup() {
    stopPolling();
    window.removeEventListener("popstate", cleanup);
    window.removeEventListener("pagehide", cleanup);
  }

  window.addEventListener("popstate", cleanup, { once: true });
  window.addEventListener("pagehide", cleanup, { once: true });

  function renderJobStatus(detail, job, status = job.status) {
    const region = detail.querySelector("[data-job-status]");
    if (!region || region.dataset.jobId !== String(job.job_id)) return;
    const normalized = String(status || "pending").toLowerCase();
    const terminal = terminalJobStates.has(normalized);
    const partial = normalized === "partial" || normalized === "partially_reverted";
    const label = normalized === "success" ? "Berhasil"
      : normalized === "failed" ? "Gagal"
        : normalized === "uncertain" ? "Tidak pasti — perlu rekonsiliasi"
          : normalized === "partial" ? "Sebagian selesai — perlu dilanjutkan manual"
            : normalized === "partially_reverted" ? "Rollback sebagian — perlu dilanjutkan manual"
              : normalized === "running" ? "Sedang berjalan"
                : normalized === "retrying" ? "Akan dicoba kembali"
                  : "Menunggu worker";
    region.className = terminal && normalized === "success"
      ? "flash flash-success"
      : terminal || partial ? "flash flash-error"
        : "notice";
    region.innerHTML = `
      <strong>Job ${escapeHTML(job.job_id)}</strong> — ${escapeHTML(label)}
      <p>Execution: <code>${escapeHTML(job.execution_id || "-")}</code>; operasi ${escapeHTML(job.operation || "apply")}; mode ${escapeHTML(job.mode || "-")}.</p>
      <p>Percobaan ${Number(job.attempt) || 0}; batch selesai ${Number(job.batches_done) || 0}; baris diproses ${Number(job.rows_done) || 0}.</p>
      ${job.last_error ? `<p role="alert">${escapeHTML(job.last_error)}</p>` : ""}
      ${job.pollError ? `<p class="flash-error" role="alert">Gagal membaca status job: ${escapeHTML(job.pollError)}</p>` : ""}
      ${job.storageError ? `<p class="flash-error" role="alert">ID job tidak dapat disimpan untuk dibuka kembali: ${escapeHTML(job.storageError)}</p>` : ""}
      ${!terminal && job.pollStopped ? `<p role="status">Polling berhenti setelah batas waktu UI. Status akhir belum diketahui.</p><button class="btn btn-secondary" type="button" data-resume-job-poll>Lanjutkan pemantauan</button>` : ""}
      ${canEdit && partial && job.can_resume !== false ? `<button class="btn btn-secondary" type="button" data-resume-execution>Lanjutkan dari checkpoint</button>` : ""}
      ${normalized === "uncertain" ? `<p>Jangan menganggap operasi berhasil atau mengirim ulang sebelum kondisi database direkonsiliasi.</p>` : ""}`;
    region.dataset.terminal = String(terminal);
    const resume = region.querySelector("[data-resume-job-poll]");
    if (resume) resume.addEventListener("click", () => startPolling(activeJob, detail));
    const resumeExecution = region.querySelector("[data-resume-execution]");
    if (resumeExecution) resumeExecution.addEventListener("click", async () => {
      resumeExecution.disabled = true;
      try {
        const result = await request(`/api/jobs/${encodeURIComponent(job.job_id)}/resume`, { method: "POST", body: {} });
        activeJob.latest = result.job;
        activeJob.status = result.job.status;
        activeJob.pollStopped = false;
        persistJob(activeJob);
        await loadList("Job dilanjutkan dari checkpoint terakhir yang committed.");
      } catch (error) {
        resumeExecution.disabled = false;
        region.insertAdjacentHTML("beforeend", `<p class="flash flash-error" role="alert">${escapeHTML(error.message)}</p>`);
      }
    });
  }

  function startPolling(job, detail) {
    if (!job || !job.job_id) return;
    stopPolling();
    job.pollStopped = false;
    const generation = pollGeneration;
    let polls = 0;
    renderJobStatus(detail, job, job.latest?.status || job.status);

    const schedule = () => {
      if (generation !== pollGeneration) return;
      pollTimer = setTimeout(async () => {
        pollTimer = null;
        if (generation !== pollGeneration) return;
        polls += 1;
        try {
          const result = await request(`/api/jobs/${encodeURIComponent(job.job_id)}`);
          job.latest = result.job;
          job.pollError = "";
          persistJob(job);
          renderJobStatus(detail, { ...job, ...result.job, pollStopped: job.pollStopped }, result.job.status);
          if (terminalJobStates.has(String(result.job.status).toLowerCase())) {
            job.pollStopped = true;
            stopPolling();
            await loadList(`Job ${String(result.job.status).toUpperCase()}; status migration diperbarui.`);
            return;
          }
        } catch (error) {
          job.pollError = error.message;
          renderJobStatus(detail, { ...job, ...(job.latest || {}), pollStopped: job.pollStopped }, job.latest?.status || job.status);
          if (error.status === 401 || error.status === 403 || error.status === 404) {
            job.pollStopped = true;
            stopPolling();
            return;
          }
        }
        if (polls >= maxPolls) {
          job.pollStopped = true;
          renderJobStatus(detail, job, job.latest?.status || job.status);
          return;
        }
        schedule();
      }, pollInterval);
    };
    schedule();
  }

  async function loadList(message = "") {
    const requestID = ++listRequestID;
    content.innerHTML = `<p class="muted">Memuat migration workflow…</p>`;
    try {
      const result = await request(`/api/databases/${databaseID}/migrations`);
      if (requestID !== listRequestID) return;
      const items = result.items || [];
      if (activeJob && !items.some(item => String(item.id) === String(activeJob.migrationID))) {
        storageWarning = "Migration untuk job tersimpan tidak ditemukan pada database ini.";
        activeJob = null;
        selectedDefinitionID = null;
        try {
          sessionStorage.removeItem(jobStorageKey);
        } catch (error) {
          storageWarning += ` Gagal membersihkan status: ${error.message}`;
        }
      }
      const workflowMessage = [message, storageWarning].filter(Boolean).join(" ");
      content.innerHTML = `
        <div data-workflow-message role="status">${workflowMessage ? escapeHTML(workflowMessage) : ""}</div>
        ${canEdit ? `
          <details class="panel" open>
            <summary>Buat migration draft</summary>
            <div class="stack" data-create-form-wrapper>${revisionForm(null, null)}</div>
          </details>` : ""}
        <h3>Migration workflow</h3>
        ${items.length ? `<div class="table-wrap"><table>
          <thead><tr><th>Nama</th><th>Revision</th><th>Status</th><th></th></tr></thead>
          <tbody>${items.map(item => `<tr>
            <td>${escapeHTML(item.name)}</td>
            <td>${item.revision}</td>
            <td>${escapeHTML(statusLabel(item.workflow_status))}</td>
            <td><button class="btn btn-secondary" type="button" data-open-workflow="${item.id}">Buka workflow</button></td>
          </tr>`).join("")}</tbody>
        </table></div>` : `<div class="empty"><p>Belum ada migration M2 untuk database ini.</p></div>`}
        <div data-workflow-detail></div>`;

      bindCreateForm();
      content.querySelectorAll("[data-open-workflow]").forEach(button => {
        button.addEventListener("click", () => openWorkflow(button.dataset.openWorkflow));
      });
      if (selectedDefinitionID && items.some(item => String(item.id) === String(selectedDefinitionID))) {
        await openWorkflow(selectedDefinitionID);
      }
    } catch (error) {
      if (requestID !== listRequestID) return;
      content.innerHTML = `<p class="flash flash-error" role="alert">${escapeHTML(error.message)}</p>${activeJob ? jobStatusMarkup(activeJob) : ""}`;
      if (activeJob && !terminalJobStates.has(String(activeJob.latest?.status || activeJob.status).toLowerCase())) {
        startPolling(activeJob, content);
      } else if (activeJob) {
        renderJobStatus(content, { ...activeJob, ...(activeJob.latest || {}) }, activeJob.latest?.status || activeJob.status);
      }
    }
  }

  function bindCreateForm() {
    const form = content.querySelector("[data-create-form-wrapper] [data-revision-form]");
    if (!form) return;
    bindIrreversibleFields(form);
    form.addEventListener("submit", async event => {
      event.preventDefault();
      const button = form.querySelector("button[type=submit]");
      const error = form.querySelector("[data-form-error]");
      button.disabled = true;
      error.hidden = true;
      const body = revisionInput(form);
      try {
        const result = await request(`/api/databases/${databaseID}/migrations`, {
          method: "POST",
          body,
        });
        selectedDefinitionID = result.migration.id;
        await loadList("Migration draft berhasil dibuat.");
      } catch (requestError) {
        error.textContent = requestError.message;
        error.hidden = false;
        button.disabled = false;
      }
    });
  }

  function bindIrreversibleFields(form) {
    const irreversible = form.elements.irreversible;
    const downSQL = form.elements.down_sql;
    const reason = form.elements.irreversible_reason;
    const update = () => {
      downSQL.disabled = irreversible.checked;
      downSQL.required = !irreversible.checked;
      reason.required = irreversible.checked;
    };
    irreversible.addEventListener("change", update);
    update();
  }

  function revisionInput(form) {
    const data = Object.fromEntries(new FormData(form));
    data.irreversible = form.elements.irreversible.checked;
    if (data.irreversible) data.down_sql = "";
    else data.irreversible_reason = "";
    return data;
  }

  async function openWorkflow(definitionID, message = "") {
    selectedDefinitionID = definitionID;
    const requestID = ++detailRequestID;
    if (activeJob && String(activeJob.migrationID) !== String(definitionID)) stopPolling();
    const detail = content.querySelector("[data-workflow-detail]");
    if (!detail) return;
    detail.innerHTML = `<p class="muted">Memuat status review…</p>`;
    try {
      const [migrationResult, revisionResult, readinessResult] = await Promise.all([
        request(`/api/migrations/${definitionID}`),
        request(`/api/migrations/${definitionID}/revisions`),
        request(`/api/migrations/${definitionID}/readiness`),
      ]);
      if (requestID !== detailRequestID) return;
      const migration = migrationResult.migration;
      const revisions = revisionResult.items || [];
      const review = readinessResult.workflow;
      const revision = revisions.find(item => item.revision === review.revision) || revisions[revisions.length - 1];
      const groups = review.required_groups || [];
      const editable = canEdit && review.status !== "PENDING_REVIEW";
      const currentUserID = Number(user.id);
      const canSubmitReview = canEdit && review.status === "DRAFT";
      const ready = Boolean(readinessResult.ready && readinessResult.revision);
      const batchConfig = /^-- dbmp:batch table=([A-Za-z_][A-Za-z_0-9]{0,62}) size=([0-9]+)(?:\r?\n|$)/.exec((revision?.up_sql || "").trim());
      const batchSize = batchConfig ? Number(batchConfig[2]) : 0;
      const isBatch = Boolean(batchConfig && Number.isSafeInteger(batchSize) && batchSize >= 1 && batchSize <= 100000 && revision.up_sql.includes("$1"));
      const operationsResult = canEdit ? await request(`/api/migrations/${definitionID}/operations`) : null;
      if (requestID !== detailRequestID) return;
      const operations = operationsResult?.operations;
      const operationReviews = {};
      for (const operation of ["rollback", "forward"]) {
        try {
          operationReviews[operation] = (await request(`/api/migrations/${definitionID}/operations/${operation}/review`)).review;
        } catch (error) {
          if (error.status !== 404) throw error;
        }
      }
      if (operations?.active_job) {
        const sameJob = activeJob?.job_id === operations.active_job.job_id;
        stopPolling();
        activeJob = {
          ...(sameJob ? activeJob : {}),
          job_id: operations.active_job.job_id,
          execution_id: operations.active_job.execution_id,
          migrationID: definitionID,
          operation: operations.active_job.operation,
          status: operations.active_job.status,
          latest: { ...(sameJob ? activeJob.latest : {}), ...operations.active_job },
        };
        persistJob(activeJob);
      }
      const currentJobStatus = String(activeJob?.latest?.status || activeJob?.status || "").toLowerCase();
      const jobIsActive = activeJob && String(activeJob.migrationID) === String(definitionID)
        && (!terminalJobStates.has(currentJobStatus) || currentJobStatus === "uncertain");
      const canEnqueueApply = canEdit && ready && review.status === "APPROVED" && operations?.apply?.available && !jobIsActive;
      const canEnqueueRollback = canEdit && operations?.rollback?.available && !jobIsActive;
      const canEnqueueForward = canEdit && operations?.forward?.available && !jobIsActive;

      detail.innerHTML = `
        <section class="panel" data-migration-detail>
          <header class="pagehead">
            <h3>${escapeHTML(migration.name)}</h3>
            <span class="status ${review.status === "APPROVED" || review.status === "EXECUTED" ? "status-ok" : review.status === "REJECTED" ? "status-fail" : "status-idle"}">${escapeHTML(statusLabel(review.status))}</span>
          </header>
          ${message ? `<p class="flash flash-success" role="status">${escapeHTML(message)}</p>` : ""}
          <p>${escapeHTML(migration.description || "Tanpa deskripsi.")}</p>
          <p><strong>Revision saat ini:</strong> ${review.revision}${isBatch ? ' <span class="tag">Batch</span>' : ""}</p>
          ${isBatch ? `<p>Konfigurasi batch: tabel <code>${escapeHTML(batchConfig[1])}</code>, ukuran maksimal ${batchSize} baris per batch.</p>` : ""}
          ${review.missing_leaders ? `<p class="notice">Belum ada leader pada ${review.missing_leaders} group yang memiliki akses. Platform Admin perlu mengatur leader sebelum review diajukan.</p>` : ""}
          ${ready ? `<p class="flash flash-success" role="status">Semua persyaratan review terpenuhi. Revision ini siap dimasukkan ke antrean worker.</p>` : ""}
          ${groups.length || review.status === "PENDING_REVIEW" ? `
            <h4>Progress persetujuan</h4>
            <p>${review.completed_approvals} dari ${review.required_approvals} group menyetujui; ${review.rejected_approvals} penolakan.</p>
            ${groups.length ? `<div class="table-wrap"><table>
              <thead><tr><th>Group</th><th>Leader snapshot</th><th>Keputusan</th><th>Aksi</th></tr></thead>
              <tbody>${groups.map(group => {
                const decision = group.decision || "pending";
                const mayDecide = review.status === "PENDING_REVIEW" && Number(group.leader_user_id) === currentUserID && decision === "pending";
                return `<tr>
                  <td>${escapeHTML(group.group_name)}</td>
                  <td>User ID ${group.leader_user_id}</td>
                  <td>${escapeHTML(decision === "approved" ? "Disetujui" : decision === "rejected" ? "Ditolak" : "Menunggu")}</td>
                  <td>${mayDecide ? `<button class="btn" type="button" data-approve-group="${group.group_id}">Setujui</button> <button class="btn-danger" type="button" data-reject-group="${group.group_id}">Tolak</button>` : decision === "pending" ? review.status === "PENDING_REVIEW" ? "Menunggu leader group" : "Review sudah ditutup" : "-"}</td>
                </tr>`;
              }).join("")}</tbody>
            </table></div>` : `<p class="notice">Belum ada snapshot group review. Periksa akses database dan leader group.</p>`}
          ` : ""}
          ${revision ? `
            <details><summary>Lihat Up SQL</summary><pre class="sqlblock">${escapeHTML(revision.up_sql)}</pre></details>
            ${revision.irreversible
              ? `<p><strong>Irreversible:</strong> ${escapeHTML(revision.irreversible_reason || "Alasan tidak tersedia.")}</p>`
              : `<details><summary>Lihat Down SQL</summary><pre class="sqlblock">${escapeHTML(revision.down_sql || "")}</pre></details>`}
          ` : ""}
          ${editable ? `
            <details class="workflow-edit">
              <summary>${review.status === "DRAFT" ? "Edit draft" : "Buat revision draft berikutnya"}</summary>
              <div class="stack" data-edit-form-wrapper>${revisionForm(migration, revision)}</div>
            </details>` : ""}
          ${canSubmitReview ? `<button class="btn" type="button" data-submit-review>Kirim untuk review</button>` : ""}
          <section data-execution-controls aria-label="Eksekusi migration">
            ${canEdit ? `
              <h4>Eksekusi worker</h4>
              ${canEnqueueApply ? `<button class="btn" type="button" data-enqueue-operation="apply">Antrekan eksekusi yang disetujui</button>` : ""}
              ${!ready && review.status === "APPROVED" ? `<p class="notice">Eksekusi belum siap. Readiness backend harus menyatakan revision disetujui dan seluruh syarat valid.</p>` : ""}
              ${ready && !operations?.apply?.available && operations?.apply?.reason ? `<p class="notice">Apply belum tersedia: ${escapeHTML(operations.apply.reason)}</p>` : ""}
              ${canEnqueueRollback ? `<button class="btn btn-secondary" type="button" data-enqueue-operation="rollback">Minta persetujuan rollback</button>` : operations?.rollback?.reason ? `<p class="muted">Rollback belum tersedia: ${escapeHTML(operations.rollback.reason)}</p>` : ""}
              ${canEnqueueForward ? `<button class="btn btn-secondary" type="button" data-enqueue-operation="forward">Minta persetujuan forward</button>` : operations?.forward?.reason ? `<p class="muted">Forward belum tersedia: ${escapeHTML(operations.forward.reason)}</p>` : ""}
            ` : ""}
            ${["rollback", "forward"].map(operation => {
              const opReview = operationReviews[operation];
              if (!opReview) return "";
              const opCanSubmit = canEdit && opReview.status === "approved" && !jobIsActive && operations?.[operation]?.available;
              return `<section class="panel" aria-label="Persetujuan ${operation}">
                <h4>Review ${escapeHTML(operation)} #${opReview.id}</h4>
                <p>Status: <strong>${escapeHTML(opReview.status)}</strong>; source execution <code>${escapeHTML(opReview.source_execution_id)}</code>.</p>
                <div class="table-wrap"><table><thead><tr><th>Group</th><th>Leader</th><th>Keputusan</th><th>Aksi</th></tr></thead><tbody>
                  ${(opReview.groups || []).map(group => {
                    const mayDecide = opReview.status === "in_review" && Number(group.leader_user_id) === currentUserID && group.decision === "pending";
                    return `<tr><td>${escapeHTML(group.group_name)}</td><td>User ID ${group.leader_user_id}</td><td>${escapeHTML(group.decision)}</td>
                      <td>${mayDecide ? `<button class="btn" type="button" data-op-review-decision="approved" data-review-id="${opReview.id}" data-group-id="${group.group_id}">Setujui</button> <button class="btn-danger" type="button" data-op-review-decision="rejected" data-review-id="${opReview.id}" data-group-id="${group.group_id}">Tolak</button>` : "-"}</td></tr>`;
                  }).join("")}
                </tbody></table></div>
                ${opCanSubmit ? `<button class="btn" type="button" data-op-review-submit="${opReview.id}">Antrekan ${escapeHTML(operation)} yang disetujui</button>` : ""}
              </section>`;
            }).join("")}
            <div data-job-status ${activeJob && String(activeJob.migrationID) === String(definitionID) ? `data-job-id="${escapeHTML(activeJob.job_id)}"` : ""}></div>
          </section>
          <details>
            <summary>Riwayat revision (${revisions.length})</summary>
            <ol>${revisions.map(item => `<li>Revision ${item.revision} — ${escapeHTML(statusLabel(item.workflow_status))}; dibuat ${escapeHTML(formatTime(item.created_at))}</li>`).join("")}</ol>
          </details>
          <p class="muted">Persetujuan dan eksekusi selalu divalidasi ulang oleh backend; tampilan ini tidak dapat melewati aturan akses atau workflow.</p>
        </section>`;

      const editForm = detail.querySelector("[data-edit-form-wrapper] [data-revision-form]");
      if (editForm) {
        bindIrreversibleFields(editForm);
        editForm.addEventListener("submit", async event => {
          event.preventDefault();
          const button = editForm.querySelector("button[type=submit]");
          const error = editForm.querySelector("[data-form-error]");
          button.disabled = true;
          error.hidden = true;
          try {
            await request(`/api/migrations/${definitionID}`, {
              method: "PUT",
              body: revisionInput(editForm),
            });
            await loadList("Revision draft berhasil disimpan.");
          } catch (requestError) {
            error.textContent = requestError.message;
            error.hidden = false;
            button.disabled = false;
          }
        });
      }

      detail.querySelector("[data-submit-review]")?.addEventListener("click", async event => {
        const button = event.currentTarget;
        button.disabled = true;
        try {
          await request(`/api/migrations/${definitionID}/submit-review`, { method: "POST", body: {} });
          await loadList("Revision dikirim untuk review.");
        } catch (error) {
          detail.querySelector("[data-migration-detail]").insertAdjacentHTML("afterbegin", `<p class="flash flash-error" role="alert">${escapeHTML(error.message)}</p>`);
          button.disabled = false;
        }
      });

      detail.querySelectorAll("[data-approve-group]").forEach(button => button.addEventListener("click", async () => {
        button.disabled = true;
        try {
          await request(`/api/migrations/${definitionID}/revisions/${review.revision}/approvals`, {
            method: "POST",
            body: { group_id: Number(button.dataset.approveGroup) },
          });
          await loadList("Persetujuan berhasil dicatat.");
        } catch (error) {
          detail.querySelector("[data-migration-detail]").insertAdjacentHTML("afterbegin", `<p class="flash flash-error" role="alert">${escapeHTML(error.message)}</p>`);
          button.disabled = false;
        }
      }));

      detail.querySelectorAll("[data-reject-group]").forEach(button => button.addEventListener("click", async () => {
        if (!confirm("Tolak revision ini? Keputusan akan mengakhiri review revision ini.")) return;
        button.disabled = true;
        try {
          await request(`/api/migrations/${definitionID}/revisions/${review.revision}/rejection`, {
            method: "POST",
            body: { group_id: Number(button.dataset.rejectGroup) },
          });
          await loadList("Penolakan berhasil dicatat.");
        } catch (error) {
          detail.querySelector("[data-migration-detail]").insertAdjacentHTML("afterbegin", `<p class="flash flash-error" role="alert">${escapeHTML(error.message)}</p>`);
          button.disabled = false;
        }
      }));

      detail.querySelectorAll("[data-op-review-decision]").forEach(button => button.addEventListener("click", async () => {
        button.disabled = true;
        try {
          await request(`/api/operation-reviews/${button.dataset.reviewId}/approvals`, { method: "POST", body: {
            group_id: Number(button.dataset.groupId), decision: button.dataset.opReviewDecision,
          } });
          await openWorkflow(definitionID, "Keputusan review operasi tercatat.");
        } catch (error) {
          button.disabled = false;
          detail.querySelector("[data-migration-detail]").insertAdjacentHTML("afterbegin", `<p class="flash flash-error" role="alert">${escapeHTML(error.message)}</p>`);
        }
      }));
      detail.querySelectorAll("[data-op-review-submit]").forEach(button => button.addEventListener("click", async () => {
        button.disabled = true;
        try {
          const result = await request(`/api/operation-reviews/${button.dataset.opReviewSubmit}/submit`, { method: "POST", body: {} });
          activeJob = { job_id: result.job_id, execution_id: result.execution_id, migrationID: definitionID, status: result.status || "pending" };
          persistJob(activeJob);
          await loadList("Job operasi yang disetujui diterima antrean.");
        } catch (error) {
          button.disabled = false;
          detail.querySelector("[data-migration-detail]").insertAdjacentHTML("afterbegin", `<p class="flash flash-error" role="alert">${escapeHTML(error.message)}</p>`);
        }
      }));

      if (activeJob && String(activeJob.migrationID) === String(definitionID)) {
        activeJob.latest = activeJob.latest || { status: activeJob.status };
        renderJobStatus(detail, { ...activeJob, ...activeJob.latest, pollStopped: activeJob.pollStopped }, activeJob.latest.status);
        if (!terminalJobStates.has(String(activeJob.latest.status).toLowerCase()) && !activeJob.pollStopped) {
          startPolling(activeJob, detail);
        }
      }

      detail.querySelectorAll("[data-enqueue-operation]").forEach(button => button.addEventListener("click", async () => {
        const operation = button.dataset.enqueueOperation;
        const confirmations = {
          apply: "Antrekan revision yang sudah disetujui untuk dieksekusi oleh worker?",
          rollback: "Antrekan rollback migration aktif terakhir? Down SQL akan dijalankan oleh worker.",
          forward: "Antrekan forward untuk menjalankan kembali revision setelah rollback?",
        };
        if (!confirm(confirmations[operation])) return;
        detail.querySelectorAll("[data-enqueue-operation]").forEach(action => { action.disabled = true; });
        const endpoints = {
          apply: `/api/migrations/${definitionID}/submit`,
          rollback: `/api/migrations/${definitionID}/rollback`,
          forward: `/api/migrations/${definitionID}/forward`,
        };
        let accepted = false;
        try {
          if (operation !== "apply") {
            await request(`/api/migrations/${definitionID}/operations/${operation}/review`, { method: "POST", body: {} });
            accepted = true;
            await openWorkflow(definitionID, `Review ${operation} dibuat. Leader seluruh group akses harus menyetujui sebelum job dapat diantrekan.`);
            return;
          }
          const result = await request(endpoints[operation], { method: "POST", body: {} });
          accepted = true;
          if (!result.job_id || !result.execution_id) throw new Error("Respons antrean tidak menyertakan job ID dan execution ID.");
          activeJob = {
            job_id: result.job_id,
            execution_id: result.execution_id,
            migrationID: definitionID,
            operation,
            status: result.status || "pending",
          };
          persistJob(activeJob);
          await loadList("Job diterima antrean. Eksekusi belum dianggap selesai; status worker sedang dipantau.");
        } catch (error) {
          detail.querySelector("[data-migration-detail]").insertAdjacentHTML("afterbegin", `<p class="flash flash-error" role="alert">${escapeHTML(error.message)}</p>`);
          if (!accepted) detail.querySelectorAll("[data-enqueue-operation]").forEach(action => { action.disabled = false; });
        }
      }));
    } catch (error) {
      if (requestID !== detailRequestID) return;
      detail.innerHTML = `<p class="flash flash-error" role="alert">${escapeHTML(error.message)}</p>${activeJob && String(activeJob.migrationID) === String(definitionID) ? jobStatusMarkup(activeJob) : ""}`;
      if (activeJob && String(activeJob.migrationID) === String(definitionID)
          && !terminalJobStates.has(String(activeJob.latest?.status || activeJob.status).toLowerCase())) {
        startPolling(activeJob, detail);
      }
    }
  }

  function formatTime(value) {
    if (!value) return "-";
    const date = new Date(value);
    return Number.isNaN(date.getTime()) ? String(value) : new Intl.DateTimeFormat("id-ID", {
      timeZone: "UTC", dateStyle: "medium", timeStyle: "short",
    }).format(date) + " UTC";
  }

  await loadList();
}
