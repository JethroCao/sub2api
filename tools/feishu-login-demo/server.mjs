import crypto from 'node:crypto';
import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));

loadDotEnv(path.join(__dirname, '.env'));

const config = {
  appId: readEnv('FEISHU_APP_ID'),
  appSecret: readEnv('FEISHU_APP_SECRET'),
  redirectUri: readEnv('FEISHU_REDIRECT_URI', 'http://localhost:3000/oauth/feishu/callback'),
  scope: readEnv(
    'FEISHU_SCOPE',
    'contact:user.base:readonly contact:user.email:readonly contact:user.employee_id:readonly contact:user.employee:readonly'
  ),
  host: readEnv('HOST', 'localhost'),
  port: Number(readEnv('PORT', '3000')),
};

const AUTHORIZE_URL = 'https://accounts.feishu.cn/open-apis/authen/v1/authorize';
const USER_TOKEN_URL = 'https://accounts.feishu.cn/oauth/v3/token';
const TENANT_TOKEN_URL = 'https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal';
const USER_INFO_URL = 'https://open.feishu.cn/open-apis/authen/v1/user_info';
const REQUEST_TIMEOUT_MS = 15000;

const server = http.createServer(async (req, res) => {
  try {
    const requestUrl = new URL(req.url ?? '/', `http://${req.headers.host}`);

    if (requestUrl.pathname === '/') {
      return renderHome(req, res);
    }

    if (requestUrl.pathname === '/login') {
      return redirectToFeishu(req, res);
    }

    if (requestUrl.pathname === '/callback' || requestUrl.pathname === '/oauth/feishu/callback') {
      return await handleCallback(req, res, requestUrl);
    }

    writeHtml(res, 404, page('404', '<p>页面不存在。</p><p><a href="/">返回首页</a></p>'));
  } catch (error) {
    writeHtml(res, 500, page('Demo 出错', renderError(error)));
  }
});

server.listen(config.port, config.host, () => {
  console.log(`Feishu login demo running: http://${config.host}:${config.port}`);
  console.log(`Redirect URI: ${config.redirectUri}`);
});

function renderHome(req, res) {
  const missing = [];
  if (!config.appId) missing.push('FEISHU_APP_ID');
  if (!config.appSecret) missing.push('FEISHU_APP_SECRET');

  const body = `
    <section class="card">
      <h1>飞书登录最小 Demo</h1>
      <p>点击下面按钮后，会跳转到飞书授权页。回调成功后，本页会展示当前登录用户的 ID 和邮箱字段。</p>
      ${missing.length > 0 ? `<div class="alert">缺少环境变量：${escapeHtml(missing.join(', '))}</div>` : ''}
      <dl>
        <dt>App ID</dt><dd>${escapeHtml(mask(config.appId))}</dd>
        <dt>Redirect URI</dt><dd><code>${escapeHtml(config.redirectUri)}</code></dd>
        <dt>Scope</dt><dd><code>${escapeHtml(config.scope || '(空)')}</code></dd>
      </dl>
      <p>
        <a class="button ${missing.length > 0 ? 'disabled' : ''}" href="${missing.length > 0 ? '#' : '/login'}">使用飞书登录测试</a>
      </p>
      <p class="hint">如果飞书提示 redirect_uri 不匹配，请到飞书后台把 <code>${escapeHtml(config.redirectUri)}</code> 加入重定向 URL。</p>
    </section>
  `;
  writeHtml(res, 200, page('飞书登录 Demo', body));
}

function redirectToFeishu(_req, res) {
  assertConfig();

  const state = randomBase64Url(24);
  const authorizeUrl = new URL(AUTHORIZE_URL);
  authorizeUrl.searchParams.set('client_id', config.appId);
  authorizeUrl.searchParams.set('response_type', 'code');
  authorizeUrl.searchParams.set('redirect_uri', config.redirectUri);
  authorizeUrl.searchParams.set('state', state);
  if (config.scope.trim()) {
    authorizeUrl.searchParams.set('scope', config.scope.trim());
  }

  res.writeHead(302, {
    Location: authorizeUrl.toString(),
    'Set-Cookie': cookie('feishu_demo_state', state, { maxAge: 600 }),
  });
  res.end();
}

