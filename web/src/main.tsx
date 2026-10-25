// Entry point.
//
// `applyPrefs` runs before React mounts so an explicit appearance choice is on the element
// before the first paint. The common case — "follow the system" — is already correct in the
// stylesheet without this, because the token layer resolves it from `prefers-color-scheme` when
// no attribute is present. Running it here cannot flash either way: module scripts execute
// after the stylesheets are parsed and before the first frame.
//
// The `serein-root` class and the theme attributes are on `<body>` in index.html rather than
// added here: the document has to be marked before the stylesheet's `.serein-root` rules can
// match, and a class added from a script arrives one frame late.

import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App";
import { applyPrefs, readPrefs } from "./appearance";
import "./styles.css";

applyPrefs(readPrefs());

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
