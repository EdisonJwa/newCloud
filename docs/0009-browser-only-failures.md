# 0009 — 两个只在真实浏览器里才现形的缺陷

状态：已修复

这两个 bug 是用户在自己的浏览器里发现的，而当时 308 个 Go 测试、类型检查、对比度审计、二十多张
截图全部通过。记录它们，是因为它们失败的方式相同：**测试驱动的流程和用户驱动的流程不是同一条**。

## 缺陷一：打开首页无限刷新

登录后一切正常；但会话不存在时直接打开 `/`，页面永不停下来。

原因是一行重定向：

```tsx
if (err instanceof APIError && err.status === 401) location.href = "/auth/login";
```

真正的登录路由是 `/api/auth/login`。`/auth/login` 没有任何路由匹配，于是落到 SPA 的兜底处理器，
返回 200 和一份完整的应用外壳。外壳启动 → 调 `/api/auth/me` → 401 → 跳 `/auth/login` → 又是外壳。
**没有任何一步报错**，所以用户看到的只是不停转圈。

为什么没被发现：截图工具每次运行都先访问 `/api/auth/login` 完成登录，再去拍页面。它从来没有以
「无会话、直接打开首页」这个用户最常见的起点开始过。

修法：把 401 的重定向指向真实路由 `/api/auth/login`。

## 缺陷二：分享页输入正确密码后没有反应

`POST /s/{token}/verify` 返回 200 并下发 cookie，但页面仍停在密码框。

原因是 cookie 前缀：

```
Set-Cookie: __Host-wc_share_<id>=...; Path=/s/<token>; Secure; HttpOnly
```

RFC 6265bis 规定 `__Host-` 前缀的 cookie **必须** `Path=/` 且不带 `Domain`，否则浏览器直接丢弃，
且不报错。这里的 cookie 刻意作用域限定在 `/s/<token>`，两个条件冲突，于是每个浏览器都把它扔掉。
"密码正确"和"密码错误"在页面上表现完全一样。

**Go 的 cookiejar 不实现前缀规则**，所以服务端测试全部通过。

修法：改用 `__Secure-` 前缀（只要求 Secure，允许任意 Path）。按分享作用域的隔离仍然存在——它由
cookie 名里的 share id 和签名后的票据值保证，而不是由前缀保证。

同一处 `__Host-wc_rcpt_` 也错了，那是邮箱验证码分享的入口，症状相同，用户还没试到。

## 新增的守卫

| 守卫 | 覆盖什么 |
|---|---|
| `demoshot -test-fresh` | 以无会话状态打开 `/`，断言页面在 3 个文档内落定；用 `performance.timeOrigin` 计数真实文档加载，而不是计数探测次数 |
| `demoshot -test-share -share-password` | 真的在表单里输入密码、提交，断言出现下载入口 |
| `TestShareAccessCookieSurvivesABrowser` | 按浏览器规则校验 `Set-Cookie` 前缀，并用返回的 cookie 真的去打开分享 |
| `TestRecipientCodeCookieSurvivesABrowser` | 同上，邮箱验证码那条路径 |

两个守卫都做了「先证明它能抓到 bug」的验证：把修复临时回退，`-test-fresh` 报
`cycled through 7 documents without settling`，`TestShareAccessCookieSurvivesABrowser` 报
`uses the __Host- prefix with Path="/s/..."`，恢复后各自通过。一个抓不到已知 bug 的测试没有价值。

## 教训

第一个 bug 的根因是一行转义错了的路径，第二个是一个规范细节。两者都不是"少测了什么功能",
而是**测的是工具走过的路，不是用户走过的路**。截图工具为了拿到登录态，总是从登录 URL 起步，
于是"用户自己打开首页"这条完全不同的路径从未被执行过。

现在的工具同时具备两个入口：`-test-fresh` 走用户的起点，其余走工具自己的起点。

## 复现命令

```bash
# 首页冷启动必须落定，不能循环
go run ./tools/demoshot -base https://localhost:8443 -out /tmp/cold -test-fresh

# 分享密码门必须放行，并留下解锁后的截图
go run ./tools/demoshot -base https://localhost:8443 -out /tmp/share \
  -test-share -share-password demo2026 <token>
```
