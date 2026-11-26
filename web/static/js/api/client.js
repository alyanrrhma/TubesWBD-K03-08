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
    const apiError = payload && typeof payload === "object" ? payload.error : null;
    const message = typeof apiError === "string"
      ? apiError
      : apiError && typeof apiError === "object" && typeof apiError.message === "string"
        ? apiError.message
        : payload && typeof payload === "object" && typeof payload.message === "string"
          ? payload.message
          : typeof payload === "string" && payload
            ? payload
            : `Request gagal (${response.status})`;
    const error = new Error(message);
    error.status = response.status;
    if (apiError && typeof apiError === "object" && typeof apiError.code === "string") {
      error.code = apiError.code;
    }
    throw error;
  }
  if (payload && typeof payload === "object"
      && Object.prototype.hasOwnProperty.call(payload, "data")
      && Object.keys(payload).length === 1) {
    return payload.data;
  }
  return payload;
}
