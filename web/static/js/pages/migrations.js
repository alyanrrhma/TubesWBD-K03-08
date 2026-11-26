import { request } from "../api/client.js";
import { escapeHTML } from "../router.js";
import { loading } from "../components/layout.js";
export async function migrationsPage(root,shell,user,{id}) {
 root.innerHTML=shell(user,"Migration history",loading);
 await draw();

 async function draw() {
  const result=await request(`/api/databases/${id}/migrations`);
  const items=result.items||[];
  root.innerHTML=shell(user,"Migration history",`
   <p class="lede">Riwayat eksekusi linear database ini.</p>
   <section aria-labelledby="migration-editor-title">
    <h2 id="migration-editor-title">SQL editor</h2>
    <form id="migration-form" class="form-stack">
     <label for="migration-sql">SQL migration</label>
     <textarea id="migration-sql" name="sql" class="sql" rows="10" maxlength="1048576" required placeholder="ALTER TABLE ..."></textarea>
     <p id="migration-error" class="flash-error" role="alert" hidden></p>
     <div class="toolbar">
      <button class="btn" type="button" id="explain-migration">EXPLAIN</button>
      <button class="btn" type="button" id="analyze-migration">EXPLAIN ANALYZE</button>
      <button class="btn" type="submit" id="execute-migration">Jalankan migration</button>
     </div>
    </form>
    <section id="migration-plan" hidden aria-live="polite">
     <h2>Execution plan</h2>
     <pre id="migration-plan-output"></pre>
    </section>
   </section>
   <section>
    <h2>Riwayat</h2>
    ${items.length?`<div class="table-wrap"><table><thead><tr><th>Versi</th><th>Status</th><th>Pelaksana</th><th>Waktu</th><th></th></tr></thead><tbody>${items.map(m=>`<tr><td>${m.Version??m.version}</td><td><span class="tag">${escapeHTML(m.Status??m.status)}</span></td><td>${escapeHTML(m.ExecutedByName??m.executed_by_name??m.ExecutedBy??m.executed_by)}</td><td>${escapeHTML(m.ExecutedAt??m.executed_at)}</td><td><a data-link href="/databases/${id}/migrations/${m.Version??m.version}">Detail</a></td></tr>`).join("")}</tbody></table></div>`:'<p class="empty-state">Belum ada migration.</p>'}
   </section>`);

  const form=root.querySelector("#migration-form");
  const sqlInput=root.querySelector("#migration-sql");
  const error=root.querySelector("#migration-error");
  const planSection=root.querySelector("#migration-plan");
  const planOutput=root.querySelector("#migration-plan-output");

  async function explain(analyze) {
   error.hidden=true;
   planSection.hidden=true;
   const button=analyze?root.querySelector("#analyze-migration"):root.querySelector("#explain-migration");
   button.disabled=true;
   try {
    const result=await request(`/api/databases/${id}/migrations/explain`,{method:"POST",body:{sql:sqlInput.value,analyze}});
    planOutput.textContent=result.plan||"";
    planSection.hidden=false;
   } catch(e) {
    error.textContent=e.message;
    error.hidden=false;
   } finally {
    button.disabled=false;
   }
  }

  root.querySelector("#explain-migration").addEventListener("click",()=>explain(false));
  root.querySelector("#analyze-migration").addEventListener("click",()=>{
   if(confirm("EXPLAIN ANALYZE menjalankan statement dalam transaksi yang di-rollback. Beberapa efek eksternal dapat tetap terjadi. Lanjutkan?")) explain(true);
  });
  form.addEventListener("submit",async e=>{
   e.preventDefault();
   if(!confirm("Jalankan migration ini pada database target?")) return;
   const button=root.querySelector("#execute-migration");
   button.disabled=true;
   error.hidden=true;
   try {
    await request(`/api/databases/${id}/migrations`,{method:"POST",body:{sql:sqlInput.value}});
    await draw();
   } catch(ex) {
    error.textContent=ex.message;
    error.hidden=false;
    button.disabled=false;
   }
  });
 }
}
