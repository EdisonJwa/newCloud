# Two browser-only defects, and the guards they left behind

Both bugs were found by users in their own browsers at a time when 308 Go tests, type checking, contrast audits, and more than twenty screenshots all passed. They are recorded because they failed in the same way: **the test-driven path and the user-driven path are not the same path**.

## Defect one: infinite refresh when opening the home page

After login, everything works. If no session exists and `/` is opened directly, the page never settles.

The cause was a single redirect line:

```tsx
if (err instanceof APIError && err.status === 401) location.href = "/auth/login";
```

The real login route is `/api/auth/login`. No route matches `/auth/login`, so the request reaches the SPA fallback handler, which returns 200 with a complete application shell. The shell starts, calls `/api/auth/me`, receives 401, navigates to `/auth/login`, and gets the shell again. **No step reports an error**, so the user sees only a spinner.

The problem was missed because the screenshot tool visited `/api/auth/login` on every run to log in before taking page screenshots. It never started from the most common user origin: no session, home page opened directly.

The fix points the 401 redirect at the real route, `/api/auth/login`.

## Defect two: share page does nothing after the correct password

`POST /s/{token}/verify` returns 200 and sets a cookie, but the page remains at the password prompt.

The cause was the cookie prefix:

```text
Set-Cookie: __Host-wc_share_<id>=...; Path=/s/<token>; Secure; HttpOnly
```

RFC 6265bis requires a cookie with the `__Host-` prefix to have `Path=/` and no `Domain`; otherwise the browser discards it without an error. This cookie is deliberately scoped to `/s/<token>`. The two conditions conflict, so every browser throws the cookie away. "Correct password" and "wrong password" look exactly the same on the page.

**Go's cookiejar does not implement prefix rules**, so all server tests passed.

The fix uses the `__Secure-` prefix, which requires only `Secure` and allows any `Path`. Per-share isolation remains: it is provided by the share id in the cookie name and the signed ticket value, not by the prefix.

The `__Host-wc_rcpt_` cookie in the same area was also wrong. It is the entry point for email one-time-code sharing, the symptom is the same, and users had not reached it yet.

## New guards

| Guard | Coverage |
|---|---|
| `demoshot -test-fresh` | Opens `/` with no session and asserts that the page settles within 3 documents; counts real document loads with `performance.timeOrigin`, not probe attempts |
| `demoshot -test-share -share-password` | Actually types the password into the form, submits it, and asserts that a download entry appears |
| `TestShareAccessCookieSurvivesABrowser` | Validates the `Set-Cookie` prefix according to browser rules and opens the share with the returned cookie |
| `TestRecipientCodeCookieSurvivesABrowser` | Performs the same check for the email one-time-code path |

Two of the guards were first verified against the bug they cover. With the fix temporarily reverted, `-test-fresh` reports `cycled through 7 documents without settling`, and `TestShareAccessCookieSurvivesABrowser` reports `uses the __Host- prefix with Path="/s/..."`; both pass again after the fix is restored. A test that cannot catch a known bug has no value.

## Lesson

The first bug came from a single mistyped path; the second from a specification detail. Neither was a case of testing too little of a feature. In both cases, **the tests measured the path the tool takes, not the path the user takes**. The screenshot tool always started from the login URL to obtain an authenticated session, so the entirely different path of a user opening the home page was never executed.

The tool now has two entry points: `-test-fresh` follows the user's origin, and the remaining checks follow the tool's own origin.

## Reproduction commands

```sh
# A cold start of the home page must settle, not loop.
go run ./tools/demoshot -base https://localhost:8443 -out /tmp/cold -test-fresh
```

```sh
# The share password gate must let the request through and leave a screenshot of the unlocked state.
go run ./tools/demoshot -base https://localhost:8443 -out /tmp/share \
  -test-share -share-password demo2026 <token>
```