async function handleCallback(req, res, requestUrl) {
  assertConfig();

  const error = requestUrl.searchParams.get('error');
  if (error) {
    return writeHtml(
      res,
      400,
      page('飞书授权被拒绝', `<section class="card"><h1>飞书授权被拒绝</h1><pre>${escapeHtml(error)}</pre><p><a href="/">返回首页</a></p></section>`)
    );
  }

  const code = requestUrl.searchParams.get('code');
  const state = requestUrl.searchParams.get('state');
  const expectedState = parseCookies(req.headers.cookie ?? '').feishu_demo_state;
  logStep('收到飞书回调', { hasCode: Boolean(code), hasState: Boolean(state), hasCookieState: Boolean(expectedState) });

  if (!code) {
    throw new Error('回调缺少 code。');
  }
  if (!state || !expectedState || state !== expectedState) {
    return writeHtml(
      res,
      400,
      page(
        'state 校验失败',
        `<section class="card">
          <h1>state 校验失败</h1>
          <p>请从 <code>http://localhost:${config.port}</code> 首页重新发起登录。不要从 <code>127.0.0.1</code> 打开 demo，否则回调到 <code>localhost</code> 时浏览器不会带上同一个 cookie。</p>
          <p><a class="button" href="/">返回首页</a></p>
        </section>`
      )
    );
  }

  logStep('开始换取 user_access_token');
  const tokenPayload = await postJson('换取 user_access_token', USER_TOKEN_URL, {
    grant_type: 'authorization_code',
    client_id: config.appId,
    client_secret: config.appSecret,
    code,
    redirect_uri: config.redirectUri,
    ...(config.scope.trim() ? { scope: config.scope.trim() } : {}),
  });
  logStep('换取 user_access_token 完成', { code: tokenPayload.code, hasAccessToken: Boolean(tokenPayload.access_token) });

  if (tokenPayload.code !== 0 || !tokenPayload.access_token) {
    return renderResult(res, {
      ok: false,
      step: '获取 user_access_token',
      tokenPayload: sanitize(tokenPayload),
    });
  }

  logStep('开始读取 /authen/v1/user_info');
  const userInfoPayload = await getJson('读取 /authen/v1/user_info', USER_INFO_URL, tokenPayload.access_token);
  logStep('读取 /authen/v1/user_info 完成', { code: userInfoPayload.code, hasData: Boolean(userInfoPayload.data) });
  const userInfo = userInfoPayload.data ?? {};
  const openId = userInfo.open_id;

  let contactDetail = null;
  if (openId) {
    try {
      contactDetail = await tryReadContactDetail(openId);
    } catch (error) {
      contactDetail = {
        step: '应用身份读取用户详情',
        error: error instanceof Error ? error.message : String(error),
      };
    }
  }

  return renderResult(res, {
    ok: true,
    actualScope: tokenPayload.scope || '',
    userInfoPayload: sanitize(userInfoPayload),
    contactDetail: sanitize(contactDetail),
  });
}

async function tryReadContactDetail(openId) {
  logStep('开始获取 tenant_access_token');
  const tenantPayload = await postJson('获取 tenant_access_token', TENANT_TOKEN_URL, {
    app_id: config.appId,
    app_secret: config.appSecret,
  });
  logStep('获取 tenant_access_token 完成', { code: tenantPayload.code, hasTenantToken: Boolean(tenantPayload.tenant_access_token) });

  if (tenantPayload.code !== 0 || !tenantPayload.tenant_access_token) {
    return {
      step: '获取 tenant_access_token',
      payload: tenantPayload,
    };
  }

  const url = new URL(`https://open.feishu.cn/open-apis/contact/v3/users/${encodeURIComponent(openId)}`);
  url.searchParams.set('user_id_type', 'open_id');
  url.searchParams.set('department_id_type', 'open_department_id');

  logStep('开始读取 /contact/v3/users/{open_id}');
  const detail = await getJson('读取 /contact/v3/users/{open_id}', url.toString(), tenantPayload.tenant_access_token);
  logStep('读取 /contact/v3/users/{open_id} 完成', { code: detail.code });
  return {
    step: '应用身份读取 /contact/v3/users/{open_id}',
    payload: detail,
  };
}

