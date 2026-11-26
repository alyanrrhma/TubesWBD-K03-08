import { request, setCSRF } from "../api/client.js";
export function loginPage(root, shell) {
  root.innerHTML = shell(null, "Masuk", `<section class="login">
    <h1>Masuk</h1>
    <p class="lede">Gunakan akun yang diberikan Platform Admin.</p>
    <form id="login-form" class="panel form-stack">
      <label>Username<input name="username" autocomplete="username" autocapitalize="none" spellcheck="false" required maxlength="64"></label>
      <label>Password<input name="password" type="password" autocomplete="current-password" required></label>
      <p id="login-error" class="flash flash-error" role="alert" hidden></p>
      <button class="btn" type="submit">Masuk</button>
    </form>
  </section>`);
  root.querySelector("#login-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const form = event.currentTarget, button = form.querySelector("button"), error = root.querySelector("#login-error");
    button.disabled = true; button.textContent = "Memeriksa…"; error.hidden = true;
    try {
      const result = await request("/api/auth/login", { method:"POST", body:Object.fromEntries(new FormData(form)) });
      setCSRF(result.csrf_token);
      location.assign("/dashboard");
    } catch (e) { error.textContent = e.message; error.hidden = false; }
    finally { button.disabled = false; button.textContent = "Masuk"; }
  });
}
