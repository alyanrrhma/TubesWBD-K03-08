let csrfToken = "";

export function setCSRF(token) { csrfToken = token || ""; }
export function getCSRF() { return csrfToken; }

export async function request(path, options = {}) {
  const headers = new Headers(options.headers || {});
  const method = (options.method || "GET").toUpperCase();
  if (options.body && !(options.body instanceof FormData)) headers.set("Content-Type", "application/json");
  if (method !== "GET" && method !== "HEAD" && csrfToken) headers.set("X-CSRF-Token", csrfToken);
  const response = await fetch(path, {
    ...options, method, headers, credentials: "same-origin",
    body: options.body && typeof options.body !== "string" && !(options.body instanceof FormData)
      ? JSON.stringify(options.body) : options.body,
  });
  const contentType = response.headers.get("content-type") || "";
  const payload = contentType.includes("application/json")
    ? await response.json().catch(() => ({}))
    : await response.text();
  if (!response.ok) {
    if (response.status === 401) window.dispatchEvent(new CustomEvent("dbmp:unauthorized"));
    const message = payload && typeof payload === "object" ? payload.error : payload;
    throw new Error(message || `Request gagal (${response.status})`);
  }
  return payload;
}