function renderResult(res, result) {
  const body = `
    <section class="card">
      <h1>${result.ok ? '飞书登录成功' : '飞书登录失败'}</h1>
      ${result.actualScope ? `<p><strong>实际授权 scope：</strong><code>${escapeHtml(result.actualScope)}</code></p>` : ''}
      ${renderIdentitySummary(result.userInfoPayload?.data)}
      <h2>用户身份接口：/authen/v1/user_info</h2>
      <pre>${escapeHtml(JSON.stringify(result.userInfoPayload ?? result.tokenPayload ?? {}, null, 2))}</pre>
      <h2>应用身份接口：/contact/v3/users/{open_id}</h2>
      <pre>${escapeHtml(JSON.stringify(result.contactDetail ?? { note: '未拿到 open_id，跳过应用身份读取用户详情。' }, null, 2))}</pre>
      <p><a href="/">返回首页</a></p>
    </section>
  `;
  writeHtml(res, result.ok ? 200 : 400, page('飞书登录结果', body));
}

function renderIdentitySummary(userInfo) {
  if (!userInfo) return '';
  const rows = [
    ['name', userInfo.name],
    ['email', userInfo.email],
    ['enterprise_email', userInfo.enterprise_email],
    ['user_id', userInfo.user_id],
    ['open_id', userInfo.open_id],
    ['union_id', userInfo.union_id],
    ['employee_no', userInfo.employee_no],
  ];
  return `
    <h2>关键字段</h2>
    <dl class="summary">
      ${rows
        .map(([key, value]) => `<dt>${escapeHtml(key)}</dt><dd>${escapeHtml(value || '(空)')}</dd>`)
        .join('')}
    </dl>
  `;
}

function writeHtml(res, statusCode, html) {
  const body = Buffer.from(html);
  logStep('写入 HTML 响应', { statusCode, bytes: body.length });
  res.writeHead(statusCode, {
    'Content-Type': 'text/html; charset=utf-8',
    'Cache-Control': 'no-store',
    'Content-Length': body.length,
    Connection: 'close',
  });
  res.end(body);
}

async function postJson(label, url, body) {
  const response = await fetchWithTimeout(label, url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json; charset=utf-8' },
    body: JSON.stringify(body),
  });
  return readJsonResponse(response);
}

async function getJson(label, url, bearerToken) {
  const response = await fetchWithTimeout(label, url, {
    method: 'GET',
    headers: {
      Authorization: `Bearer ${bearerToken}`,
      'Content-Type': 'application/json; charset=utf-8',
    },
  });
  return readJsonResponse(response);
}

async function fetchWithTimeout(label, url, options) {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), REQUEST_TIMEOUT_MS);
  try {
    return await fetch(url, {
      ...options,
      signal: controller.signal,
    });
  } catch (error) {
    if (error?.name === 'AbortError') {
      throw new Error(`${label} 超时：${REQUEST_TIMEOUT_MS / 1000} 秒内没有收到飞书响应。`);
    }
    throw error;
  } finally {
    clearTimeout(timeout);
  }
}

async function readJsonResponse(response) {
  const text = await response.text();
  let payload;
  try {
    payload = JSON.parse(text);
  } catch {
    payload = { raw: text };
  }
  if (!response.ok && typeof payload === 'object' && payload !== null) {
    payload.http_status = response.status;
  }
  return payload;
}

function logStep(message, extra = {}) {
  const suffix = Object.keys(extra).length > 0 ? ` ${JSON.stringify(extra)}` : '';
  console.log(`[${new Date().toISOString()}] ${message}${suffix}`);
}

function assertConfig() {
  if (!config.appId || !config.appSecret) {
    throw new Error('请先配置 FEISHU_APP_ID 和 FEISHU_APP_SECRET。');
  }
}

function loadDotEnv(filePath) {
  if (!fs.existsSync(filePath)) return;
  const content = fs.readFileSync(filePath, 'utf8');
  for (const rawLine of content.split(/\r?\n/)) {
    const line = rawLine.trim();
    if (!line || line.startsWith('#')) continue;
    const index = line.indexOf('=');
    if (index <= 0) continue;
    const key = line.slice(0, index).trim();
    let value = line.slice(index + 1).trim();
    if ((value.startsWith('"') && value.endsWith('"')) || (value.startsWith("'") && value.endsWith("'"))) {
      value = value.slice(1, -1);
    }
    if (!(key in process.env)) {
      process.env[key] = value;
    }
  }
}

