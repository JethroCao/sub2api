# 飞书登录最小 Demo

这个 demo 只用于验证飞书 OAuth 登录后能不能拿到当前登录用户的 ID 和邮箱。

它不会写数据库，也不会接入 Sub2API 正式后端。App Secret 只从 `.env` 或环境变量读取，不要提交。

## 1. 配置飞书回调地址

在飞书开发者后台，把下面地址加入应用的重定向 URL：

```text
http://localhost:3000/oauth/feishu/callback
```

如果你改了 `PORT` 或 `FEISHU_REDIRECT_URI`，飞书后台也必须配置成完全一致。

## 2. 配置本地环境变量

```bash
cd /Users/caochenxu/Work/open-source/sub2api/tools/feishu-login-demo
cp .env.example .env
```

编辑 `.env`：

```text
FEISHU_APP_ID=你的 App ID
FEISHU_APP_SECRET=你的 App Secret
FEISHU_REDIRECT_URI=http://localhost:3000/oauth/feishu/callback
FEISHU_SCOPE=contact:user.base:readonly contact:user.email:readonly contact:user.employee_id:readonly contact:user.employee:readonly
PORT=3000
```

建议权限：

- `contact:user.base:readonly`
- `contact:user.email:readonly`
- `contact:user.employee_id:readonly`
- `contact:user.employee:readonly`

其中：

- OAuth 登录后的 `/authen/v1/user_info` 主要验证“当前登录用户”能不能返回邮箱。
- `email` 对应用户联系方式邮箱；`enterprise_email` 对应企业邮箱，需要 `contact:user.employee:readonly`，并且企业已启用飞书邮箱服务。
- 已知 `open_id` 后，demo 还会尝试用应用身份调用 `/contact/v3/users/{open_id}`，验证应用身份读取用户详情是否能返回邮箱。

## 3. 启动

```bash
node server.mjs
```

打开：

```text
http://localhost:3000
```

点“使用飞书登录测试”。

## 4. 看结果

回调成功后页面会展示：

- 授权返回的实际 scope。
- `/authen/v1/user_info` 返回的当前用户信息。
- 如果拿到了 `open_id`，再展示 `/contact/v3/users/{open_id}` 的应用身份读取结果。

页面会自动隐藏 access token、refresh token、App Secret 等敏感字段。

## 5. 常见问题

### redirect_uri mismatch

飞书后台配置的重定向 URL 必须和 `.env` 里的 `FEISHU_REDIRECT_URI` 完全一致。

### 20027

授权链接里的 `scope` 包含应用后台没有开通的用户身份权限。删掉未开通的 scope，或去飞书后台开通。

### user_info 没有邮箱

一般是 `contact:user.email:readonly` 没有以用户身份开通，或授权链接没有带这个 scope。

### contact/v3/users 失败

这条是应用身份读取用户详情，依赖应用身份的通讯录权限和可访问数据范围。它失败不一定影响首次登录绑定，只说明后台组织同步/应用身份读取详情还需要补权限。
