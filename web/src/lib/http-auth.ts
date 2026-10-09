// Basic credentials belong to the browser, never to application JavaScript.
// Keep the JWT when the outer gate challenges, rather than redirecting to a
// login page that is protected by the same gate and causing a redirect loop.
export class HTTPEntryAuthError extends Error {
  constructor() {
    super("HTTP 入口认证失败，请重新打开系统并在浏览器认证框中输入已配置的验证账号密码。");
    this.name = "HTTPEntryAuthError";
  }
}

export async function entryAwareFetch(input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
  const response = await fetch(input, init);
  if (response.status === 401 && /^Basic(?:\s|$)/i.test(response.headers.get("WWW-Authenticate") ?? "")) {
    throw new HTTPEntryAuthError();
  }
  return response;
}
