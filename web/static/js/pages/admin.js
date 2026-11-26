import { request } from "../api/client.js";
import { escapeHTML } from "../router.js";
import { loading } from "../components/layout.js";

export async function adminPage(root,shell,user) {
  root.innerHTML=shell(user,"Administrasi",loading);
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
        <div class="table-wrap"><table><thead><tr><th>Nama</th><th>Anggota</th></tr></thead><tbody>
        ${groups.map(g=>`<tr><td>${escapeHTML(g.Name)}</td><td>${g.MemberCount}</td></tr>`).join("")}
        </tbody></table></div>
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
