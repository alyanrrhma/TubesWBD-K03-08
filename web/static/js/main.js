import { request, setCSRF } from "./api/client.js";
import { shell } from "./components/layout.js";
import { escapeHTML, route, startRouter } from "./router.js";
import { loginPage } from "./pages/login.js";
import { dashboardPage } from "./pages/dashboard.js";
import { databasePage } from "./pages/database.js";
import { tablePage } from "./pages/table.js";
import { migrationsPage } from "./pages/migrations.js";
import { adminPage } from "./pages/admin.js";

const root=document.querySelector("#app");
let user=null;
route("/login", async r=>loginPage(r,shell));
route("/dashboard", async r=>{if(!user){location.replace("/login");return;}await dashboardPage(r,shell,user);});
route("/databases/:id", async(r,p)=>{if(!user){location.replace("/login");return;}await databasePage(r,shell,user,p);});
route("/databases/:id/tables/:schema/:table", async(r,p)=>{if(!user){location.replace("/login");return;}await tablePage(r,shell,user,p);});
route("/databases/:id/tables/:schema/:table/data", async(r,p)=>{if(!user){location.replace("/login");return;}await tablePage(r,shell,user,{...p,tab:"data"});});
route("/databases/:id/migrations", async(r,p)=>{if(!user){location.replace("/login");return;}await migrationsPage(r,shell,user,p);});
route("/databases/:id/migrations/:version", async(r,p)=>{
 if(!user){location.replace("/login");return;}
 const [m,databaseResult]=await Promise.all([
  request(`/api/databases/${p.id}/migrations/${p.version}`),
  request(`/api/databases/${p.id}`)
 ]);
 const database=databaseResult.database;
 const version=m.Version??m.version;
 r.innerHTML=shell(user,`Migration v${version}`,`
  <nav class="crumbs" aria-label="Lokasi"><a href="/dashboard" data-link>Database</a> / <a href="/databases/${p.id}" data-link>${escapeHTML(database.name)}</a> / <a href="/databases/${p.id}/migrations" data-link>Migration</a> / <span>v${version}</span></nav>
  <nav class="tabs" aria-label="Bagian database">
   <a href="/databases/${p.id}" data-link>Schema dan tabel</a>
   <a href="/databases/${p.id}/migrations" data-link aria-current="page">Migration</a>
  </nav>
  <header class="pagehead"><h1>Migration v${version}</h1></header>
  <p>Status: <span class="status ${m.Status==="SUCCESS"||m.status==="SUCCESS"?"status-ok":"status-fail"}">${escapeHTML(m.Status??m.status)}</span></p>
  <pre class="sqlblock">${escapeHTML(m.SQL??m.sql??"")}</pre>`);
});
route("/admin/users", async r=>{if(!user?.is_platform_admin){location.replace("/dashboard");return;}await adminPage(r,shell,user);});
route("/admin/groups", async r=>{if(!user?.is_platform_admin){location.replace("/dashboard");return;}await adminPage(r,shell,user);});
route("/admin/connections", async r=>{if(!user?.is_platform_admin){location.replace("/dashboard");return;}await adminPage(r,shell,user);});
window.addEventListener("dbmp:unauthorized",()=>{user=null;if(location.pathname!=="/login")location.assign("/login");});
root.addEventListener("click",async e=>{
 if(e.target.closest("[data-logout]")){try{await request("/api/auth/logout",{method:"POST"});}finally{setCSRF("");location.assign("/login");}}
});
async function boot(){
 try{
  const session=await request("/api/auth/me");
  user=session.user;setCSRF(session.csrf_token);
  if(location.pathname==="/")location.replace("/dashboard");
 }catch{user=null;if(location.pathname==="/")location.replace("/login");}
 startRouter(root,shell);
}
boot();