function readEnv(key, fallback = '') {
  return process.env[key] ?? fallback;
}

function sanitize(value) {
  if (Array.isArray(value)) return value.map(sanitize);
  if (!value || typeof value !== 'object') return value;

  const out = {};
  for (const [key, item] of Object.entries(value)) {
    const normalized = key.toLowerCase();
    if (
      normalized.includes('token') ||
      normalized.includes('secret') ||
      normalized === 'authorization' ||
      normalized === 'app_secret'
    ) {
      out[key] = item ? '[已隐藏]' : item;
      continue;
    }
    out[key] = sanitize(item);
  }
  return out;
}

function randomBase64Url(bytes) {
  return crypto.randomBytes(bytes).toString('base64url');
}

function parseCookies(header) {
  const cookies = {};
  for (const pair of header.split(';')) {
    const index = pair.indexOf('=');
    if (index <= 0) continue;
    const key = pair.slice(0, index).trim();
    const value = pair.slice(index + 1).trim();
    cookies[key] = decodeURIComponent(value);
  }
  return cookies;
}

function cookie(name, value, options = {}) {
  const parts = [
    `${name}=${encodeURIComponent(value)}`,
    'Path=/',
    'HttpOnly',
    'SameSite=Lax',
  ];
  if (options.maxAge) parts.push(`Max-Age=${options.maxAge}`);
  return parts.join('; ');
}

function page(title, body) {
  return `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>${escapeHtml(title)}</title>
  <style>
    :root {
      color-scheme: light;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      color: #172033;
      background: #f5f7fb;
    }
    *, *::before, *::after {
      box-sizing: border-box;
    }
    body {
      margin: 0;
      min-height: 100vh;
      padding: 32px;
      background: #f5f7fb;
    }
    .card {
      width: 100%;
      max-width: 920px;
      margin: 0 auto;
      background: #fff;
      border: 1px solid #e5e8f0;
      border-radius: 8px;
      box-shadow: 0 12px 32px rgba(17, 24, 39, 0.08);
      padding: 28px;
    }
    h1 {
      margin: 0 0 14px;
      font-size: 24px;
      line-height: 1.25;
    }
    h2 {
      margin-top: 24px;
      font-size: 16px;
    }
    p {
      color: #5b6475;
      line-height: 1.7;
    }
    dl {
      display: grid;
      grid-template-columns: 140px 1fr;
      gap: 10px 16px;
      margin: 22px 0;
    }
    dt {
      color: #687184;
    }
    dd {
      margin: 0;
      min-width: 0;
      overflow-wrap: anywhere;
    }
    .summary {
      grid-template-columns: 180px 1fr;
      padding: 16px;
      border: 1px solid #e5e8f0;
      border-radius: 8px;
      background: #f8fafc;
    }
    code, pre {
      font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", monospace;
    }
    pre {
      overflow: auto;
      max-height: 420px;
      max-width: 100%;
      padding: 16px;
      border-radius: 8px;
      background: #111827;
      color: #e5e7eb;
      line-height: 1.55;
      font-size: 13px;
      white-space: pre-wrap;
      overflow-wrap: anywhere;
    }
    .button {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      min-height: 42px;
      padding: 0 18px;
      border-radius: 7px;
      background: #0f9f8f;
      color: #fff;
      text-decoration: none;
      font-weight: 650;
    }
    .button.disabled {
      pointer-events: none;
      background: #a8b0bd;
    }
    .alert {
      margin: 18px 0;
      padding: 12px 14px;
      border-radius: 8px;
      background: #fff7ed;
      color: #9a3412;
      border: 1px solid #fed7aa;
    }
    .hint {
      font-size: 13px;
    }
  </style>
</head>
<body>${body}</body>
</html>`;
}

function renderError(error) {
  const message = error instanceof Error ? error.message : String(error);
  return `<section class="card"><h1>Demo 出错</h1><pre>${escapeHtml(message)}</pre><p><a href="/">返回首页</a></p></section>`;
}

function escapeHtml(value) {
  return String(value)
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
    .replaceAll("'", '&#39;');
}

function mask(value) {
  if (!value) return '(未配置)';
  if (value.length <= 10) return `${value.slice(0, 2)}***`;
  return `${value.slice(0, 8)}...${value.slice(-4)}`;
}
