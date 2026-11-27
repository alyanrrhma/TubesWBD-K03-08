import { request } from "../api/client.js";
import { escapeHTML } from "../router.js";
import { loading } from "../components/layout.js";

export async function adminPage(root,shell,user) {
  root.innerHTML=shell(user,"Administrasi",loading);
  const accessLevelLabel = level => {
    if (level === 2 || level === "VIEW_EDIT") return "View + Edit";
    if (level === 1 || level === "VIEW_ONLY") return "View Only";
    return "Unknown";
  };
  async function draw(message="") {
    const [users,groups,connections]=await Promise.all([
      request("/api/admin/users"),request("/api/admin/groups"),request("/api/admin/connections")
    ]);
    root.innerHTML=shell(user,"Administrasi",`
      <h1>Administrasi</h1>
      ${message?`<p class="flash" role="status">${escapeHTML(message)}</p>`:""}
      <section id="users"><h2>Pengguna</h2>
        <form id="create-user" class="inline-form">
          <label>Username<input name="username" required minlength="3" maxlength="64"></label>
          <label>Password<input name="password" type="password" required minlength="8"></label>
          <label class="check"><input name="is_platform_admin" type="checkbox"> Platform Admin</label>
          <button class="btn">Tambah pengguna</button>
        </form>
        <div class="table-wrap"><table><thead><tr><th>Username</th><th>Role</th><th>Aksi</th></tr></thead><tbody>
        ${users.map(u=>`<tr><td>${escapeHTML(u.Username)}</td><td>${u.IsAdmin?"Admin":"User"}</td><td>${u.ID===user.id?"Akun aktif":`<button class="btn-danger" data-delete-user="${u.ID}">Hapus</button>`}</td></tr>`).join("")}
        </tbody></table></div>
      </section>
      <section id="groups"><h2>Group</h2>
        <form id="create-group" class="inline-form"><label>Nama group<input name="name" required maxlength="64"></label><button class="btn">Buat group</button></form>
        <div class="table-wrap"><table><thead><tr><th>Nama</th><th>Anggota</th><th>Approval</th></tr></thead><tbody>
        ${groups.map(g=>`<tr><td>${escapeHTML(g.Name)}</td><td data-group-count="${g.ID}">${g.MemberCount}</td><td><button class="btn btn-secondary" type="button" data-manage-group="${g.ID}">Atur group</button></td></tr>`).join("")}
        </tbody></table></div>
        <div id="group-details" class="notice" aria-live="polite">Pilih group untuk melihat dan mengatur leader approval.</div>
      </section>
      <section id="connections"><h2>Koneksi database</h2>
        <form id="create-connection" class="inline-form connection-form">
          <label>Nama koneksi<input name="name" required></label><label>Host<input name="host" required></label>
          <label>Port<input name="port" type="number" value="5432" required></label><label>Database<input name="database" required></label>
          <label>Username DB<input name="username" required></label><label>Password DB<input name="password" type="password" required></label>
          <label>SSL<select name="ssl_mode"><option>prefer</option><option>require</option><option>disable</option></select></label>
          <button class="btn">Tambah koneksi</button>
        </form>
        <div class="table-wrap"><table><thead><tr><th>Nama</th><th>Host</th><th>Database</th><th>Status</th><th>Aksi</th></tr></thead><tbody>
        ${connections.map(c=>`<tr><td>${escapeHTML(c.Name)}</td><td>${escapeHTML(c.Host)}:${c.Port}</td><td>${escapeHTML(c.Database)}</td><td>${c.Active?"Aktif":"Tidak aktif"}</td><td><button class="btn-danger" data-delete-connection="${c.ID}">Hapus</button></td></tr>`).join("")}
        </tbody></table></div>
      </section>`);
    const groupDetails = root.querySelector("#group-details");
    let activeGroupRequest = 0;
    async function showGroupDetails(groupID, message = "") {
      const requestID = ++activeGroupRequest;
      groupDetails.textContent = "Memuat anggota dan leader group…";
      try {
        const [detail, leaderResponse] = await Promise.all([
          request(`/api/admin/groups/${groupID}`),
          request(`/api/groups/${groupID}/leader`),
        ]);
        if (requestID !== activeGroupRequest) return;
        const group = detail.group;
        const members = detail.members || [];
        const candidates = detail.candidates || [];
        const accessRows = detail.access || [];
        const connections = detail.connections || [];
        const countCell = root.querySelector(`[data-group-count="${groupID}"]`);
        if (countCell) countCell.textContent = group.MemberCount;
        const leaderID = leaderResponse.leader?.leader_user_id;
        const leader = members.find(member => member.ID === leaderID);
        groupDetails.className = "notice";
        groupDetails.removeAttribute("role");
        groupDetails.innerHTML = `
          <h3>${escapeHTML(group.Name)} — leader approval</h3>
          ${message ? `<p role="status">${escapeHTML(message)}</p>` : ""}
          <p>${leaderID == null
            ? "Belum ada leader. Migration yang memakai group ini belum dapat diajukan untuk review."
            : `Leader saat ini: ${escapeHTML(leader?.Username || `User ID ${leaderID}`)}`}</p>
          <h4>Akses database</h4>
          ${accessRows.length ? `
            <div class="table-wrap">
              <table>
                <thead><tr><th>Database</th><th>Level</th><th>Aksi</th></tr></thead>
                <tbody>${accessRows.map(access => `
                  <tr data-group-access-row="${access.DatabaseID}">
                    <td>${escapeHTML(access.DatabaseName)}</td>
                    <td>${accessLevelLabel(access.Level)}</td>
                    <td><button class="btn-danger" type="button" data-revoke-group-access="${access.DatabaseID}">Cabut akses</button></td>
                  </tr>`).join("")}</tbody>
              </table>
            </div>` : "<p>Group ini belum punya akses ke database mana pun.</p>"}
          ${connections.length ? `
            <form id="group-access-form" class="inline-form">
              <label for="group-access-database">Database
                <select id="group-access-database" name="database_id" required>
                  ${connections.map(connection => {
                    const current = accessRows.find(access => access.DatabaseID === connection.ID);
                    return `<option value="${connection.ID}">${escapeHTML(connection.Name)}${current ? ` (akses saat ini: ${accessLevelLabel(current.Level)})` : ""}</option>`;
                  }).join("")}
                </select>
              </label>
              <label for="group-access-level">Level akses
                <select id="group-access-level" name="level" required>
                  <option value="VIEW_ONLY">View Only</option>
                  <option value="VIEW_EDIT">View + Edit</option>
                </select>
              </label>
              <button class="btn" type="submit">Simpan akses</button>
            </form>
            <p class="muted">Menyimpan akses pada database yang sudah terdaftar akan memperbarui levelnya. Jika pengguna ada di beberapa group, akses tertinggi yang berlaku.</p>` : "<p class=\"muted\">Belum ada koneksi database.</p>"}
          <h4>Anggota group</h4>
          ${members.length
            ? `<ul>${members.map(member => `<li>${escapeHTML(member.Username)}${member.ID === leaderID ? " — leader approval" : ""}</li>`).join("")}</ul>`
            : "<p>Belum ada anggota group.</p>"}
          ${candidates.length ? `
            <form id="group-member-form" class="inline-form">
              <label for="group-member">Tambahkan pengguna</label>
              <select id="group-member" name="user_id" required>
                ${candidates.map(candidate => `<option value="${candidate.ID}">${escapeHTML(candidate.Username)}</option>`).join("")}
              </select>
              <button class="btn" type="submit">Tambahkan anggota</button>
            </form>` : "<p class=\"muted\">Semua pengguna sudah menjadi anggota group ini.</p>"}</p>
          ${members.length ? `
            <form id="group-leader-form" class="inline-form">
              <label for="group-leader">Pilih leader dari anggota group</label>
              <select id="group-leader" name="user_id" required>
                ${members.map(member => `<option value="${member.ID}" ${member.ID === leaderID ? "selected" : ""}>${escapeHTML(member.Username)}</option>`).join("")}
              </select>
              <button class="btn" type="submit">${leaderID == null ? "Tetapkan leader" : "Simpan leader"}</button>
            </form>` : `<p>Group ini belum memiliki anggota untuk dijadikan leader.</p>`}
          <p class="muted">Perubahan leader hanya berlaku untuk review baru. Review yang sedang berjalan memakai snapshot leader saat diajukan.</p>`;
        const accessForm = groupDetails.querySelector("#group-access-form");
        if (accessForm) {
          accessForm.addEventListener("submit", async event => {
            event.preventDefault();
            const formData = Object.fromEntries(new FormData(accessForm));
            const submit = accessForm.querySelector("button");
            submit.disabled = true;
            try {
              await request(`/api/admin/groups/${groupID}/access`, {
                method: "POST",
                body: { database_id: Number(formData.database_id), level: formData.level },
              });
              await showGroupDetails(groupID, "Akses database berhasil disimpan.");
            } catch (error) {
              groupDetails.insertAdjacentHTML("afterbegin", `<p class="flash flash-error" role="alert">${escapeHTML(error.message)}</p>`);
              submit.disabled = false;
            }
          });
        }
        groupDetails.querySelectorAll("[data-revoke-group-access]").forEach(button => {
          button.addEventListener("click", async () => {
            const databaseID = Number(button.dataset.revokeGroupAccess);
            const access = accessRows.find(row => row.DatabaseID === databaseID);
            if (!confirm(`Cabut akses ${access?.DatabaseName || "database ini"} dari group?`)) return;
            button.disabled = true;
            try {
              await request(`/api/admin/groups/${groupID}/access/${databaseID}`, { method: "DELETE" });
              await showGroupDetails(groupID, "Akses database berhasil dicabut.");
            } catch (error) {
              groupDetails.insertAdjacentHTML("afterbegin", `<p class="flash flash-error" role="alert">${escapeHTML(error.message)}</p>`);
              button.disabled = false;
            }
          });
        });
        const memberForm = groupDetails.querySelector("#group-member-form");
        if (memberForm) {
          memberForm.addEventListener("submit", async event => {
            event.preventDefault();
            const userID = Number(new FormData(memberForm).get("user_id"));
            const submit = memberForm.querySelector("button");
            submit.disabled = true;
            try {
              await request(`/api/admin/groups/${groupID}/members`, {
                method: "POST",
                body: { user_id: userID },
              });
              await showGroupDetails(groupID, "Anggota group berhasil ditambahkan.");
            } catch (error) {
              groupDetails.insertAdjacentHTML("afterbegin", `<p class="flash flash-error" role="alert">${escapeHTML(error.message)}</p>`);
              submit.disabled = false;
            }
          });
        }
        const form = groupDetails.querySelector("#group-leader-form");
        if (form) {
          form.addEventListener("submit", async event => {
            event.preventDefault();
            const userID = Number(new FormData(form).get("user_id"));
            if (leaderID != null && userID !== leaderID && !confirm("Ganti leader group ini? Review yang sedang berjalan tidak akan berubah.")) return;
            const submit = form.querySelector("button");
            submit.disabled = true;
            try {
              await request(`/api/admin/groups/${groupID}/leader`, {
                method: "PUT",
                body: { user_id: userID },
              });
              await showGroupDetails(groupID, "Leader approval berhasil diperbarui.");
            } catch (error) {
              groupDetails.insertAdjacentHTML("afterbegin", `<p class="flash flash-error" role="alert">${escapeHTML(error.message)}</p>`);
              submit.disabled = false;
            }
          });
        }
      } catch (error) {
        if (requestID !== activeGroupRequest) return;
        groupDetails.className = "flash flash-error";
        groupDetails.setAttribute("role", "alert");
        groupDetails.textContent = error.message;
      }
    }
    root.querySelectorAll("[data-manage-group]").forEach(button => button.addEventListener("click", () => {
      showGroupDetails(button.dataset.manageGroup);
    }));
    root.querySelector("#create-user").addEventListener("submit",async e=>{
      e.preventDefault();const f=e.currentTarget;const data=Object.fromEntries(new FormData(f));data.is_platform_admin=f.elements.is_platform_admin.checked;
      try{await request("/api/admin/users",{method:"POST",body:data});await draw("Pengguna berhasil dibuat.");}catch(err){alert(err.message);}
    });
    root.querySelector("#create-group").addEventListener("submit",async e=>{
      e.preventDefault();try{await request("/api/admin/groups",{method:"POST",body:Object.fromEntries(new FormData(e.currentTarget))});await draw("Group berhasil dibuat.");}catch(err){alert(err.message);}
    });
    root.querySelector("#create-connection").addEventListener("submit",async e=>{
      e.preventDefault();const data=Object.fromEntries(new FormData(e.currentTarget));data.port=Number(data.port);
      try{await request("/api/admin/connections",{method:"POST",body:data});await draw("Koneksi berhasil dibuat.");}catch(err){alert(err.message);}
    });
    root.querySelectorAll("[data-delete-user]").forEach(btn=>btn.addEventListener("click",async()=>{
      if(!confirm("Hapus pengguna ini?"))return;try{await request(`/api/admin/users/${btn.dataset.deleteUser}`,{method:"DELETE"});await draw("Pengguna dihapus.");}catch(err){alert(err.message);}
    }));
    root.querySelectorAll("[data-delete-connection]").forEach(btn=>btn.addEventListener("click",async()=>{
      if(!confirm("Hapus koneksi ini?"))return;try{await request(`/api/admin/connections/${btn.dataset.deleteConnection}`,{method:"DELETE"});await draw("Koneksi dihapus.");}catch(err){alert(err.message);}
    }));
  }
  await draw();
}
