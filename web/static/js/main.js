import { request, setCSRF } from "./api/client.js";
import { shell } from "./components/layout.js";
import { route, startRouter } from "./router.js";
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
route("/databases/:id/migrations", async(r,p)=>{if(!user){location.replace("/login");return;}await migrationsPage(r,shell,user,p);});
route("/databases/:id/migrations/:version", async(r,p)=>{
 if(!user){location.replace("/login");return;}
 const m=await request(`/api/databases/${p.id}/migrations/${p.version}`);
 r.innerHTML=shell(user,`Migration v${m.Version??m.version}`,`<pre class="sql">${(m.SQL??m.sql??"").replace(/[&<>]/g,c=>({"&":"&amp;","<":"&lt;",">":"&gt;"}[c]))}</pre><p>Status: ${m.Status??m.status}</p>`);
});
route("/admin/users", async r=>{if(!user?.is_platform_admin){location.replace("/dashboard");return;}await adminPage(r,shell,user);});
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
