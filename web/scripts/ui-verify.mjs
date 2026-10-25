// Browser verification for the Cirrus/Serein interface.
//
// This is an *evidence* script, not a test suite with assertions baked into prose: it drives
// the real application in a real browser, records what it saw, and fails on a console error, a
// failed request, a horizontally overflowing page or an unlabelled interactive control.
//
// It runs against a live demo instance (tools/demoinmem), because the interface's whole job is
// to talk to that server: a static fixture would let a wrong URL, a missing field or a broken
// `from=` contract pass.
//
// Usage:
//   node scripts/ui-verify.mjs --base https://localhost:18543 --out artifacts/ui
//
// The certificate is self-signed and the demo is throwaway, so `ignoreHTTPSErrors` is correct
// here and nowhere else.
//
// ## Why playwright-core is resolved by path
//
// `web/` deliberately has no browser-automation dependency: the client has three runtime
// dependencies and the build is `tsc` plus `vite`. This script is a review tool, so it borrows
// the copy that already exists in the design-system checkout (`cirrus-ui/node_modules`) or one
// named with `--playwright`. That is a path, not a dependency, and the script says so when it
// cannot find one instead of failing on a bare import.

import { mkdirSync, writeFileSync } from "node:fs";
import { resolve, join } from "node:path";
import { pathToFileURL } from "node:url";

function arg(name, fallback) {
  const i = process.argv.indexOf(`--${name}`);
  return i >= 0 && process.argv[i + 1] ? process.argv[i + 1] : fallback;
}

const CANDIDATES = [
  arg("playwright", ""),
  process.env.PLAYWRIGHT_CORE ?? "",
  resolve("..", "cirrus-ui", "node_modules", "playwright-core"),
  resolve("node_modules", "playwright-core"),
].filter(Boolean);

let chromium = null;
let loadedFrom = "";
for (const candidate of CANDIDATES) {
  try {
    const mod = await import(pathToFileURL(join(candidate, "index.js")).href);
    chromium = mod.chromium ?? mod.default?.chromium;
    loadedFrom = candidate;
    if (chromium) break;
  } catch {
    /* try the next candidate */
  }
}
if (!chromium) {
  console.error(
    "ui-verify: playwright-core not found.\n" +
      "  Pass --playwright <dir> or set PLAYWRIGHT_CORE to a playwright-core package directory.\n" +
      `  Tried:\n    ${CANDIDATES.join("\n    ")}`,
  );
  process.exit(2);
}
console.log(`using playwright-core from ${loadedFrom}`);

const BASE = arg("base", "https://localhost:18543");
const OUT = resolve(arg("out", "artifacts/ui"));
const CHROME = arg("chrome", process.env.CHROME_PATH || "");
const ONLY = arg("only", "");

mkdirSync(OUT, { recursive: true });

const results = [];
const problems = [];

function record(name, ok, detail) {
  results.push({ name, ok, detail });
  if (!ok) problems.push(`${name}: ${detail}`);
}

/** The screens to capture, in the order a reviewer would walk them. */
const SHOTS = [
  { name: "01-home-light", path: "/", theme: "light", width: 1440, height: 900 },
  { name: "02-files-light", path: "/?screen=files", theme: "light", width: 1440, height: 900 },
  { name: "03-files-inspector", path: "/?screen=files&inspector=1&pane=info", theme: "light", width: 1440, height: 900 },
  { name: "04-sharing-received", path: "/?screen=sharing&tab=received", theme: "light", width: 1440, height: 900 },
  { name: "05-sharing-managed", path: "/?screen=sharing&tab=managed", theme: "light", width: 1440, height: 900 },
  { name: "06-settings-trash", path: "/?screen=settings&settings=trash", theme: "light", width: 1440, height: 900 },
  { name: "07-settings-activity", path: "/?screen=settings&settings=activity", theme: "light", width: 1440, height: 900 },
  { name: "08-settings-access", path: "/?screen=settings&settings=access", theme: "light", width: 1440, height: 900 },
  { name: "09-settings-admin", path: "/?screen=settings&settings=admin", theme: "light", width: 1440, height: 900 },
  { name: "10-home-dark", path: "/", theme: "dark", width: 1440, height: 900 },
  { name: "11-files-dark", path: "/?screen=files", theme: "dark", width: 1440, height: 900 },
  { name: "12-files-large", path: "/?screen=files", theme: "light", width: 1440, height: 900, textsize: "large" },
  { name: "13-files-390", path: "/?screen=files", theme: "light", width: 390, height: 844 },
  { name: "14-home-390", path: "/", theme: "light", width: 390, height: 844 },
  { name: "15-sharing-received-390", path: "/?screen=sharing&tab=received", theme: "light", width: 390, height: 844 },
  { name: "16-files-320", path: "/?screen=files", theme: "light", width: 320, height: 720 },
  { name: "17-files-1024", path: "/?screen=files", theme: "light", width: 1024, height: 768 },
  { name: "18-home-dark-390", path: "/", theme: "dark", width: 390, height: 844 },
];

const browser = await chromium.launch({ executablePath: CHROME || undefined, args: ["--ignore-certificate-errors"] });

try {
  const context = await browser.newContext({
    ignoreHTTPSErrors: true,
    viewport: { width: 1440, height: 900 },
  });
  const page = await context.newPage();

  const consoleErrors = [];
  const failedRequests = [];
  page.on("console", (msg) => {
    if (msg.type() === "error") consoleErrors.push(msg.text());
  });
  page.on("pageerror", (err) => consoleErrors.push(`pageerror: ${err.message}`));
  page.on("requestfailed", (req) => {
    // A share link the seed could not create, or a favicon, is not an interface failure.
    if (req.url().includes("/favicon")) return;
    failedRequests.push(`${req.method()} ${req.url()} — ${req.failure()?.errorText ?? ""}`);
  });

  // Sign in through the demo's real OIDC flow by following the redirect chain.
  //
  // The bounce itself produces 401s: the shell asks `/api/auth/me` before the session exists,
  // which is exactly how an unauthenticated visitor is detected. Counting those as interface
  // errors would make every run fail on its own sign-in, so the console is cleared once the
  // chain finishes and only what happens afterwards is evidence.
  await page.goto(BASE + "/", { waitUntil: "domcontentloaded" });
  if (page.url().includes("/api/auth/login") || page.url().includes("127.0.0.1:19301")) {
    await page.waitForLoadState("networkidle").catch(() => {});
  }
  await page.waitForTimeout(600);
  record("signed in", !page.url().includes("/api/auth/login"), `landed on ${page.url()}`);
  const bounceErrors = consoleErrors.length;
  consoleErrors.length = 0;
  failedRequests.length = 0;
  console.log(`sign-in produced ${bounceErrors} console error(s), ignored as part of the OIDC bounce`);

  // Preferences are per-browser, so they are set through the UI's own storage keys where the
  // keys are stable, and through the appearance sheet otherwise. Setting them directly keeps
  // this script from depending on the sheet's markup.
  const setPrefs = async (theme, textsize) => {
    await page.evaluate(
      ({ theme, textsize }) => {
        if (theme && theme !== "auto") localStorage.setItem("wc.appearance.theme", theme);
        else localStorage.removeItem("wc.appearance.theme");
        if (textsize && textsize !== "normal") localStorage.setItem("wc.appearance.textsize", textsize);
        else localStorage.removeItem("wc.appearance.textsize");
      },
      { theme, textsize },
    );
  };

  for (const shot of SHOTS) {
    if (ONLY && !shot.name.includes(ONLY)) continue;
    await page.setViewportSize({ width: shot.width, height: shot.height });
    await setPrefs(shot.theme, shot.textsize);
    await page.goto(BASE + shot.path, { waitUntil: "networkidle" });
    await page.waitForTimeout(350);

    // A page that scrolls sideways is broken at that width, whatever it looks like.
    const overflow = await page.evaluate(() => {
      const doc = document.documentElement;
      return { scroll: doc.scrollWidth, client: doc.clientWidth };
    });
    record(
      `${shot.name} fits ${shot.width}px`,
      overflow.scroll <= overflow.client + 1,
      `scrollWidth ${overflow.scroll} vs clientWidth ${overflow.client}`,
    );

    // The appearance attribute has to actually be on the root, or a screenshot labelled "dark"
    // is a screenshot of the light appearance.
    const applied = await page.evaluate(() => document.body.dataset.theme ?? "auto");
    record(
      `${shot.name} appearance`,
      (shot.theme ?? "auto") === applied,
      `expected ${shot.theme}, root has ${applied}`,
    );

    await page.screenshot({ path: resolve(OUT, `${shot.name}.png`), fullPage: false });
  }

  /* ---- interaction checks, in the same browser ---- */

  if (!ONLY) {
    await page.setViewportSize({ width: 1440, height: 900 });
    await setPrefs("light", "normal");

    // 1. Files: one listing, folders first, a row per item.
    await page.goto(BASE + "/?screen=files", { waitUntil: "networkidle" });
    const rows = await page.locator(".serein-file-row").count();
    record("files lists rows", rows > 0, `${rows} rows`);
    const folderFirst = await page.evaluate(() => {
      const all = [...document.querySelectorAll(".serein-file-row")];
      const firstFile = all.findIndex((r) => r.getAttribute("data-folder") !== "true");
      const lastFolder = all.map((r) => r.getAttribute("data-folder") === "true").lastIndexOf(true);
      return { firstFile, lastFolder };
    });
    record(
      "folders precede files",
      folderFirst.lastFolder < folderFirst.firstFile || folderFirst.firstFile === -1,
      `last folder ${folderFirst.lastFolder}, first file ${folderFirst.firstFile}`,
    );

    // 2. Keyboard traversal: the listbox takes focus and ArrowDown moves the active row.
    const list = page.locator(".serein-filelist__body");
    await list.focus();
    await page.keyboard.press("ArrowDown");
    await page.keyboard.press("ArrowDown");
    const active = await list.getAttribute("aria-activedescendant");
    record("arrow keys move the active row", Boolean(active), `aria-activedescendant = ${active}`);

    // 3. Breadcrumb: an entry into a folder produces a path, and the crumb returns.
    const folderRow = page.locator('.serein-file-row[data-folder="true"]').first();
    if ((await folderRow.count()) > 0) {
      const folderName = (await folderRow.locator(".serein-file-row__title").innerText()).trim();
      await folderRow.locator(".serein-file-row__title").click();
      await page.waitForTimeout(600);
      const url = page.url();
      record("entering a folder is in the URL", url.includes("space="), url);
      // The folder you are in is the page TITLE; the trail above it holds ancestors only, so
      // the title is what has to name the folder and the crumb has to be the way back.
      const heading = (await page.locator(".serein-page-header__title").innerText()).trim();
      record("the page title names the folder", heading === folderName, `title "${heading}" vs "${folderName}"`);
      const rootCrumb = page.locator(".serein-breadcrumb__link").first();
      record("a parent crumb exists", (await rootCrumb.count()) > 0, "breadcrumb link present");
      await rootCrumb.click();
      await page.waitForTimeout(500);
      record("the crumb returns to the root", !page.url().includes("space="), page.url());
      const noTrailAtRoot = (await page.locator(".wc-crumbs").count()) === 0;
      record("the root has no breadcrumb", noTrailAtRoot, "trail hidden where there is no ancestor");
    } else {
      record("breadcrumb", false, "no folder row to open");
    }

    // 4. Search: Ctrl+K opens the palette, typing a name finds it, Escape closes it.
    //
    // The query is taken from a file that actually exists in the listing rather than a fixed
    // string: the demo's seed decides the filenames, and a script that hard-coded one would
    // report "search is broken" for a fixture change.
    await page.goto(BASE + "/?screen=files", { waitUntil: "networkidle" });
    const knownName = await page.evaluate(() => {
      const row = document.querySelector('.serein-file-row:not([data-folder="true"]) .serein-file-row__title');
      return row ? row.textContent.trim() : "";
    });
    const stem = knownName.replace(/\.[^.]+$/, "").slice(0, 4);
    await page.keyboard.press("Control+k");
    await page.waitForTimeout(250);
    record("Ctrl+K opens search", await page.locator(".wc-search").isVisible(), "search dialog visible");
    await page.keyboard.type(stem || "a");
    await page.waitForTimeout(700);
    const searchItems = await page.locator(".wc-search__item").count();
    record("search returns results", searchItems > 0, `${searchItems} items for "${stem}"`);
    await page.keyboard.press("Escape");
    await page.waitForTimeout(200);
    record("Escape closes search", (await page.locator(".wc-search").count()) === 0, "dialog removed");

    // 5. Viewer: opening a file is a URL, the footer counts the collection, Escape returns.
    await page.goto(BASE + "/?screen=files", { waitUntil: "networkidle" });
    const fileRow = page.locator(".serein-file-row").filter({ hasNot: page.locator("[data-folder]") }).first();
    const anyFile = page.locator('.serein-file-row:not([data-folder="true"])').first();
    const target = (await anyFile.count()) > 0 ? anyFile : fileRow;
    if ((await target.count()) > 0) {
      await target.locator(".serein-file-row__title").click();
      await page.waitForTimeout(900);
      const vurl = page.url();
      record("opening a file is in the URL", vurl.includes("screen=viewer") && vurl.includes("from="), vurl);
      const viewerVisible = await page.locator(".serein-viewer").isVisible();
      record("the viewer renders", viewerVisible, "viewer visible");
      const footerLabel = await page.locator(".serein-viewer__footer-meta strong").innerText().catch(() => "");
      record("the footer names the collection", footerLabel.trim().length > 0, `label "${footerLabel}"`);
      await page.screenshot({ path: resolve(OUT, "19-viewer.png") });

      await page.keyboard.press("Escape");
      await page.waitForTimeout(600);
      record("Escape leaves the viewer", !page.url().includes("screen=viewer"), page.url());
    } else {
      record("viewer", false, "no file row to open");
    }

    // 6. Back button: opening a folder then pressing Back returns to the previous location.
    await page.goto(BASE + "/?screen=files", { waitUntil: "networkidle" });
    const before = page.url();
    const row2 = page.locator('.serein-file-row[data-folder="true"]').first();
    if ((await row2.count()) > 0) {
      await row2.locator(".serein-file-row__title").click();
      await page.waitForTimeout(500);
      await page.goBack();
      await page.waitForTimeout(500);
      record("browser Back leaves the folder", page.url() === before, `${page.url()} vs ${before}`);
    }

    // 7. Sharing tabs are in the URL and are real tabs.
    await page.goto(BASE + "/?screen=sharing", { waitUntil: "networkidle" });
    const receivedSelected = await page
      .locator('#share-tab-received[aria-selected="true"]')
      .count();
    record("received is the default sharing view", receivedSelected === 1, "aria-selected on received");
    await page.locator("#share-tab-managed").click();
    await page.waitForTimeout(400);
    record("switching sharing tab is in the URL", page.url().includes("tab=managed"), page.url());

    // 8. The inspector opens and closes without replacing the page.
    await page.goto(BASE + "/?screen=files&inspector=1", { waitUntil: "networkidle" });
    record("inspector renders when asked", (await page.locator(".serein-inspector").count()) === 1, "one inspector");
    const paneTabs = await page.locator(".serein-inspector .serein-tab").count();
    record("inspector has its panes", paneTabs >= 4, `${paneTabs} tabs`);

    // 9. Touch targets on a phone: every visible control at least 44px in both axes.
    //
    // "Visible" means laid out *and* on the page, not merely not `display:none`: an unopened
    // `details` menu still reports a size for its contents in some layouts, and counting those
    // would report a menu that is not on screen as a failing control.
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto(BASE + "/?screen=files", { waitUntil: "networkidle" });
    const small = await page.evaluate(() => {
      const out = [];
      const shown = (el) => {
        // Walk to the root: a `details` whose content is closed, or an ancestor with
        // `content-visibility:hidden`, is not a control a finger can reach.
        for (let node = el; node && node !== document.body; node = node.parentElement) {
          if (node instanceof HTMLDetailsElement && node.open === false && node !== el) return false;
          const s = getComputedStyle(node);
          if (s.display === "none" || s.visibility === "hidden" || s.visibility === "collapse") return false;
          if (node.hasAttribute("hidden")) return false;
        }
        const r = el.getBoundingClientRect();
        return r.width > 0 && r.height > 0;
      };
      for (const el of document.querySelectorAll("button, a[href], select, summary, [role=tab], [role=menuitem]")) {
        if (!shown(el)) continue;
        const r = el.getBoundingClientRect();
        const cls = (el.className || "").toString();
        // The row's filename is a button for keyboard reachability and the skip link is a
        // document-level affordance; 2.5.8 exempts both. Everything else must be 44px.
        if (cls.includes("serein-file-row__title") || cls.includes("serein-skip-link")) continue;
        if (el.tagName === "A" && !/btn|icon|crumb/.test(cls)) continue;
        if (r.width < 44 || r.height < 44) {
          out.push(`${el.tagName}.${cls.split(" ")[0]} ${Math.round(r.width)}x${Math.round(r.height)}`);
        }
      }
      return out;
    });
    record("phone controls are at least 44px", small.length === 0, small.slice(0, 8).join(", "));

    // 10. No unlabelled interactive control anywhere in the shell.
    const unlabelled = await page.evaluate(() => {
      const out = [];
      const shown = (el) => {
        for (let node = el; node && node !== document.body; node = node.parentElement) {
          if (node instanceof HTMLDetailsElement && node.open === false && node !== el) return false;
          const s = getComputedStyle(node);
          if (s.display === "none" || s.visibility === "hidden") return false;
        }
        return true;
      };
      for (const el of document.querySelectorAll("button, [role=button], [role=tab], [role=menuitem]")) {
        if (!shown(el)) continue;
        const text = (el.innerText || "").trim();
        const label = el.getAttribute("aria-label") || el.getAttribute("title") || "";
        const labelledBy = el.getAttribute("aria-labelledby");
        if (!text && !label && !labelledBy) {
          out.push(`${el.tagName}.${(el.className || "").toString().split(" ")[0]}`);
        }
      }
      return [...new Set(out)];
    });
    record("every control has a name", unlabelled.length === 0, unlabelled.slice(0, 8).join(", "));

    // 11. The share landing page renders outside the shell, with no session.
    const anon = await browser.newContext({ ignoreHTTPSErrors: true });
    const anonPage = await anon.newPage();
    await anonPage.goto(BASE + "/s/definitely-not-a-token", { waitUntil: "networkidle" });
    const landingText = await anonPage.locator("body").innerText();
    record(
      "an unknown share shows an unavailable page",
      /not available|不可用/i.test(landingText),
      landingText.slice(0, 80).replace(/\s+/g, " "),
    );
    const hasShell = await anonPage.locator(".serein-shell").count();
    record("the recipient page has no application shell", hasShell === 0, `${hasShell} shells`);
    await anonPage.screenshot({ path: resolve(OUT, "20-share-unavailable.png") });

    // 11b. A share that CAN be opened: the gate must disclose nothing. The token comes from
    // the run's own console output; without it the check is skipped rather than faked.
    const shareToken = arg("share", process.env.SHARE_TOKEN ?? "");
    if (shareToken) {
      await anonPage.goto(`${BASE}/s/${shareToken}`, { waitUntil: "networkidle" });
      const gateText = await anonPage.locator("body").innerText();
      const discloses = /notes\.md|Welcome\.txt|Projects|item/i.test(gateText);
      record("the password gate discloses nothing", !discloses, gateText.slice(0, 90).replace(/\s+/g, " "));
      record(
        "the gate makes no attempt-count claim",
        !/attempt|remaining|left/i.test(gateText),
        "no attempt budget printed",
      );
      await anonPage.screenshot({ path: resolve(OUT, "21-share-gate.png") });
    }
    await anon.close();

    // 12. The account menu and appearance sheet are real overlays with real effects.
    await page.setViewportSize({ width: 1440, height: 900 });
    await page.goto(BASE + "/", { waitUntil: "networkidle" });
    await page.locator(".wc-accountbtn").click();
    await page.waitForTimeout(250);
    record("the account menu opens", (await page.locator(".wc-accountmenu").count()) === 1, "menu present");
    await page.screenshot({ path: resolve(OUT, "22-account-menu.png") });
    await page.getByRole("menuitem", { name: /appearance|外观/i }).click();
    await page.waitForTimeout(350);
    record("the appearance sheet opens", (await page.locator(".serein-sheet").count()) === 1, "sheet present");
    await page.screenshot({ path: resolve(OUT, "23-appearance.png") });
    const darkBtn = page.locator(".serein-sheet .serein-seg__btn").filter({ hasText: /^Dark$|^暗色$/ });
    if ((await darkBtn.count()) > 0) {
      await darkBtn.first().click();
      await page.waitForTimeout(250);
      const themeNow = await page.evaluate(() => document.body.dataset.theme ?? "auto");
      record("the appearance choice applies", themeNow === "dark", `root theme = ${themeNow}`);
      await page.screenshot({ path: resolve(OUT, "24-appearance-dark.png") });
      await setPrefs("auto", "normal");
    }
    await page.keyboard.press("Escape");
    await page.waitForTimeout(200);

    // 13. The command palette lists commands as well as files.
    await page.keyboard.press("Control+k");
    await page.waitForTimeout(250);
    const groups = await page.evaluate(() =>
      [...document.querySelectorAll(".wc-search__group")].map((n) => n.textContent.trim()),
    );
    record("the palette groups its results", groups.length >= 2, groups.join(", "));
    await page.screenshot({ path: resolve(OUT, "25-search.png") });
    await page.keyboard.press("Escape");

    // 14. A CSV opens as a real table, not as raw commas.
    //
    // The name is DISCOVERED rather than hardcoded, because the two demo binaries seed different
    // sample sets (tools/demoinmem ships `data.csv`, tools/demo ships `数据.csv`). A hardcoded name
    // made this check silently inapplicable on the PostgreSQL demo — it reported "0 rows x 0 cols"
    // for a viewer that was never opened, which reads as a broken table rather than a broken check.
    await page.goto(BASE + "/?screen=files", { waitUntil: "networkidle" });
    await page.waitForTimeout(400);
    const csvName = await page.evaluate(() => {
      const row = [...document.querySelectorAll(".serein-file-row")].find((r) =>
        (r.textContent ?? "").toLowerCase().includes(".csv"),
      );
      return row?.querySelector(".serein-file-row__title")?.textContent?.trim() ?? "";
    });
    if (csvName) {
      await page.goto(
        BASE + "/?screen=viewer&file=%2F" + encodeURIComponent(csvName) + "&from=folder%3A",
        { waitUntil: "networkidle" },
      );
      await page.waitForTimeout(700);
    }
    const sheet = await page.evaluate(() => {
      const table = document.querySelector(".wc-sheet table");
      if (!table) return { rows: 0, cols: 0 };
      return {
        rows: table.querySelectorAll("tbody tr").length,
        cols: table.querySelectorAll("tbody tr:first-child td").length,
      };
    });
    record(
      "a CSV renders as a table",
      sheet.rows > 1 && sheet.cols > 1,
      csvName ? `${csvName}: ${sheet.rows} rows × ${sheet.cols} cols` : "no CSV in the seed",
    );
    await page.screenshot({ path: resolve(OUT, "26-viewer-csv.png") });

    // 15. Selecting a row fills the inspector, and Versions never offers an impossible restore.
    //
    // The click is on the row's metadata cell, not on the filename: the filename is a button
    // that opens the viewer (that is its job, checked in step 5), while a click anywhere else
    // on the row is a selection. Both behaviours are deliberate and this asserts the second.
    await page.goto(BASE + "/?screen=files&inspector=1", { waitUntil: "networkidle" });
    const firstRow = page.locator('.serein-file-row:not([data-folder="true"])').first();
    if ((await firstRow.count()) > 0) {
      await firstRow.locator(".serein-file-row__size").click();
      await page.waitForTimeout(600);
      const inspectorName = await page.locator(".serein-inspector__title").innerText();
      record(
        "selecting a row fills the inspector",
        inspectorName.trim().length > 0,
        `inspector shows "${inspectorName.trim()}"`,
      );
      await page.screenshot({ path: resolve(OUT, "27-inspector-selected.png") });
      await page.locator("#insp-tab-versions").click();
      await page.waitForTimeout(600);
      const restoreState = await page.evaluate(() => {
        const btn = document.querySelector(".serein-version .serein-btn");
        if (!btn) return "no versions listed";
        return btn.disabled ? "restore disabled with a reason" : "restore enabled";
      });
      record("versions never offer an impossible restore", restoreState !== "restore enabled", restoreState);
    }

    // 16. The seven core-scope functions, driven the way a person drives them.
    //
    // These are the only checks in this script that MUTATE: they star a file, label it, discuss it,
    // trash it and empty the bin. That is deliberate — the social surfaces have no read-only state
    // worth inspecting, and a check that only looked at the empty case would pass against a server
    // that refuses every write. The demo instance is throwaway, so the destruction is free.
    //
    // Each step reads its result back from the SERVER through the UI (a reload, or a fresh query),
    // never from the component's own state: an optimistic update the server rejected is exactly the
    // bug this is built to catch.
    await page.setViewportSize({ width: 1440, height: 900 });
    await page.goto(BASE + "/?screen=files&inspector=1", { waitUntil: "networkidle" });

    const targetRow = page.locator('.serein-file-row:not([data-folder="true"])').first();
    if ((await targetRow.count()) > 0) {
      await targetRow.locator(".serein-file-row__size").click();
      await page.waitForTimeout(500);
      const targetName = (await page.locator(".serein-inspector__name").innerText()).trim();

      // §4 — star, then reload and require the star to still be there.
      //
      // The selection is component state rather than a URL parameter, so a reload empties the
      // inspector; the row is re-selected before the star is read. That is the honest form of this
      // check anyway — it asserts what the SERVER holds, not what the panel remembered.
      const starBtn = page.locator(".serein-inspector__actions .wc-star").first();
      await starBtn.click();
      await page.waitForTimeout(600);
      const starPressed = await starBtn.getAttribute("aria-pressed");
      await page.reload({ waitUntil: "networkidle" });
      await page.waitForTimeout(600);
      await page
        .locator('.serein-file-row:not([data-folder="true"])')
        .first()
        .locator(".serein-file-row__size")
        .click();
      await page.waitForTimeout(500);
      const starAfterReload = await page
        .locator(".serein-inspector__actions .wc-star")
        .first()
        .getAttribute("aria-pressed");
      record(
        "a star survives a reload",
        starPressed === "true" && starAfterReload === "true",
        `${targetName}: pressed=${starPressed}, after reload=${starAfterReload}`,
      );

      // The favorites filter is the other half of §4: a star with no surface is a bit nobody sees.
      await page.goto(BASE + "/?screen=files&starred=1", { waitUntil: "networkidle" });
      await page.waitForTimeout(700);
      const starredRows = await page.locator(".serein-file-row").count();
      record("the starred filter lists the star", starredRows >= 1, `${starredRows} starred row(s)`);

      // §5 — create a label, apply it, and require the chip to appear.
      await page.goto(BASE + "/?screen=files&inspector=1", { waitUntil: "networkidle" });
      await page
        .locator('.serein-file-row:not([data-folder="true"])')
        .first()
        .locator(".serein-file-row__size")
        .click();
      await page.waitForTimeout(500);
      const label = `verify-${Date.now().toString(36)}`;
      await page.locator(".wc-tageditor__add").first().click();
      await page.waitForTimeout(300);
      await page.locator(".wc-tageditor__new input").fill(label);
      await page.locator(".wc-tageditor__new button[type=submit]").click();
      await page.waitForTimeout(800);
      const chips = await page.locator(".serein-inspector .wc-tagchip").allInnerTexts();
      record(
        "a label can be created and applied",
        chips.some((c) => c.includes(label)),
        `chips: ${chips.join(", ") || "none"}`,
      );

      // The chip has to reach the ROW too, or the listing does not show what a file is labelled.
      // The row shows at most three and counts the rest: a row with nine chips is a row whose file
      // name is invisible, so the truncation is the contract and "+N" is how it says so.
      await page.reload({ waitUntil: "networkidle" });
      await page.waitForTimeout(700);
      const rowChips = await page.locator(".serein-file-row .wc-tagchip").allInnerTexts();
      const counted = rowChips.filter((c) => /^\+\d+$/.test(c.trim()));
      record(
        "a label reaches the row, truncated with a count",
        rowChips.length > 0 && (rowChips.length <= 3 || counted.length > 0),
        `row chips: ${rowChips.join(", ") || "none"}`,
      );

      // §6 — post a comment, and require it to come back from the server.
      //
      // Same re-selection as above: the row must be picked before a pane can render anything about
      // it, and picking it is what a person does after a reload too.
      await page
        .locator('.serein-file-row:not([data-folder="true"])')
        .first()
        .locator(".serein-file-row__size")
        .click();
      await page.waitForTimeout(500);
      await page.locator("#insp-tab-comments").click();
      await page.waitForTimeout(700);
      const body = `verified at ${new Date().toISOString()}`;
      await page.locator(".wc-comments__input").fill(body);
      await page.locator(".wc-comments__compose-actions button[type=submit]").click();
      await page.waitForTimeout(900);
      const thread = await page.locator(".wc-comment__text").allInnerTexts();
      record(
        "a comment is posted and read back",
        thread.some((c) => c.includes(body)),
        `thread: ${thread.join(" | ") || "empty"}`,
      );
      await page.screenshot({ path: resolve(OUT, "28-inspector-comments.png") });

      // §2 — the checksum is shown in FULL, not truncated: its only use is comparison. It lives in
      // the Info pane, so the pane is switched back before it is read.
      await page.locator("#insp-tab-info").click();
      await page.waitForTimeout(600);
      const hash = await page
        .locator(".wc-checksum__value")
        .innerText()
        .catch(() => "");
      record(
        "the inspector shows the content address",
        /^[0-9a-f]{64}$/.test(hash.trim()),
        hash ? `${hash.trim().slice(0, 16)}… (${hash.trim().length} chars)` : "not shown",
      );

      // §3 — trash the file, then empty the bin and require it to be gone.
      await page.locator(".serein-inspector__actions .serein-menu summary").click();
      await page.waitForTimeout(200);
      await page.locator(".serein-inspector__actions .serein-menu__item--danger").click();
      await page.waitForTimeout(400);
      await page
        .locator(".serein-dialog .serein-btn--danger")
        .last()
        .click();
      await page.waitForTimeout(900);

      await page.goto(BASE + "/?screen=settings&settings=trash", { waitUntil: "networkidle" });
      await page.waitForTimeout(600);
      const trashBefore = await page.locator(".wc-list__row").count();
      const emptyBtn = page.locator(".wc-trash-actions > .serein-btn").first();
      if ((await emptyBtn.count()) > 0) {
        await emptyBtn.click();
        await page.waitForTimeout(300);
        await page.locator(".wc-trash-actions__confirm .serein-btn--danger").click();
        await page.waitForTimeout(1600);
        const trashAfter = await page.locator(".wc-list__row").count();
        record(
          "emptying the trash removes every entry",
          trashAfter === 0,
          `${trashBefore} before, ${trashAfter} after`,
        );
        await page.screenshot({ path: resolve(OUT, "29-trash-emptied.png") });
      } else {
        record("emptying the trash removes every entry", trashBefore === 0, `${trashBefore} in the bin`);
      }
    }

    // 17. The fixed phone tab bar never covers the end of a page.
    //
    // Serein fixes the tab bar to the bottom edge and the page reserves its height; reserving
    // too little hides the last row's controls behind it. This is measured at maximum scroll on
    // every surface, ignoring content inside closed `details` (which reports geometry without
    // being on screen).
    await page.setViewportSize({ width: 390, height: 844 });
    for (const path of ["/", "/?screen=files", "/?screen=sharing", "/?screen=settings&settings=access"]) {
      await page.goto(BASE + path, { waitUntil: "networkidle" });
      await page.waitForTimeout(350);
      await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
      await page.waitForTimeout(200);
      const covered = await page.evaluate(() => {
        const bar = document.querySelector(".serein-tabbar");
        if (!bar) return { ok: true, note: "no tab bar at this width" };
        const barTop = bar.getBoundingClientRect().top;
        const shown = (el) => {
          for (let node = el; node && node !== document.body; node = node.parentElement) {
            if (node instanceof HTMLDetailsElement && node.open === false && node !== el) return false;
            if (node.hasAttribute("hidden")) return false;
            const s = getComputedStyle(node);
            if (s.display === "none" || s.visibility === "hidden") return false;
          }
          return true;
        };
        const all = [...document.querySelectorAll(".serein-page *")].filter((el) => {
          const r = el.getBoundingClientRect();
          return r.width > 0 && r.height > 0 && shown(el);
        });
        const last = all[all.length - 1];
        if (!last) return { ok: true, note: "empty page" };
        const bottom = last.getBoundingClientRect().bottom;
        return {
          ok: bottom <= barTop + 1,
          note: `last content bottom ${Math.round(bottom)}, tab bar top ${Math.round(barTop)}`,
        };
      });
      record(`the tab bar does not cover ${path}`, covered.ok, covered.note);
    }
    await page.setViewportSize({ width: 1440, height: 900 });

    record("no console errors", consoleErrors.length === 0, consoleErrors.slice(0, 5).join(" | "));
    record("no failed requests", failedRequests.length === 0, failedRequests.slice(0, 5).join(" | "));
  }
} finally {
  await browser.close();
}

const passed = results.filter((r) => r.ok).length;
writeFileSync(
  resolve(OUT, "results.json"),
  JSON.stringify({ base: BASE, at: new Date().toISOString(), passed, total: results.length, results }, null, 2),
);

for (const r of results) {
  console.log(`${r.ok ? "ok  " : "FAIL"} ${r.name}${r.detail ? ` — ${r.detail}` : ""}`);
}
console.log(`\n${passed}/${results.length} checks passed; screenshots in ${OUT}`);

if (problems.length > 0) {
  console.error(`\n${problems.length} problem(s):\n  ${problems.join("\n  ")}`);
  process.exit(1);
}
